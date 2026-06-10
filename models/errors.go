package models

import "errors"

var (
	ErrConnectionExists    = errors.New("gotgcall: call already exists for chat")
	ErrConnectionNotFound  = errors.New("gotgcall: no call for chat")
	ErrConnectionFailed    = errors.New("gotgcall: ICE failed permanently")
	ErrInvalidParams       = errors.New("gotgcall: malformed telegram params")
	ErrUnsupportedCallMode = errors.New("gotgcall: call mode not supported")
	ErrFFmpegSpawn         = errors.New("gotgcall: ffmpeg failed to start")
	ErrFFmpegCrashed       = errors.New("gotgcall: ffmpeg exited non-zero")
	ErrFile                = errors.New("gotgcall: input file unreadable")
	ErrClosed              = errors.New("gotgcall: client closed")
	ErrInternal            = errors.New("gotgcall: internal error")
	ErrNotConnected        = errors.New("gotgcall: call not connected")
	ErrWrongMode           = errors.New("gotgcall: operation not valid for call mode")
	ErrNoSource            = errors.New("gotgcall: no source currently playing")
	ErrSeekUnsupported     = errors.New("gotgcall: source is not seekable")
)
