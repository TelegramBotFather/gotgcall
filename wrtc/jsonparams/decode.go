package jsonparams

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pion/sdp/v3"
)

// ErrUnsupportedMode signals that Telegram's response describes the group
// call as an RTMP livestream ({"rtmp": ...}) or an MTProto broadcast
// stream ({"stream": ...}) rather than a WebRTC call ({"transport": ...}).
// gotgcall has no MTProto segment-stream implementation, so the caller
// must surface this as "not joinable as a voice chat". Mirrors ntgcalls'
// branch in wrtc/src/models/response_payload.cpp:23-30.
var ErrUnsupportedMode = errors.New("call mode unsupported")

// ParseRemote decodes Telegram's response JSON. Lenient: unknown top-level
// keys are ignored. Missing required keys (transport.ufrag/pwd/fingerprints)
// yield a typed error. RTMP/Stream responses yield ErrUnsupportedMode.
func ParseRemote(raw string) (*RemoteParams, error) {
	var probe struct {
		Rtmp   json.RawMessage `json:"rtmp"`
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		if jsonFieldPresent(probe.Rtmp) {
			return nil, fmt.Errorf("%w: rtmp livestream", ErrUnsupportedMode)
		}
		if jsonFieldPresent(probe.Stream) {
			return nil, fmt.Errorf("%w: mtproto broadcast stream", ErrUnsupportedMode)
		}
	}
	rp := &RemoteParams{}
	if err := json.Unmarshal([]byte(raw), rp); err != nil {
		return nil, fmt.Errorf("decode remote params: %w", err)
	}
	if rp.Transport.Ufrag == "" || rp.Transport.Pwd == "" {
		return nil, fmt.Errorf("remote params missing ice creds")
	}
	if len(rp.Transport.Fingerprints) == 0 {
		return nil, fmt.Errorf("remote params missing fingerprint")
	}
	return rp, nil
}

func jsonFieldPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// SynthesizeRemoteOfferSDP builds an OFFER-shaped SDP from Telegram's
// response so the caller can feed it to pion as the remote description.
// The offer-shape (vs answer-shape, which we used pre-v0.6.26) flips pion
// into the ICE answerer role — pion's role logic at peerconnection.go:1351
// only assigns ICEROLE_CONTROLLING when `weOffer && !ICELite`; when we
// rollback our local offer and SetRemoteDescription(offer) instead, weOffer
// becomes false and pion ends up ICEROLE_CONTROLLED. That matches what
// ntgcalls (group_connection.cpp:511-513) and tgcalls (GroupNetworkManager
// .cpp:376-378) do, and avoids the STUN 487 Role Conflict storm that pion
// can't recover from (pion v4.2.7 silently drops ClassErrorResponse at
// agent.go:1701, so the role-conflict flip never executes — see the v0.6.25
// diagnostic logs).
//
// Codec/extension/mid layout is mirrored from our offer so pion's answer
// keeps the same transceiver/SSRC binding it negotiated when we generated
// our credentials.
func SynthesizeRemoteOfferSDP(offerSDP string, rp *RemoteParams) (string, error) {
	var off sdp.SessionDescription
	if err := off.UnmarshalString(offerSDP); err != nil {
		return "", fmt.Errorf("parse offer: %w", err)
	}
	syn := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      off.Origin.SessionID,
			SessionVersion: off.Origin.SessionVersion + 1,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: "0.0.0.0",
		},
		SessionName: "-",
		TimeDescriptions: []sdp.TimeDescription{
			{Timing: sdp.Timing{StartTime: 0, StopTime: 0}},
		},
	}
	// No a=ice-lite at session level: we WANT pion's role logic to land
	// on ICEROLE_CONTROLLED (i.e. answerer + remote!=lite). Adding ice-lite
	// here would flip pion back to controlling and reintroduce the 487
	// role-conflict storm we just fixed.
	for _, a := range off.Attributes {
		if a.Key == "group" || a.Key == "msid-semantic" {
			syn.Attributes = append(syn.Attributes, a)
		}
	}
	for _, om := range off.MediaDescriptions {
		syn.MediaDescriptions = append(syn.MediaDescriptions, mirrorMediaSectionAsOffer(om, rp))
	}
	b, err := syn.Marshal()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mirrorMediaSectionAsOffer builds an m-section that LOOKS like Telegram
// offered it: codec/extension/mid set carried over from our original
// offer, but transport-level info (ufrag, pwd, fingerprint, candidates)
// substituted with Telegram's values, DTLS direction set to actpass
// (offer default — pion's CreateAnswer will pick passive so we end up the
// DTLS server, matching ntgcalls' SSL_SERVER), and direction set to
// recvonly so pion answers with sendonly (we send, Telegram receives —
// matching the actual data flow of a music bot).
func mirrorMediaSectionAsOffer(om *sdp.MediaDescription, rp *RemoteParams) *sdp.MediaDescription {
	am := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   om.MediaName.Media,
			Port:    sdp.RangedPort{Value: 9},
			Protos:  om.MediaName.Protos,
			Formats: om.MediaName.Formats,
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: "0.0.0.0"},
		},
	}
	am.Attributes = append(am.Attributes,
		sdp.NewAttribute("rtcp", "9 IN IP4 0.0.0.0"),
		sdp.NewAttribute("ice-ufrag", rp.Transport.Ufrag),
		sdp.NewAttribute("ice-pwd", rp.Transport.Pwd),
		sdp.NewAttribute("ice-options", "trickle"),
	)
	for _, fp := range rp.Transport.Fingerprints {
		am.Attributes = append(am.Attributes,
			sdp.NewAttribute("fingerprint", fp.Hash+" "+fp.Fingerprint),
		)
	}
	// DTLS role: setup=actpass is the offer default. Pion's CreateAnswer
	// picks setup=passive (so pion = DTLS server, matching ntgcalls'
	// SSL_SERVER). Telegram then sends ClientHello as DTLS-active.
	am.Attributes = append(am.Attributes, sdp.NewAttribute("setup", "actpass"))
	for _, a := range om.Attributes {
		switch a.Key {
		case "mid", "rtcp-mux", "rtcp-rsize":
			am.Attributes = append(am.Attributes, a)
		}
	}
	// recvonly from the (synthetic) offerer's POV — Telegram only receives;
	// pion's answer mirrors as sendonly, which matches our music-bot flow.
	am.Attributes = append(am.Attributes, sdp.NewPropertyAttribute("recvonly"))
	for _, a := range om.Attributes {
		switch a.Key {
		case "rtpmap", "fmtp", "rtcp-fb", "extmap":
			am.Attributes = append(am.Attributes, a)
		}
	}
	for _, c := range rp.Transport.Candidates {
		am.Attributes = append(am.Attributes, sdp.NewAttribute("candidate", candidateToSDP(c)))
	}
	am.Attributes = append(am.Attributes, sdp.NewPropertyAttribute("end-of-candidates"))
	return am
}

func candidateToSDP(c Candidate) string {
	var b strings.Builder
	b.WriteString(c.Foundation)
	b.WriteByte(' ')
	b.WriteString(c.Component)
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(c.Protocol))
	b.WriteByte(' ')
	b.WriteString(c.Priority)
	b.WriteByte(' ')
	b.WriteString(c.IP)
	b.WriteByte(' ')
	b.WriteString(c.Port)
	b.WriteString(" typ ")
	if c.Type == "" {
		b.WriteString("host")
	} else {
		b.WriteString(c.Type)
	}
	if c.RelAddr != "" && c.RelPort != "" {
		b.WriteString(" raddr ")
		b.WriteString(c.RelAddr)
		b.WriteString(" rport ")
		b.WriteString(c.RelPort)
	}
	if c.Generation != "" {
		b.WriteString(" generation ")
		b.WriteString(c.Generation)
	}
	if c.Network != "" {
		b.WriteString(" network-id ")
		b.WriteString(c.Network)
	}
	if c.TCPType != "" {
		b.WriteString(" tcptype ")
		b.WriteString(c.TCPType)
	}
	return b.String()
}
