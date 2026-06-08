package wrtc

import (
	"context"
	"log/slog"
	"sync"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/wrtc/native"
)

// PeerConnection wraps a native.Stack with the legacy method surface
// callers used during the pion/webrtc PeerConnection era. The wrapper
// owns no per-call goroutines beyond what the Stack already runs.
type PeerConnection struct {
	stack   *native.Stack
	log     *slog.Logger
	monitor *FactoryMonitor

	onStateChange func(models.ConnState)

	mu sync.RWMutex
}

// NewPeerConnection constructs a PeerConnection by taking a Stack from
// the factory's pool of native components.
func NewPeerConnection(f *Factory, log *slog.Logger) (*PeerConnection, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stack, err := f.newStack(context.Background())
	if err != nil {
		return nil, err
	}
	p := &PeerConnection{stack: stack, log: log, monitor: f.Monitor()}
	stack.OnConnectionStateChange(p.forwardState)
	p.monitor.Register(p)
	return p, nil
}

// LocalParams returns the JoinGroupCall blob — credentials, fingerprint,
// SSRCs, codec/extension manifest.
func (p *PeerConnection) LocalParams() (string, error) {
	return p.stack.LocalParams()
}

// Connect applies Telegram's response, runs ICE+DTLS+SRTP setup, and
// returns once the connection is ready to carry media.
func (p *PeerConnection) Connect(remoteJSON string) error {
	return p.stack.Connect(context.Background(), remoteJSON)
}

// AudioTrack returns the audio sink the streamer pumps Opus samples
// into. Always non-nil for normal calls.
func (p *PeerConnection) AudioTrack() *native.Track { return p.stack.AudioTrack() }

// VideoTrack returns the video sink (VP8), or nil for audio-only calls.
func (p *PeerConnection) VideoTrack() *native.Track { return p.stack.VideoTrack() }

// AudioSSRC reports the audio SSRC Telegram associates with this participant.
func (p *PeerConnection) AudioSSRC() uint32 { return p.stack.AudioSSRC() }

// VideoSSRC reports the video SSRC announced in the FID group of LocalParams.
func (p *PeerConnection) VideoSSRC() uint32 { return p.stack.VideoSSRC() }

// OnConnectionStateChange registers fn for state transitions. The
// callback may fire from the underlying native.Stack's ICE goroutine —
// fn must not block. Replaces any previous handler.
func (p *PeerConnection) OnConnectionStateChange(fn func(models.ConnState)) {
	p.mu.Lock()
	p.onStateChange = fn
	p.mu.Unlock()
}

// State exposes the Stack's current connection state for the monitor.
func (p *PeerConnection) State() models.ConnState { return p.stack.State() }

// Close tears down the underlying Stack and unregisters from the
// Factory monitor. Idempotent — the stack uses sync.Once and the
// monitor's map delete is a no-op for absent entries, so multiple
// callers (user code + the monitor's force-close path) are safe.
func (p *PeerConnection) Close() error {
	p.monitor.Unregister(p)
	return p.stack.Close()
}

func (p *PeerConnection) forwardState(s models.ConnState) {
	p.mu.RLock()
	fn := p.onStateChange
	p.mu.RUnlock()
	if fn != nil {
		fn(s)
	}
}
