package media

import (
	"context"
	"errors"
	"fmt"
	stdio "io"
	"strconv"
	"strings"
	"time"

	gtio "github.com/annihilatorrrr/gotgcall/io"
	"github.com/annihilatorrrr/gotgcall/models"
)

// ErrNotSeekable is returned by OpenAt when the source has no seekable input.
var ErrNotSeekable = errors.New("media: source is not seekable")

// transcodeSource runs one or two ffmpeg processes to produce ogg/Opus and
// ivf/VP8 streams from an arbitrary input.
type transcodeSource struct {
	stdin     stdio.Reader // wired to ffmpeg stdin (mutually exclusive with two-track)
	path      string       // plain file or URL; set => seekable
	inputArgs []string     // ffmpeg args up through "-i <something>"
	opt       EncodeOptions
}

func (s *transcodeSource) Tracks() Track { return s.opt.withDefaults().Tracks }

// SourcePath is implemented by Sources backed by a plain file path or
// URL. The RTMP transport uses this to feed ffmpeg directly without
// going through the Source.Open() OGG/IVF pipeline.
type SourcePath interface {
	InputPath() string
	InputArgs() []string
	EncodeOpts() EncodeOptions
}

func (s *transcodeSource) InputPath() string         { return s.path }
func (s *transcodeSource) InputArgs() []string       { return s.inputArgs }
func (s *transcodeSource) EncodeOpts() EncodeOptions { return s.opt.withDefaults() }

func (s *transcodeSource) Open(ctx context.Context) (*Streams, error) {
	return s.open(ctx, s.inputArgs)
}

func (s *transcodeSource) OpenAt(ctx context.Context, offset time.Duration) (*Streams, error) {
	if s.path == "" {
		return nil, ErrNotSeekable
	}
	args := append(
		[]string{"-ss", strconv.FormatFloat(offset.Seconds(), 'f', 3, 64)},
		ffmpegInputPrefix(s.path)...,
	)
	args = append(args, "-i", s.path)
	return s.open(ctx, args)
}

func (s *transcodeSource) open(ctx context.Context, input []string) (*Streams, error) {
	o := s.opt.withDefaults()
	if s.stdin != nil && o.Tracks.Has(TrackAudio) && o.Tracks.Has(TrackVideo) {
		return nil, errors.New("media: reader source cannot feed both audio and video; pick one track or use FromFile/FromURL")
	}
	var closers []stdio.Closer
	closeAll := func() error {
		var firstErr error
		for _, c := range closers {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	start := func(args []string) (stdio.Reader, error) {
		// For stdin-fed inputs we use a local wrapper that also pipes stdin;
		// otherwise we use io.ShellReader which gives us a stderr ring buffer.
		if s.stdin != nil {
			r, err := newStdinShellReader(ctx, ffmpegPath(), args, s.stdin)
			if err != nil {
				return nil, err
			}
			closers = append(closers, r)
			return r, nil
		}
		r, err := gtio.NewShellReader(ctx, ffmpegPath(), args, getLogger())
		if err != nil {
			return nil, err
		}
		closers = append(closers, r)
		return r, nil
	}
	st := &Streams{close: closeAll}
	if o.Tracks.Has(TrackAudio) {
		r, err := start(audioFFArgs(input, o))
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("%w: audio ffmpeg: %v", models.ErrFFmpegSpawn, err)
		}
		st.Audio = r
	}
	if o.Tracks.Has(TrackVideo) {
		r, err := start(videoFFArgs(input, o))
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("%w: video ffmpeg: %v", models.ErrFFmpegSpawn, err)
		}
		st.Video = r
	}
	return st, nil
}

func audioFFArgs(input []string, o EncodeOptions) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, input...)
	args = append(args,
		"-vn", "-sn", "-dn",
		"-c:a", "libopus",
		"-b:a", fmt.Sprintf("%dk", o.AudioBitrateKbps),
		"-vbr", "on",
		"-compression_level", "10",
		"-frame_duration", strconv.Itoa(models.OpusFrameDurationMs),
		// Critical: tell the OGG muxer to flush one page per frame_duration
		// (microseconds). Default is 1s, which would batch ~50 Opus frames
		// per OGG page — frame-per-page readers then consume the song at
		// ~50× real-time.
		"-page_duration", strconv.Itoa(models.OpusFrameDurationMs*1000),
		"-application", "audio",
		"-mapping_family", "0",
		"-ar", strconv.Itoa(models.OpusSampleRate),
		"-ac", strconv.Itoa(o.AudioChannels),
		"-f", "ogg",
		"pipe:1",
	)
	return args
}

func videoFFArgs(input []string, o EncodeOptions) []string {
	rate := fmt.Sprintf("%dk", o.VideoBitrateKbps)
	gop := strconv.Itoa(o.VideoFPS * 2)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-re"}
	args = append(args, input...)
	args = append(args,
		"-an", "-sn", "-dn",
		"-c:v", "libvpx",
		"-b:v", rate, "-minrate", rate, "-maxrate", rate, "-bufsize", rate,
		"-deadline", "realtime",
		"-cpu-used", "4",
		"-vf", fmt.Sprintf("scale=%d:%d", o.VideoWidth, o.VideoHeight),
		"-r", strconv.Itoa(o.VideoFPS),
		"-g", gop, "-keyint_min", gop,
		"-auto-alt-ref", "0",
		"-error-resilient", "1",
		"-f", "ivf",
		"pipe:1",
	)
	return args
}

// ffmpegInputPrefix replicates the m3u8/http/ts-specific flag logic from
// the user's prior constructFFmpegInput, expressed as []string.
func ffmpegInputPrefix(input string) []string {
	isM3U8 := strings.Contains(input, ".m3u8")
	isHTTP := strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")
	switch {
	case isM3U8:
		return []string{
			"-user_agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
			"-protocol_whitelist", "file,http,https,tcp,tls",
			"-rw_timeout", "10000000",
			"-http_persistent", "1",
			"-analyzeduration", "0",
			"-probesize", "32k",
		}
	case isHTTP:
		return []string{
			"-reconnect", "1",
			"-reconnect_at_eof", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-timeout", "10000000",
			"-analyzeduration", "0",
			"-probesize", "32k",
		}
	}
	// Local files: ffmpeg's defaults (analyzeduration=5s, probesize=5MB) add
	// ~1-2s of startup latency before the first OGG page is produced. For
	// common containers (mp3/m4a/webm/ogg/mp4) a 64k probe is plenty.
	return []string{
		"-analyzeduration", "0",
		"-probesize", "64k",
	}
}

// --- Constructors -------------------------------------------------------------

func first(opt []EncodeOptions) EncodeOptions {
	if len(opt) > 0 {
		return opt[0]
	}
	return EncodeOptions{}
}

// FromFile streams any ffmpeg-decodable file (mp4, mkv, webm, mp3, wav, ...).
// Seekable.
func FromFile(path string, opt ...EncodeOptions) Source {
	prefix := ffmpegInputPrefix(path)
	prefix = append(prefix, "-i", path)
	return &transcodeSource{inputArgs: prefix, path: path, opt: first(opt)}
}

// FromURL streams from a URL (http(s), hls/.m3u8, rtmp, ...). Seekable.
func FromURL(url string, opt ...EncodeOptions) Source {
	prefix := ffmpegInputPrefix(url)
	prefix = append(prefix, "-i", url)
	return &transcodeSource{inputArgs: prefix, path: url, opt: first(opt)}
}

// FromReader transcodes from an io.Reader via ffmpeg stdin. A single stdin
// pipe cannot feed two encoders; set opt.Tracks to TrackAudio or TrackVideo,
// or switch to FromFile/FromURL.
func FromReader(r stdio.Reader, opt ...EncodeOptions) Source {
	return &transcodeSource{
		inputArgs: []string{"-i", "pipe:0"},
		stdin:     r,
		opt:       first(opt),
	}
}

// FromRawPCM encodes raw PCM audio (no container) into Opus via ffmpeg.
func FromRawPCM(r stdio.Reader, f RawAudioFormat, opt ...EncodeOptions) Source {
	if f.SampleFmt == "" {
		f.SampleFmt = "s16le"
	}
	if f.SampleRate == 0 {
		f.SampleRate = models.OpusSampleRate
	}
	if f.Channels == 0 {
		f.Channels = models.DefaultChannelCount
	}
	o := first(opt)
	o.Tracks = TrackAudio
	return &transcodeSource{
		inputArgs: []string{
			"-f", f.SampleFmt,
			"-ar", strconv.Itoa(f.SampleRate),
			"-ac", strconv.Itoa(f.Channels),
			"-i", "pipe:0",
		},
		stdin: r,
		opt:   o,
	}
}

// FromRawVideo encodes raw video frames (no container) into VP8 via ffmpeg.
func FromRawVideo(r stdio.Reader, f RawVideoFormat, opt ...EncodeOptions) Source {
	if f.PixelFmt == "" {
		f.PixelFmt = "yuv420p"
	}
	if f.FPS == 0 {
		f.FPS = 30
	}
	o := first(opt)
	o.Tracks = TrackVideo
	if f.Width != 0 {
		o.VideoWidth = f.Width
	}
	if f.Height != 0 {
		o.VideoHeight = f.Height
	}
	o.VideoFPS = f.FPS
	return &transcodeSource{
		inputArgs: []string{
			"-f", "rawvideo",
			"-pix_fmt", f.PixelFmt,
			"-s", fmt.Sprintf("%dx%d", f.Width, f.Height),
			"-r", strconv.Itoa(f.FPS),
			"-i", "pipe:0",
		},
		stdin: r,
		opt:   o,
	}
}

// --- Passthrough --------------------------------------------------------------

type passthroughSource struct {
	audio stdio.Reader
	video stdio.Reader
}

func (s *passthroughSource) Tracks() Track {
	var t Track
	if s.audio != nil {
		t |= TrackAudio
	}
	if s.video != nil {
		t |= TrackVideo
	}
	return t
}

func (s *passthroughSource) Open(_ context.Context) (*Streams, error) {
	return &Streams{Audio: s.audio, Video: s.video}, nil
}

// FromOggOpus serves a pre-encoded ogg/Opus stream directly (no ffmpeg).
func FromOggOpus(r stdio.Reader) Source { return &passthroughSource{audio: r} }

// FromIVF serves a pre-encoded IVF/VP8 stream directly (no ffmpeg).
func FromIVF(r stdio.Reader) Source { return &passthroughSource{video: r} }

// FromEncoded serves both a pre-encoded ogg/Opus and IVF/VP8 stream.
func FromEncoded(ogg, ivf stdio.Reader) Source {
	return &passthroughSource{audio: ogg, video: ivf}
}
