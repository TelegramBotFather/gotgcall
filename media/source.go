package media

import (
	"context"
	stdio "io"
	"log/slog"
	"sync/atomic"
	"time"
)

// Track is a bitmask selecting which tracks a Source provides.
type Track int

const (
	TrackAudio Track = 1 << iota
	TrackVideo
)

func (t Track) Has(x Track) bool { return t&x != 0 }

// Streams is the encoded output of a Source: ogg/Opus audio and/or IVF/VP8
// video. A nil reader means that track is absent. Close releases any
// underlying ffmpeg processes and pipes.
type Streams struct {
	Audio stdio.Reader
	Video stdio.Reader
	close func() error
}

func (s *Streams) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

// Source is the public input abstraction. A Source is created lazily;
// Open spawns whatever processes are needed and returns the encoded
// audio+video byte streams.
type Source interface {
	Tracks() Track
	Open(ctx context.Context) (*Streams, error)
}

// SeekableSource is a Source that can begin playback at an offset. Only
// file/URL transcode sources implement it; pre-encoded passthrough sources
// and stdin-fed reader sources do not.
type SeekableSource interface {
	Source
	OpenAt(ctx context.Context, offset time.Duration) (*Streams, error)
}

// EncodeOptions tunes the ffmpeg encode for transcoding sources. Zero
// values become sensible defaults.
type EncodeOptions struct {
	VideoBitrateKbps int
	VideoWidth       int
	VideoHeight      int
	VideoFPS         int
	AudioBitrateKbps int
	AudioChannels    int
	// Tracks limits which tracks to produce. Zero means audio+video.
	Tracks Track
}

func (o EncodeOptions) withDefaults() EncodeOptions {
	if o.VideoBitrateKbps == 0 {
		o.VideoBitrateKbps = 800
	}
	if o.VideoWidth == 0 {
		o.VideoWidth = 1280
	}
	if o.VideoHeight == 0 {
		o.VideoHeight = 720
	}
	if o.VideoFPS == 0 {
		o.VideoFPS = 30
	}
	if o.AudioBitrateKbps == 0 {
		o.AudioBitrateKbps = 64
	}
	if o.AudioChannels == 0 {
		o.AudioChannels = 2
	}
	if o.Tracks == 0 {
		// Audio-only default — both-tracks broke FromFile on audio-only inputs
		// (video ffmpeg exited 234 → IVF parse error). Opt in via opt.Tracks.
		o.Tracks = TrackAudio
	}
	return o
}

// ffmpeg binary path; configurable via SetFFmpegPath. Default "ffmpeg".
var ffmpegBinary atomic.Value

func init() { ffmpegBinary.Store("ffmpeg") }

// SetFFmpegPath overrides the binary used for transcoding. Empty resets to "ffmpeg".
func SetFFmpegPath(p string) {
	if p == "" {
		p = "ffmpeg"
	}
	ffmpegBinary.Store(p)
}

func ffmpegPath() string {
	if v := ffmpegBinary.Load(); v != nil {
		return v.(string)
	}
	return "ffmpeg"
}

// Package-level logger used by ffmpeg-spawning sources for stderr/exit
// diagnostics. Defaults to discard; SetLogger plumbs the Client's logger in.
var mediaLogger atomic.Pointer[slog.Logger]

// SetLogger sets the logger used by media-package ffmpeg shell readers.
// Pass nil to disable logging.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	mediaLogger.Store(l)
}

func getLogger() *slog.Logger {
	if l := mediaLogger.Load(); l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}
