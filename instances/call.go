// Package instances holds the per-chat call state. One Call interface,
// two implementations: GroupCall (WebRTC, default) and RTMPCall (FFmpeg
// push to a Telegram-issued RTMP URL).
package instances

import (
	"context"

	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
)

// Call is the per-chat interface the top-level Client multiplexes over.
type Call interface {
	// CreateLocalParams produces the local-side JSON. WebRTC mode only;
	// RTMPCall returns ErrWrongMode.
	CreateLocalParams() (string, error)

	// Connect feeds Telegram's response JSON. WebRTC mode only.
	Connect(remoteJSON string) error

	// SetSource installs the streaming source. Replaces atomically.
	SetSource(ctx context.Context, src media.Source) error

	Pause() (bool, error)
	Resume() (bool, error)
	Mute() (bool, error)
	Unmute() (bool, error)
	Stop() error

	ElapsedMs() uint64
	State() models.MediaState
	NetState() models.ConnState

	// Mode returns either "webrtc" or "rtmp" so the Client can guard
	// mode-specific operations.
	Mode() string
}
