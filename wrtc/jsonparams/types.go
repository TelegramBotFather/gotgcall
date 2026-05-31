// Package jsonparams encodes and decodes the SDP-like JSON envelope that
// Telegram's group-call signaling uses in place of standard SDP O/A.
package jsonparams

// LocalParams is what we send to Telegram via phone.JoinGroupCall.
// Field shape matches ntgcalls' getJoinPayload() output:
//   - "ssrc" is the audio SSRC only; video SSRC is inferred server-side from
//     the sdes-mid RTP header extension on incoming video packets.
//   - "ssrc-groups" is always emitted (as an empty array if there are no
//     video FID/SIM groups) to mirror ntgcalls — never as a cross-media
//     FID:[audio, video] pair, which causes Telegram's SFU to drop video.
type LocalParams struct {
	Ufrag        string        `json:"ufrag"`
	Pwd          string        `json:"pwd"`
	Fingerprints []Fingerprint `json:"fingerprints"`
	SSRCGroups   []SSRCGroup   `json:"ssrc-groups"`
	PayloadTypes []PayloadType `json:"payload-types"`
	RTPHdrExts   []RTPHdrExt   `json:"rtp-hdrexts"`
	SSRC         uint32        `json:"ssrc"`
}

// RemoteParams is what Telegram returns. Schema is more lenient than
// LocalParams; unknown keys are ignored.
type RemoteParams struct {
	Transport Transport `json:"transport"`
}

type Transport struct {
	Ufrag        string        `json:"ufrag"`
	Pwd          string        `json:"pwd"`
	Fingerprints []Fingerprint `json:"fingerprints"`
	Candidates   []Candidate   `json:"candidates"`
}

type Fingerprint struct {
	Hash        string `json:"hash"`
	Setup       string `json:"setup,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

type SSRCGroup struct {
	Semantics string   `json:"semantics"`
	Sources   []uint32 `json:"sources"`
}

type PayloadType struct {
	Parameters map[string]string `json:"parameters,omitempty"`
	Name       string            `json:"name"`
	RTCPFbs    []RTCPFb          `json:"rtcp-fbs,omitempty"`
	ID         int               `json:"id"`
	Clockrate  int               `json:"clockrate"`
	Channels   int               `json:"channels,omitempty"`
}

type RTCPFb struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

type RTPHdrExt struct {
	URI string `json:"uri"`
	ID  int    `json:"id"`
}

// Candidate mirrors a libnice/ICE candidate. All string for forward
// compatibility with Telegram's quoted-numeric serialization.
type Candidate struct {
	Generation string `json:"generation"`
	Component  string `json:"component"`
	Protocol   string `json:"protocol"`
	Port       string `json:"port"`
	IP         string `json:"ip"`
	Foundation string `json:"foundation"`
	ID         string `json:"id"`
	Priority   string `json:"priority"`
	Type       string `json:"type"`
	Network    string `json:"network"`
	RelAddr    string `json:"rel-addr,omitempty"`
	RelPort    string `json:"rel-port,omitempty"`
	TCPType    string `json:"tcptype,omitempty"`
}
