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
// ivf/VP8 streams from a file or URL input.
type transcodeSource struct {
	path      string   // plain file or URL; set => seekable
	inputArgs []string // ffmpeg args up through "-i <something>"
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
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		// Drop corrupt input packets and ignore decode errors so a bad
		// frame in the source doesn't propagate into the OGG output
		// (which would fail OGG-page CRC validation downstream).
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
	}
	args = append(args, input...)
	args = append(args,
		// "?" makes mapping optional — if the source has no audio stream,
		// ffmpeg exits cleanly instead of failing with "Output file does
		// not contain any stream" (exit 234). startLocked sees an empty
		// reader and skips the audio track.
		"-map", "0:a?",
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
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-re",
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
	}
	args = append(args, input...)
	args = append(args,
		"-map", "0:v?",
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

// FromFile streams any ffmpeg-decodable file (mp4, mkv, webm, mp3, wav, ...).
// Seekable. Pass EncodeOptions{} for defaults.
func FromFile(path string, opt EncodeOptions) Source {
	prefix := ffmpegInputPrefix(path)
	prefix = append(prefix, "-i", path)
	return &transcodeSource{inputArgs: prefix, path: path, opt: opt}
}

// FromURL streams from a URL (http(s), hls/.m3u8, rtmp, ...). Seekable.
// Pass EncodeOptions{} for defaults.
func FromURL(url string, opt EncodeOptions) Source {
	prefix := ffmpegInputPrefix(url)
	prefix = append(prefix, "-i", url)
	return &transcodeSource{inputArgs: prefix, path: url, opt: opt}
}
