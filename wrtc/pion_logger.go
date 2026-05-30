package wrtc

import (
	"strings"

	"github.com/pion/logging"
)

// filteringLoggerFactory wraps pion's DefaultLoggerFactory and silences a
// curated list of error messages that are noise in the Telegram group-call
// scenario. Telegram's mixer forwards every other participant's RTP to us;
// our PeerConnection has only send-only tracks, so pion logs "Simulcast
// probing failed" for each unknown incoming SSRC. Useless to operators.
type filteringLoggerFactory struct {
	inner logging.LoggerFactory
}

func newFilteringLoggerFactory() logging.LoggerFactory {
	return &filteringLoggerFactory{inner: logging.NewDefaultLoggerFactory()}
}

func (f *filteringLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &filteringLogger{inner: f.inner.NewLogger(scope)}
}

type filteringLogger struct {
	inner logging.LeveledLogger
}

// noisyError returns true for messages we want to drop at Error level.
func noisyError(msg string) bool {
	return strings.Contains(msg, "Simulcast probing") ||
		strings.Contains(msg, "Incoming unhandled RTP ssrc")
}

func (l *filteringLogger) Error(msg string) {
	if noisyError(msg) {
		return
	}
	l.inner.Error(msg)
}

func (l *filteringLogger) Errorf(format string, args ...any) {
	if strings.Contains(format, "Simulcast probing") ||
		strings.Contains(format, "Incoming unhandled RTP ssrc") {
		return
	}
	l.inner.Errorf(format, args...)
}

func (l *filteringLogger) Trace(msg string)                  { l.inner.Trace(msg) }
func (l *filteringLogger) Tracef(format string, args ...any) { l.inner.Tracef(format, args...) }
func (l *filteringLogger) Debug(msg string)                  { l.inner.Debug(msg) }
func (l *filteringLogger) Debugf(format string, args ...any) { l.inner.Debugf(format, args...) }
func (l *filteringLogger) Info(msg string)                   { l.inner.Info(msg) }
func (l *filteringLogger) Infof(format string, args ...any)  { l.inner.Infof(format, args...) }
func (l *filteringLogger) Warn(msg string)                   { l.inner.Warn(msg) }
func (l *filteringLogger) Warnf(format string, args ...any)  { l.inner.Warnf(format, args...) }
