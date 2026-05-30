package models

type MediaState struct {
	Muted        bool
	Paused       bool
	VideoStopped bool
}

type ConnState int

const (
	Connecting ConnState = iota
	Connected
	Failed
	Closed
	Timeout
)

func (s ConnState) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Connected:
		return "connected"
	case Failed:
		return "failed"
	case Closed:
		return "closed"
	case Timeout:
		return "timeout"
	default:
		return "unknown"
	}
}

type NetworkInfo struct {
	State ConnState
}

type CallInfo struct {
	CaptureTimeMs uint64
}
