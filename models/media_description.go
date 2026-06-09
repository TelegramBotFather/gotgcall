package models

type Device int

const (
	Microphone Device = iota
	Camera
)

func (d Device) String() string {
	switch d {
	case Microphone:
		return "microphone"
	case Camera:
		return "camera"
	default:
		return "unknown"
	}
}

type StreamType int

const (
	Audio StreamType = iota
	Video
)

func (s StreamType) String() string {
	switch s {
	case Audio:
		return "audio"
	case Video:
		return "video"
	default:
		return "unknown"
	}
}

const (
	DefaultChannelCount = 2
	OpusSampleRate      = 48000
	OpusFrameDurationMs = 20
	OpusPayloadType     = 111
	VP8PayloadType      = 100
)
