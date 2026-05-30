package media

import (
	"context"
	"fmt"
	stdio "io"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"gotgcall/models"
)

// frameReader is the internal interface the Streamer pulls from. It
// parses a byte stream (ogg or ivf) and yields one media.Sample per
// call. Closing it must close the underlying byte stream.
type frameReader interface {
	Next(ctx context.Context) (media.Sample, error)
	Close() error
}

// --- Opus -----------------------------------------------------------------

func newOpusFrameReader(r stdio.Reader) (frameReader, error) {
	ogg, _, err := oggreader.NewWith(r)
	if err != nil {
		if c, ok := r.(stdio.Closer); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("%w: ogg parse: %v", models.ErrFile, err)
	}
	return &opusFrameReader{src: r, ogg: ogg}, nil
}

type opusFrameReader struct {
	src           stdio.Reader
	ogg           *oggreader.OggReader
	skippedHeader bool
	skippedTags   bool
}

func (o *opusFrameReader) Next(ctx context.Context) (media.Sample, error) {
	for {
		if err := ctx.Err(); err != nil {
			return media.Sample{}, err
		}
		payload, _, err := o.ogg.ParseNextPage()
		if err != nil {
			return media.Sample{}, err
		}
		if len(payload) >= 8 {
			head := string(payload[:8])
			if !o.skippedHeader && head == "OpusHead" {
				o.skippedHeader = true
				continue
			}
			if !o.skippedTags && head == "OpusTags" {
				o.skippedTags = true
				continue
			}
		}
		o.skippedHeader, o.skippedTags = true, true
		return media.Sample{
			Data:     payload,
			Duration: time.Duration(models.OpusFrameDurationMs) * time.Millisecond,
		}, nil
	}
}

func (o *opusFrameReader) Close() error {
	if c, ok := o.src.(stdio.Closer); ok {
		return c.Close()
	}
	return nil
}

// --- VP8 ------------------------------------------------------------------

func newVP8FrameReader(r stdio.Reader, fps int) (frameReader, error) {
	if fps <= 0 {
		fps = 30
	}
	ivf, _, err := ivfreader.NewWith(r)
	if err != nil {
		if c, ok := r.(stdio.Closer); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("%w: ivf parse: %v", models.ErrFile, err)
	}
	return &vp8FrameReader{src: r, ivf: ivf, frameDur: time.Second / time.Duration(fps)}, nil
}

type vp8FrameReader struct {
	src      stdio.Reader
	ivf      *ivfreader.IVFReader
	frameDur time.Duration
}

func (v *vp8FrameReader) Next(ctx context.Context) (media.Sample, error) {
	if err := ctx.Err(); err != nil {
		return media.Sample{}, err
	}
	payload, _, err := v.ivf.ParseNextFrame()
	if err != nil {
		return media.Sample{}, err
	}
	return media.Sample{Data: payload, Duration: v.frameDur}, nil
}

func (v *vp8FrameReader) Close() error {
	if c, ok := v.src.(stdio.Closer); ok {
		return c.Close()
	}
	return nil
}

// NewOpusFrameReader exposes the internal opus reader for callers that
// have a raw ogg byte stream (e.g. instances/group_call).
func NewOpusFrameReader(r stdio.Reader) (FrameReader, error) {
	fr, err := newOpusFrameReader(r)
	return fr, err
}

// NewVP8FrameReader exposes the internal vp8 reader.
func NewVP8FrameReader(r stdio.Reader, fps int) (FrameReader, error) {
	fr, err := newVP8FrameReader(r, fps)
	return fr, err
}

// FrameReader is the exported alias for the internal frameReader so
// other packages in the module can hold one.
type FrameReader = frameReader
