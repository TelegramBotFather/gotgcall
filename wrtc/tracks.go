package wrtc

import (
	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/models"
)

// NewAudioTrack creates a TrackLocalStaticSample carrying Opus (PT=111).
func NewAudioTrack(id, streamID string) (*webrtc.TrackLocalStaticSample, error) {
	return webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   uint32(models.OpusSampleRate),
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=510000",
		},
		id, streamID,
	)
}

// NewVideoTrack creates a TrackLocalStaticSample carrying VP8 (PT=100).
func NewVideoTrack(id, streamID string) (*webrtc.TrackLocalStaticSample, error) {
	return webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		id, streamID,
	)
}
