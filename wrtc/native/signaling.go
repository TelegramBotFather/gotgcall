package native

import (
	"encoding/json"
	"fmt"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/wrtc/jsonparams"
)

// RTP header extension IDs. Values match the historical SDP allocations
// used in v0.6.x so an in-place upgrade keeps the same wire shape.
const (
	ExtAudioLevelID       uint8 = 1
	ExtAbsSendTimeID      uint8 = 2
	ExtTransportCCID      uint8 = 3
	ExtSdesMidID          uint8 = 4
	ExtVideoOrientationID uint8 = 5
)

// RTP header extension URIs.
const (
	URIAudioLevel       = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"
	URIAbsSendTime      = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
	URITransportCC      = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
	URISdesMid          = "urn:ietf:params:rtp-hdrext:sdes:mid"
	URIVideoOrientation = "urn:3gpp:video-orientation"
)

// buildLocalParamsJSON produces the JoinGroupCall blob from per-call state.
// No SDP detour — fields are populated directly so any pion-webrtc /
// PeerConnection bug along the SDP path (the rollback limitation that
// killed the v0.6.26 role-swap attempt) is bypassed entirely.
//
// The shape mirrors `jsonparams.LocalParams` exactly — Telegram's SFU
// has been accepting this layout in production for the entire v0.6.x line.
func buildLocalParamsJSON(ufrag, pwd, fingerprintSHA256 string, audioSSRC, videoSSRC uint32) (string, error) {
	if ufrag == "" || pwd == "" || fingerprintSHA256 == "" {
		return "", fmt.Errorf("buildLocalParams: ufrag/pwd/fingerprint required")
	}
	if audioSSRC == 0 {
		return "", fmt.Errorf("buildLocalParams: audio SSRC required")
	}

	lp := jsonparams.LocalParams{
		Ufrag: ufrag,
		Pwd:   pwd,
		SSRC:  audioSSRC,
		Fingerprints: []jsonparams.Fingerprint{
			// setup=passive: we are the DTLS server (SSL_SERVER role, matching
			// ntgcalls). Telegram's SFU is the DTLS client and sends
			// ClientHello first after ICE settles.
			{Hash: "sha-256", Setup: "passive", Fingerprint: fingerprintSHA256},
		},
		PayloadTypes: []jsonparams.PayloadType{
			{
				ID:        models.OpusPayloadType,
				Name:      "opus",
				Clockrate: models.OpusSampleRate,
				Channels:  2,
				Parameters: map[string]string{
					"minptime":          "10",
					"useinbandfec":      "1",
					"stereo":            "1",
					"sprop-stereo":      "1",
					"maxaveragebitrate": "510000",
				},
				RTCPFbs: []jsonparams.RTCPFb{{Type: "transport-cc"}},
			},
			{
				ID:        models.VP8PayloadType,
				Name:      "VP8",
				Clockrate: 90000,
				RTCPFbs: []jsonparams.RTCPFb{
					{Type: "goog-remb"},
					{Type: "transport-cc"},
					{Type: "ccm", Subtype: "fir"},
					{Type: "nack"},
					{Type: "nack", Subtype: "pli"},
				},
			},
		},
		RTPHdrExts: []jsonparams.RTPHdrExt{
			{ID: int(ExtAudioLevelID), URI: URIAudioLevel},
			{ID: int(ExtAbsSendTimeID), URI: URIAbsSendTime},
			{ID: int(ExtTransportCCID), URI: URITransportCC},
			{ID: int(ExtSdesMidID), URI: URISdesMid},
			{ID: int(ExtVideoOrientationID), URI: URIVideoOrientation},
		},
	}

	// Video FID group: Telegram requires the (primary, rtx) pair even when
	// we never actually emit RTX packets. The second SSRC is a number the
	// SFU expects in the manifest. Empty groups list when video isn't
	// configured (audio-only call).
	if videoSSRC != 0 {
		lp.SSRCGroups = []jsonparams.SSRCGroup{{
			Semantics: "FID",
			Sources:   []uint32{videoSSRC, videoSSRC + 1},
		}}
	} else {
		lp.SSRCGroups = []jsonparams.SSRCGroup{}
	}

	out, err := json.Marshal(lp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
