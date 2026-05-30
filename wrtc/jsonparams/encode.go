package jsonparams

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

// FromOfferSDP extracts the fields Telegram expects from a pion-generated
// offer SDP (the result of PeerConnection.CreateOffer + SetLocalDescription)
// and returns a JSON-marshaled LocalParams.
//
// audioSSRC and videoSSRC override the SSRCs pion put in the offer so the
// caller can preallocate them (Telegram requires the audio SSRC to be the
// one passed in phone.joinGroupCall's Source field).
func FromOfferSDP(offerSDP string, audioSSRC, videoSSRC uint32) (string, error) {
	var s sdp.SessionDescription
	if err := s.UnmarshalString(offerSDP); err != nil {
		return "", fmt.Errorf("parse offer: %w", err)
	}

	lp := LocalParams{SSRC: audioSSRC}

	// ICE creds and DTLS fingerprint may live at session level or per-media.
	lp.Ufrag, lp.Pwd = sessionAttr(&s, "ice-ufrag"), sessionAttr(&s, "ice-pwd")
	if fp := sessionAttr(&s, "fingerprint"); fp != "" {
		lp.Fingerprints = append(lp.Fingerprints, parseFingerprint(fp, "active"))
	}

	seenAudio, seenVideo := false, false
	for _, md := range s.MediaDescriptions {
		ufrag := mediaAttr(md, "ice-ufrag")
		pwd := mediaAttr(md, "ice-pwd")
		fp := mediaAttr(md, "fingerprint")
		setup := mediaAttr(md, "setup")
		if ufrag != "" {
			lp.Ufrag = ufrag
		}
		if pwd != "" {
			lp.Pwd = pwd
		}
		if fp != "" {
			lp.Fingerprints = []Fingerprint{parseFingerprint(fp, setup)}
		}

		switch md.MediaName.Media {
		case "audio":
			if seenAudio {
				continue
			}
			seenAudio = true
			lp.PayloadTypes = append(lp.PayloadTypes, audioPayloadTypes(md)...)
			lp.RTPHdrExts = append(lp.RTPHdrExts, hdrExts(md)...)
		case "video":
			if seenVideo {
				continue
			}
			seenVideo = true
			lp.PayloadTypes = append(lp.PayloadTypes, videoPayloadTypes(md)...)
			lp.RTPHdrExts = mergeHdrExts(lp.RTPHdrExts, hdrExts(md))
		}
	}

	if videoSSRC != 0 && audioSSRC != 0 {
		lp.SSRCGroups = []SSRCGroup{{Semantics: "FID", Sources: []uint32{audioSSRC, videoSSRC}}}
	}

	out, err := json.Marshal(lp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func sessionAttr(s *sdp.SessionDescription, key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

func mediaAttr(m *sdp.MediaDescription, key string) string {
	for _, a := range m.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

func parseFingerprint(raw, setup string) Fingerprint {
	parts := strings.SplitN(raw, " ", 2)
	fp := Fingerprint{Setup: setup}
	if len(parts) == 2 {
		fp.Hash = parts[0]
		fp.Fingerprint = parts[1]
	} else {
		fp.Fingerprint = raw
	}
	if fp.Setup == "" {
		fp.Setup = "active"
	}
	return fp
}

func audioPayloadTypes(md *sdp.MediaDescription) []PayloadType {
	var out []PayloadType
	for _, fmtID := range md.MediaName.Formats {
		id, err := strconv.Atoi(fmtID)
		if err != nil {
			continue
		}
		rtpmap := findRtpmap(md, fmtID)
		if rtpmap == nil {
			continue
		}
		if !strings.EqualFold(rtpmap.name, "opus") {
			continue
		}
		pt := PayloadType{
			ID:        id,
			Name:      rtpmap.name,
			Clockrate: rtpmap.clockrate,
			Channels:  rtpmap.channels,
		}
		if pt.Channels == 0 {
			pt.Channels = 2
		}
		pt.Parameters = parseFmtp(md, fmtID)
		pt.RTCPFbs = findRtcpFbs(md, fmtID)
		out = append(out, pt)
	}
	return out
}

func videoPayloadTypes(md *sdp.MediaDescription) []PayloadType {
	var out []PayloadType
	for _, fmtID := range md.MediaName.Formats {
		id, err := strconv.Atoi(fmtID)
		if err != nil {
			continue
		}
		rtpmap := findRtpmap(md, fmtID)
		if rtpmap == nil {
			continue
		}
		if !strings.EqualFold(rtpmap.name, "VP8") {
			continue
		}
		pt := PayloadType{
			ID:        id,
			Name:      rtpmap.name,
			Clockrate: rtpmap.clockrate,
		}
		pt.RTCPFbs = findRtcpFbs(md, fmtID)
		out = append(out, pt)
	}
	return out
}

type rtpmapEntry struct {
	name      string
	clockrate int
	channels  int
}

func findRtpmap(md *sdp.MediaDescription, fmtID string) *rtpmapEntry {
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		parts := strings.SplitN(a.Value, " ", 2)
		if len(parts) != 2 || parts[0] != fmtID {
			continue
		}
		body := parts[1] // e.g. "opus/48000/2"
		segs := strings.Split(body, "/")
		entry := &rtpmapEntry{}
		if len(segs) > 0 {
			entry.name = segs[0]
		}
		if len(segs) > 1 {
			if cr, err := strconv.Atoi(segs[1]); err == nil {
				entry.clockrate = cr
			}
		}
		if len(segs) > 2 {
			if ch, err := strconv.Atoi(segs[2]); err == nil {
				entry.channels = ch
			}
		}
		return entry
	}
	return nil
}

func parseFmtp(md *sdp.MediaDescription, fmtID string) map[string]string {
	for _, a := range md.Attributes {
		if a.Key != "fmtp" {
			continue
		}
		parts := strings.SplitN(a.Value, " ", 2)
		if len(parts) != 2 || parts[0] != fmtID {
			continue
		}
		out := make(map[string]string)
		for kv := range strings.SplitSeq(parts[1], ";") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			before, after, ok := strings.Cut(kv, "=")
			if !ok {
				out[kv] = ""
				continue
			}
			out[before] = after
		}
		return out
	}
	return nil
}

func findRtcpFbs(md *sdp.MediaDescription, fmtID string) []RTCPFb {
	var out []RTCPFb
	for _, a := range md.Attributes {
		if a.Key != "rtcp-fb" {
			continue
		}
		parts := strings.SplitN(a.Value, " ", 2)
		if len(parts) != 2 || parts[0] != fmtID {
			continue
		}
		fb := parts[1]
		before, after, ok := strings.Cut(fb, " ")
		if !ok {
			out = append(out, RTCPFb{Type: fb})
			continue
		}
		out = append(out, RTCPFb{Type: before, Subtype: after})
	}
	return out
}

func hdrExts(md *sdp.MediaDescription) []RTPHdrExt {
	var out []RTPHdrExt
	for _, a := range md.Attributes {
		if a.Key != "extmap" {
			continue
		}
		// extmap value form: "<id>[/<dir>] <uri> [<extensionattributes>]"
		v := a.Value
		before, after, ok := strings.Cut(v, " ")
		if !ok {
			continue
		}
		idPart := before
		uri := strings.TrimSpace(after)
		// Strip trailing extension attributes from URI.
		if sp := strings.IndexByte(uri, ' '); sp > 0 {
			uri = uri[:sp]
		}
		if slash := strings.IndexByte(idPart, '/'); slash > 0 {
			idPart = idPart[:slash]
		}
		id, err := strconv.Atoi(idPart)
		if err != nil {
			continue
		}
		out = append(out, RTPHdrExt{ID: id, URI: uri})
	}
	return out
}

func mergeHdrExts(a, b []RTPHdrExt) []RTPHdrExt {
	seen := make(map[int]bool, len(a))
	for _, e := range a {
		seen[e.ID] = true
	}
	for _, e := range b {
		if !seen[e.ID] {
			a = append(a, e)
			seen[e.ID] = true
		}
	}
	return a
}
