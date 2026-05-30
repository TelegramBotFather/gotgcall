package wrtc

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pion/webrtc/v4"

	"gotgcall/models"
	"gotgcall/wrtc/jsonparams"
)

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
// video tracks already attached. SSRCs are pre-generated (audio random,
// video = audio+1) and bound as an FID group when the local params are
// emitted.
func NewPeerConnection(f *Factory, log *slog.Logger) (*PeerConnection, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	audioSSRC := randUint32()
	videoSSRC := audioSSRC + 1

	pc, err := f.NewPeerConnection(webrtc.Configuration{
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
		SDPSemantics:  webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	audio, err := NewAudioTrack("audio0", "gotgcall-stream")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err = pc.AddTrack(audio); err != nil {
		_ = pc.Close()
		return nil, err
	}
	video, err := NewVideoTrack("video0", "gotgcall-stream")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err = pc.AddTrack(video); err != nil {
		_ = pc.Close()
		return nil, err
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

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}
