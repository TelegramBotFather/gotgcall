package native

import (
	"fmt"
	"strings"

	"github.com/pion/ice/v4"

	"github.com/annihilatorrrr/gotgcall/wrtc/jsonparams"
)

// remoteParams is the parsed shape of Telegram's JoinGroupCall response.
// Only the fields the native stack actually consumes — codec list and
// header-extension IDs in the response are ignored because we hardcode
// the Telegram-mandated set in signaling.go (matching what ntgcalls does
// internally; the SFU expects PT=111 Opus + PT=100 VP8 regardless of
// what it advertises back).
type remoteParams struct {
	ufrag      string
	pwd        string
	candidates []jsonparams.Candidate
}

// parseRemoteJSON unmarshals Telegram's response. We reuse
// jsonparams.ParseRemote so the lenient/strict semantics stay aligned
// with the old SDP path; only the consumer differs.
func parseRemoteJSON(raw string) (*remoteParams, error) {
	rp, err := jsonparams.ParseRemote(raw)
	if err != nil {
		return nil, err
	}
	return &remoteParams{
		ufrag:      rp.Transport.Ufrag,
		pwd:        rp.Transport.Pwd,
		candidates: rp.Transport.Candidates,
	}, nil
}

// buildICECandidate translates one of Telegram's JSON candidate entries
// into a pion ice.Candidate via UnmarshalCandidate. Building the
// canonical SDP candidate line first (rather than poking at
// CandidateHostConfig directly) lets pion own the field-level parsing
// and validation — same code path the test suite exercises.
func buildICECandidate(c jsonparams.Candidate) (ice.Candidate, error) {
	if c.IP == "" || c.Port == "" {
		return nil, fmt.Errorf("candidate missing ip/port")
	}
	var b strings.Builder
	b.Grow(96)
	if c.Foundation == "" {
		b.WriteByte('1')
	} else {
		b.WriteString(c.Foundation)
	}
	b.WriteByte(' ')
	if c.Component == "" {
		b.WriteByte('1')
	} else {
		b.WriteString(c.Component)
	}
	b.WriteByte(' ')
	if c.Protocol == "" {
		b.WriteString("UDP")
	} else {
		b.WriteString(strings.ToUpper(c.Protocol))
	}
	b.WriteByte(' ')
	if c.Priority == "" {
		b.WriteString("2130706431")
	} else {
		b.WriteString(c.Priority)
	}
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
	if c.TCPType != "" {
		b.WriteString(" tcptype ")
		b.WriteString(c.TCPType)
	}
	return ice.UnmarshalCandidate(b.String())
}
