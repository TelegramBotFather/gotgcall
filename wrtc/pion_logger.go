package wrtc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pion/logging"
)

// pionScopeKey is the slog attribute key used to tag pion-internal log lines
// with their originating scope ("ice", "dtls", "sctp", "interceptor", etc.).
// Filtering by `pion=ice` etc. in your slog handler lets you focus on one
// subsystem without drowning in the others.
const pionScopeKey = "pion"

// slogPionFactory is a pion logging.LoggerFactory that bridges all pion-
// internal log output into the user-supplied slog.Logger. Without this
// bridge, pion writes to its own default logger (stderr via the `log`
// package) and gotgcall.WithLogger has no effect on pion's output — which
// is the single biggest "debug logs aren't working" complaint.
//
// Levels map straight through:
//
//	pion Trace → slog LevelDebug-4  (sub-debug; off unless you set traceAsDebug)
//	pion Debug → slog LevelDebug
//	pion Info  → slog LevelInfo
//	pion Warn  → slog LevelWarn
//	pion Error → slog LevelError
//
// When traceAsDebug is true (via FactoryOptions.PionTraceAsDebug, surfaced
// as gotgcall.WithPionTraceLogs()), pion Trace is remapped to LevelDebug so
// standard Debug-level handlers see ICE per-check, per-candidate, and
// per-binding-request lines — the data you actually need when ICE is stuck
// in Checking and you want to know which candidate pairs are failing.
type slogPionFactory struct {
	log          *slog.Logger
	traceAsDebug bool
}

func newSlogPionFactory(log *slog.Logger, traceAsDebug bool) logging.LoggerFactory {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &slogPionFactory{log: log, traceAsDebug: traceAsDebug}
}

func (f *slogPionFactory) NewLogger(scope string) logging.LeveledLogger {
	return &filteringLogger{
		inner: &slogLeveled{log: f.log, scope: scope, traceAsDebug: f.traceAsDebug},
	}
}

// slogLeveled is a pion LeveledLogger that forwards to slog.
type slogLeveled struct {
	log          *slog.Logger
	scope        string
	traceAsDebug bool
}

// pionTraceLevel sits below slog.LevelDebug so debug-level handlers don't
// see pion's trace spam by default.
const pionTraceLevel = slog.LevelDebug - 4

func (l *slogLeveled) traceLevel() slog.Level {
	if l.traceAsDebug {
		return slog.LevelDebug
	}
	return pionTraceLevel
}

func (l *slogLeveled) logf(level slog.Level, msg string) {
	if !l.log.Enabled(context.Background(), level) {
		return
	}
	l.log.LogAttrs(context.Background(), level, msg, slog.String(pionScopeKey, l.scope))
}

func (l *slogLeveled) Trace(msg string)                  { l.logf(l.traceLevel(), msg) }
func (l *slogLeveled) Tracef(format string, args ...any) { l.logf(l.traceLevel(), sprintfLazy(format, args)) }
func (l *slogLeveled) Debug(msg string)                  { l.logf(slog.LevelDebug, msg) }
func (l *slogLeveled) Debugf(format string, args ...any) { l.logf(slog.LevelDebug, sprintfLazy(format, args)) }
func (l *slogLeveled) Info(msg string)                   { l.logf(slog.LevelInfo, msg) }
func (l *slogLeveled) Infof(format string, args ...any)  { l.logf(slog.LevelInfo, sprintfLazy(format, args)) }
func (l *slogLeveled) Warn(msg string)                   { l.logf(slog.LevelWarn, msg) }
func (l *slogLeveled) Warnf(format string, args ...any)  { l.logf(slog.LevelWarn, sprintfLazy(format, args)) }
func (l *slogLeveled) Error(msg string)                  { l.logf(slog.LevelError, msg) }
func (l *slogLeveled) Errorf(format string, args ...any) { l.logf(slog.LevelError, sprintfLazy(format, args)) }

// sprintfLazy skips Sprintf when there are no args. logf already gated by
// Enabled() so the format cost is only paid when the level is enabled.
func sprintfLazy(format string, args []any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// filteringLogger wraps a LeveledLogger and silences a curated list of
// error/warn messages that are noise in the Telegram group-call scenario.
// Telegram's mixer forwards every other participant's RTP to us; our
// PeerConnection has only send-only tracks, so pion logs "Simulcast probing
// failed" / "Incoming unhandled RTP ssrc" for each unknown incoming SSRC.
// Useless to operators.
type filteringLogger struct {
	inner logging.LeveledLogger
}

func noisy(msg string) bool {
	return strings.Contains(msg, "Simulcast probing") ||
		strings.Contains(msg, "Incoming unhandled RTP ssrc") ||
		strings.Contains(msg, "stream is already closed") ||
		strings.Contains(msg, "DTLS transport has not started yet") ||
		strings.Contains(msg, "connecting canceled by caller") ||
		strings.Contains(msg, "the agent is closed") ||
		strings.Contains(msg, "without candidate pairs")
}

func (l *filteringLogger) Trace(msg string)                  { l.inner.Trace(msg) }
func (l *filteringLogger) Tracef(format string, args ...any) { l.inner.Tracef(format, args...) }
func (l *filteringLogger) Debug(msg string)                  { l.inner.Debug(msg) }
func (l *filteringLogger) Debugf(format string, args ...any) { l.inner.Debugf(format, args...) }
func (l *filteringLogger) Info(msg string)                   { l.inner.Info(msg) }
func (l *filteringLogger) Infof(format string, args ...any)  { l.inner.Infof(format, args...) }
func (l *filteringLogger) Warn(msg string) {
	if noisy(msg) {
		return
	}
	l.inner.Warn(msg)
}
func (l *filteringLogger) Warnf(format string, args ...any) {
	if noisy(format) || noisy(sprintfLazy(format, args)) {
		return
	}
	l.inner.Warnf(format, args...)
}
func (l *filteringLogger) Error(msg string) {
	if noisy(msg) {
		return
	}
	l.inner.Error(msg)
}
func (l *filteringLogger) Errorf(format string, args ...any) {
	if noisy(format) || noisy(sprintfLazy(format, args)) {
		return
	}
	l.inner.Errorf(format, args...)
}
