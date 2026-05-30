package wrtc

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/wrtc/jsonparams"
)

// defaultSTUNServers gives pion at least one reflexive-address server so it
// can gather srflx candidates behind NATs. Without these, only host
// candidates are emitted and any non-LAN connection fails ICE.
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

	log *slog.Logger

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

	pc, err := f.NewPeerConnection(webrtc.Configuration{
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
		SDPSemantics:  webrtc.SDPSemanticsUnifiedPlan,
		ICEServers:    defaultSTUNServers,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	// ICE-stuck watchdog: if ICE stays in Checking for more than 5 s, close
	// the PeerConnection so the caller can retry. Without this, a flaky NAT
	// hole-punch leaves us hanging silently.
	var (
		iceStuckTimer *time.Timer
		iceTimerMu    sync.Mutex
	)
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debug("ICE state", slog.String("state", state.String()))
		iceTimerMu.Lock()
		defer iceTimerMu.Unlock()
		if iceStuckTimer != nil {
			iceStuckTimer.Stop()
			iceStuckTimer = nil
		}
		if state == webrtc.ICEConnectionStateChecking {
			iceStuckTimer = time.AfterFunc(5*time.Second, func() {
				log.Warn("ICE stuck in checking for 5s, closing peer connection")
				_ = pc.Close()
			})
		}
	})

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

	return &PeerConnection{
		pc:        pc,
		audio:     audio,
		video:     video,
		audioSSRC: audioSSRC,
		videoSSRC: videoSSRC,
		log:       log,
	}, nil
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
func (p *PeerConnection) OnConnectionStateChange(fn func(models.ConnState)) {
	p.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fn(translateState(s))
	})
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
	return p.pc.Close()
}
