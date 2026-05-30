package wrtc

import (
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const (
	audioLevelURI       = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"
	absSendTimeURI      = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
	transportCCURI      = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
	sdesMidURI          = "urn:ietf:params:rtp-hdrext:sdes:mid"
	videoOrientationURI = "urn:3gpp:video-orientation"
)

// audioLevelInterceptorFactory builds interceptors that stamp the
// ssrc-audio-level (RFC 6464) and abs-send-time extensions on every
// outgoing audio RTP packet. Telegram's SFU silently drops streams that
// don't carry audio-level (it treats them as silence and stops forwarding
// them to listeners); pion's defaults do not stamp these for us.
type audioLevelInterceptorFactory struct{}

func (f *audioLevelInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &audioLevelInterceptor{streams: make(map[uint32]*audioLevelStream)}, nil
}

type audioLevelStream struct {
	audioLevelID  uint8
	absSendTimeID uint8
	hasAudioLevel bool
	hasAbsSend    bool
}

type audioLevelInterceptor struct {
	interceptor.NoOp
	streams map[uint32]*audioLevelStream
	mu      sync.RWMutex
}

func (a *audioLevelInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	s := &audioLevelStream{}
	for _, ext := range info.RTPHeaderExtensions {
		switch ext.URI {
		case audioLevelURI:
			s.audioLevelID = uint8(ext.ID)
			s.hasAudioLevel = true
		case absSendTimeURI:
			s.absSendTimeID = uint8(ext.ID)
			s.hasAbsSend = true
		}
	}
	a.mu.Lock()
	a.streams[info.SSRC] = s
	a.mu.Unlock()
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		if s.hasAudioLevel {
			// Voice-activity bit (0x80) | level in -dBov (0..127); fixed -20 dBov.
			_ = header.SetExtension(s.audioLevelID, []byte{0x80 | 20})
		}
		if s.hasAbsSend {
			// 24-bit fixed-point seconds since epoch * 2^18, big-endian.
			now := time.Now()
			abs := (uint64(now.Unix())<<18 | uint64(now.Nanosecond())*uint64(1<<18)/uint64(1e9)) & 0x00FFFFFF
			_ = header.SetExtension(s.absSendTimeID, []byte{byte(abs >> 16), byte(abs >> 8), byte(abs)})
		}
		return writer.Write(header, payload, attrs)
	})
}

func (a *audioLevelInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	a.mu.Lock()
	delete(a.streams, info.SSRC)
	a.mu.Unlock()
}

// markerClearInterceptorFactory clears RTP marker on outbound audio.
// Pion's packetizer marks every single-payload Opus packet, but per
// RFC 7587 the marker should only be set on the first packet after
// silence; an always-set marker forces jitter-buffer resync at the SFU
// and degrades audio quality.
type markerClearInterceptorFactory struct{}

func (f *markerClearInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &markerClearInterceptor{}, nil
}

type markerClearInterceptor struct {
	interceptor.NoOp
}

func (m *markerClearInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !strings.HasPrefix(info.MimeType, "audio/") {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		header.Marker = false
		return writer.Write(header, payload, attrs)
	})
}
