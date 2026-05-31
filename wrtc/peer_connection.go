package wrtc

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/wrtc/jsonparams"
)

// connStateFn is the user-side state-change delegate. Stored under
// PeerConnection.onStateChange and invoked by handleStateChange. Per-PC
// keepalive + liveness lifecycle is owned by the Factory-shared
// FactoryMonitor (1 goroutine for all PCs), so handleStateChange itself
// is now a thin forwarder.
type connStateFn func(models.ConnState)

// defaultSTUNServers gives pion at least one reflexive-address server so it
// can gather srflx candidates behind NATs. Without these, only host
// candidates are emitted and any non-LAN connection fails ICE.
// Used only when the Factory has no caller-supplied ICEServers.
var defaultSTUNServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
	{URLs: []string{"stun:stun1.l.google.com:19302"}},
	{URLs: []string{"stun:stun2.l.google.com:19302"}},
}

// PeerConnection wraps pion's PeerConnection with Telegram-specific
// signaling glue. One per group-call instance. Send-only audio+video.
type PeerConnection struct {
	pc    *webrtc.PeerConnection
	audio *webrtc.TrackLocalStaticSample
	video *webrtc.TrackLocalStaticSample

	audioSender *webrtc.RTPSender
	videoSender *webrtc.RTPSender

	// monitor is the Factory-shared keepalive + liveness goroutine. We
	// hold the reference so Close can Unregister; there's no per-PC
	// goroutine to stop (the Factory's monitor handles all PCs).
	monitor *FactoryMonitor

	log *slog.Logger

	onStateChange connStateFn

	onStateChangeMu sync.RWMutex

	mu        sync.Mutex
	audioSSRC uint32
	videoSSRC uint32

	closed bool
}

// NewPeerConnection creates an outgoing pion connection with audio and
// video tracks already attached. SSRCs are read from pion's senders after
// AddTrack so the values we announce to Telegram match the SSRCs pion
// will actually emit on the wire.
func NewPeerConnection(f *Factory, log *slog.Logger) (*PeerConnection, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	iceServers := f.ICEServers()
	if len(iceServers) == 0 {
		iceServers = defaultSTUNServers
	}
	pc, err := f.NewPeerConnection(webrtc.Configuration{
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
		SDPSemantics:  webrtc.SDPSemanticsUnifiedPlan,
		ICEServers:    iceServers,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	// Pion's own ICE failure timeout (set in peer_factory) surfaces stuck
	// checking via ICEConnectionStateFailed → OnConnectionStateChange.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debug("ICE state", slog.String("state", state.String()))
	})
	if f.LogICECandidates() {
		// Opt-in (via gotgcall.WithICECandidateLogs()) — every locally-gathered
		// candidate is logged at Debug. The nil candidate signals end-of-gather.
		// Combine with WithPionTraceLogs to also see remote-side checks and
		// pair selection.
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				log.Debug("ICE candidate gather complete")
				return
			}
			log.Debug("ICE candidate gathered",
				slog.String("typ", c.Typ.String()),
				slog.String("proto", c.Protocol.String()),
				slog.String("addr", c.Address),
				slog.Int("port", int(c.Port)),
				slog.String("foundation", c.Foundation),
				slog.Int("component", int(c.Component)))
		})
	}

	audio, err := NewAudioTrack("audio0", "gotgcall-stream")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	audioSender, err := pc.AddTrack(audio)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	video, err := NewVideoTrack("video0", "gotgcall-stream")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	videoSender, err := pc.AddTrack(video)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}

	audioSSRC := senderSSRC(audioSender)
	videoSSRC := senderSSRC(videoSender)
	if audioSSRC == 0 {
		_ = pc.Close()
		return nil, fmt.Errorf("%w: audio sender returned no SSRC", models.ErrInternal)
	}
	// Symmetric video guard. In practice pion v4 assigns the encoding SSRC
	// during AddTrack so this never trips, but if it ever returned 0 (pion
	// regression, upstream API change) FromOfferSDP would emit an empty
	// ssrc-groups — recreating exactly the v0.6.0/v0.6.2 "video declared
	// but never reaches participants" bug. Cheap defense.
	if videoSSRC == 0 {
		_ = pc.Close()
		return nil, fmt.Errorf("%w: video sender returned no SSRC", models.ErrInternal)
	}

	peerConn := &PeerConnection{
		pc:          pc,
		audio:       audio,
		video:       video,
		audioSender: audioSender,
		videoSender: videoSender,
		monitor:     f.Monitor(),
		audioSSRC:   audioSSRC,
		videoSSRC:   videoSSRC,
		log:         log,
	}
	// Register with the Factory-shared monitor. The monitor's tick loop
	// skips PCs that aren't Connected, so we can register here (before
	// Connect) without false-tripping the liveness watchdog. Unregister
	// happens in Close.
	if peerConn.monitor != nil {
		peerConn.monitor.Register(peerConn)
	}

	// Single OnConnectionStateChange registration that forwards to the
	// user-side delegate set via PeerConnection.OnConnectionStateChange.
	// pion only supports one handler per PC, so we must funnel everything
	// through here.
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		peerConn.handleStateChange(translateState(s))
	})

	return peerConn, nil
}

// handleStateChange forwards pion's state transitions to the user-side
// delegate. The shared FactoryMonitor decides per-tick whether to act on
// each PC's state (skip / keepalive / liveness check), so there's no
// per-PC goroutine to start or stop here.
func (p *PeerConnection) handleStateChange(state models.ConnState) {
	p.onStateChangeMu.RLock()
	fn := p.onStateChange
	p.onStateChangeMu.RUnlock()
	if fn != nil {
		fn(state)
	}
}

// senderSSRC returns the SSRC pion has chosen for the sender's first
// encoding. This is what will appear in outbound RTP — using it as the
// announced SSRC keeps Telegram's mixer in sync with our packets.
func senderSSRC(s *webrtc.RTPSender) uint32 {
	if s == nil {
		return 0
	}
	p := s.GetParameters()
	if len(p.Encodings) == 0 {
		return 0
	}
	return uint32(p.Encodings[0].SSRC)
}

// LocalParams returns the JSON string to send to Telegram via
// phone.joinGroupCall. Generates the offer internally.
func (p *PeerConnection) LocalParams() (string, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("%w: create offer: %v", models.ErrInternal, err)
	}
	if err = p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("%w: set local: %v", models.ErrInternal, err)
	}
	return jsonparams.FromOfferSDP(offer.SDP, p.audioSSRC, p.videoSSRC)
}

// Connect finalizes the handshake using Telegram's response JSON.
func (p *PeerConnection) Connect(remoteJSON string) error {
	rp, err := jsonparams.ParseRemote(remoteJSON)
	if err != nil {
		return fmt.Errorf("%w: %v", models.ErrInvalidParams, err)
	}
	local := p.pc.LocalDescription()
	if local == nil {
		return fmt.Errorf("%w: no local description; call LocalParams first", models.ErrInvalidParams)
	}
	answer, err := jsonparams.SynthesizeAnswerSDP(local.SDP, rp)
	if err != nil {
		return fmt.Errorf("%w: synth answer: %v", models.ErrInvalidParams, err)
	}
	return p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	})
}

func (p *PeerConnection) AudioSSRC() uint32                          { return p.audioSSRC }
func (p *PeerConnection) VideoSSRC() uint32                          { return p.videoSSRC }
func (p *PeerConnection) AudioTrack() *webrtc.TrackLocalStaticSample { return p.audio }
func (p *PeerConnection) VideoTrack() *webrtc.TrackLocalStaticSample { return p.video }

// OnConnectionStateChange registers fn for pion state transitions.
// The callback fires from a pion goroutine; fn must not block.
//
// pion only supports one OnConnectionStateChange handler per PC and we
// already installed our internal lifecycle handler in NewPeerConnection.
// This method stores fn as a delegate that the internal handler calls
// after its own bookkeeping.
func (p *PeerConnection) OnConnectionStateChange(fn func(models.ConnState)) {
	p.onStateChangeMu.Lock()
	p.onStateChange = fn
	p.onStateChangeMu.Unlock()
}

func translateState(s webrtc.PeerConnectionState) models.ConnState {
	switch s {
	case webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting:
		return models.Connecting
	case webrtc.PeerConnectionStateConnected:
		return models.Connected
	case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
		return models.Failed
	case webrtc.PeerConnectionStateClosed:
		return models.Closed
	default:
		return models.Connecting
	}
}

func (p *PeerConnection) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	if p.monitor != nil {
		p.monitor.Unregister(p)
	}
	return p.pc.Close()
}
