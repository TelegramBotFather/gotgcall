package media

import (
	"context"
	"fmt"
	stdio "io"
	"strings"

	gtio "github.com/annihilatorrrr/gotgcall/io"
	"github.com/annihilatorrrr/gotgcall/models"
)

// FromShell parses cmdline as a shell command (handling double-quoted
// arguments) and spawns it directly via exec — NOT via /bin/sh, so shell
// metacharacters in filenames cannot inject commands. The command MUST
// produce Opus-in-OGG on stdout for audio:
//
//	ffmpeg -i <file> -c:a libopus -application audio -frame_duration 20 \
//	       -b:a 64k -ar 48000 -ac 2 -f ogg pipe:1
//
// or VP8-in-IVF on stdout for video:
//
//	ffmpeg -i <file> -c:v libvpx -deadline realtime -b:v 800k -f ivf pipe:1
//
// The first token is the binary; everything after is treated as args.
// Use track to declare which stream the command emits. Args are
// pre-flight-checked for known-bad codec combinations (e.g. pcm_s16le on
// an audio track) and rejected early with a clear error.
func FromShell(cmdline string, track Track) Source {
	tokens := tokenizeShell(cmdline)
	if len(tokens) == 0 {
		return &shellSource{err: fmt.Errorf("%w: empty command", models.ErrFFmpegSpawn), track: track}
	}
	if err := validateOutputCodec(tokens[1:], track); err != nil {
		return &shellSource{err: err, track: track}
	}
	return &shellSource{
		binary: tokens[0],
		args:   tokens[1:],
		track:  track,
	}
}

// FromFFmpegArgs spawns the configured ffmpeg binary (set via the Client's
// WithFFmpegPath option or media.SetFFmpegPath) with the given args. Same
// output-codec contract as FromShell.
func FromFFmpegArgs(args []string, track Track) Source {
	if err := validateOutputCodec(args, track); err != nil {
		return &shellSource{err: err, track: track}
	}
	return &shellSource{binary: "", args: args, track: track}
}

// validateOutputCodec scans argv for combinations known to produce output
// the frame readers can't parse, and returns a user-actionable error.
func validateOutputCodec(args []string, track Track) error {
	rawAudioFmts := map[string]bool{
		"s16le": true, "s16be": true, "s24le": true, "s32le": true,
		"f32le": true, "f64le": true, "u8": true, "alaw": true, "mulaw": true,
	}
	for i, a := range args {
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch a {
		case "-acodec", "-c:a":
			if strings.HasPrefix(next, "pcm_") && track.Has(TrackAudio) {
				return fmt.Errorf("%w: audio output codec %q produces raw PCM; gotgcall expects libopus in OGG. "+
					"Replace your baseCodec with: -c:a libopus -application audio -frame_duration 20 "+
					"-b:a 64k -ar 48000 -ac 2 -f ogg pipe:1", models.ErrInvalidParams, next)
			}
		case "-f":
			if rawAudioFmts[next] && track.Has(TrackAudio) {
				return fmt.Errorf("%w: audio output container %q is raw PCM; gotgcall expects -f ogg with -c:a libopus", models.ErrInvalidParams, next)
			}
			if next == "rawvideo" && track.Has(TrackVideo) {
				return fmt.Errorf("%w: video output container %q is raw YUV; gotgcall expects -f ivf with -c:v libvpx", models.ErrInvalidParams, next)
			}
		case "-vcodec", "-c:v":
			if next == "rawvideo" && track.Has(TrackVideo) {
				return fmt.Errorf("%w: video output codec %q is raw YUV; gotgcall expects -c:v libvpx -f ivf", models.ErrInvalidParams, next)
			}
		}
	}
	return nil
}

type shellSource struct {
	err    error
	binary string // empty = use configured ffmpegPath()
	args   []string
	track  Track
}

func (s *shellSource) Tracks() Track {
	if s.track == 0 {
		return TrackAudio
	}
	return s.track
}

func (s *shellSource) Open(ctx context.Context) (*Streams, error) {
	if s.err != nil {
		return nil, s.err
	}
	bin := s.binary
	if bin == "" {
		bin = ffmpegPath()
	}
	r, err := gtio.NewShellReader(ctx, bin, s.args, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrFFmpegSpawn, err)
	}
	st := &Streams{close: r.Close}
	switch {
	case s.Tracks().Has(TrackAudio):
		st.Audio = r
	case s.Tracks().Has(TrackVideo):
		st.Video = r
	default:
		_ = r.Close()
		return nil, fmt.Errorf("%w: no track selected", models.ErrInvalidParams)
	}
	return st, nil
}

// tokenizeShell splits a shell-like string into argv tokens. Supports
// double-quoted segments with spaces. Single quotes and backslash escapes
// are treated as literal characters (the ffmpeg command strings we expect
// don't use them; keeping this simple avoids surprises). Whitespace
// outside quotes separates tokens.
func tokenizeShell(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t' || c == '\n') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// shellReaderCloser adapts gtio.ShellReader's Close to the close-func
// shape Streams expects. Kept here in case Open wants to wrap; currently
// st.close = r.Close suffices.
var _ stdio.ReadCloser = (*gtio.ShellReader)(nil)
