package jsonparams

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

// ParseRemote decodes Telegram's response JSON. Lenient: unknown top-level
// keys are ignored. Missing required keys (transport.ufrag/pwd/fingerprints)
// yield a typed error.
func ParseRemote(raw string) (*RemoteParams, error) {
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

// SynthesizeAnswerSDP builds an SDP answer from Telegram's remote params,
// reusing the codec/extension/mid layout from our own offer SDP. The
// answer's transport-level info (ufrag, pwd, fingerprint, candidates) is
// substituted with Telegram's values.
func SynthesizeAnswerSDP(offerSDP string, rp *RemoteParams) (string, error) {
	var off sdp.SessionDescription
	if err := off.UnmarshalString(offerSDP); err != nil {
		return "", fmt.Errorf("parse offer: %w", err)
	}

	ans := &sdp.SessionDescription{
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

	// Telegram's SFU is ICE-lite (server-side, never sends connectivity
	// checks). Declaring this in the answer lets pion skip waiting for
	// reverse checks and nominate pairs faster — without it pion's ICE
	// state machine may intermittently time out waiting for checks from
	// the SFU that never arrive.
	ans.Attributes = append(ans.Attributes, sdp.NewPropertyAttribute("ice-lite"))

	// Copy session-level group attribute (BUNDLE).
	for _, a := range off.Attributes {
		if a.Key == "group" || a.Key == "msid-semantic" {
			ans.Attributes = append(ans.Attributes, a)
		}
	}

	for _, om := range off.MediaDescriptions {
		am := mirrorMediaSection(om, rp)
		ans.MediaDescriptions = append(ans.MediaDescriptions, am)
	}

	b, err := ans.Marshal()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mirrorMediaSection builds an answer m-section that mirrors the offer's
// codec/extension/mid set but substitutes Telegram's transport info.
// IPv6 candidates are filtered out — Telegram's group-call SFU only
// operates over IPv4, and stale IPv6 entries waste ICE pairing time.
func mirrorMediaSection(om *sdp.MediaDescription, rp *RemoteParams) *sdp.MediaDescription {
	remoteIP, remotePort := findFirstIPv4Candidate(rp.Transport.Candidates)
	am := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   om.MediaName.Media,
			Port:    sdp.RangedPort{Value: remotePort},
			Protos:  om.MediaName.Protos,
			Formats: om.MediaName.Formats,
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: remoteIP},
		},
	}

	// Transport: substitute Telegram's values.
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
	// DTLS role: derive from Telegram's fingerprint "setup" field. Telegram's
	// SFU typically sends "active" (it initiates the DTLS handshake, we are
	// passive/server — matching ntgcalls' hardcoded SSL_SERVER). If the field
	// is missing or unrecognized, default to "active" for backwards compat.
	setup := remoteSetup(rp)
	am.Attributes = append(am.Attributes, sdp.NewAttribute("setup", setup))

	// mid, rtcp-mux, direction (recvonly since we're send-only).
	for _, a := range om.Attributes {
		switch a.Key {
		case "mid", "rtcp-mux", "rtcp-rsize":
			am.Attributes = append(am.Attributes, a)
		}
	}
	am.Attributes = append(am.Attributes, sdp.NewPropertyAttribute("recvonly"))

	// Carry codec set across unchanged (rtpmap/fmtp/rtcp-fb/extmap).
	for _, a := range om.Attributes {
		switch a.Key {
		case "rtpmap", "fmtp", "rtcp-fb", "extmap":
			am.Attributes = append(am.Attributes, a)
		}
	}

	// ICE candidates from Telegram — IPv4 only.
	for _, c := range rp.Transport.Candidates {
		if ip := net.ParseIP(c.IP); ip == nil || ip.To4() == nil {
			continue
		}
		am.Attributes = append(am.Attributes, sdp.NewAttribute("candidate", candidateToSDP(c)))
	}
	am.Attributes = append(am.Attributes, sdp.NewPropertyAttribute("end-of-candidates"))

	return am
}

func candidateToSDP(c Candidate) string {
	// SDP candidate line format:
	// <foundation> <component> <transport> <priority> <ip> <port> typ <type> [raddr <ip> rport <port>] [generation <n>] [network-id <n>]
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

func remoteSetup(rp *RemoteParams) string {
	for _, fp := range rp.Transport.Fingerprints {
		switch fp.Setup {
		case "active", "passive", "actpass":
			return fp.Setup
		}
	}
	return "active"
}

func findFirstIPv4Candidate(candidates []Candidate) (string, int) {
	for _, c := range candidates {
		if ip := net.ParseIP(c.IP); ip != nil && ip.To4() != nil {
			port, _ := strconv.Atoi(c.Port)
			if port == 0 {
				port = 1
			}
			return c.IP, port
		}
	}
	return "0.0.0.0", 9
}
