package io

import (
	"context"
	"errors"
	"fmt"
	stdio "io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
)

// ShellReader spawns a ffmpeg subprocess, exposes its stdout as a Reader,
// captures the tail of stderr in a fixed-size ring, and cleans up when
// the context is canceled or the process exits.
//
// ShellReader is safe to use across goroutines for Close and Err. Read
// must be serialized by a single consumer (the convention for io.Reader).
//
// We manage the stdout pipe ourselves (os.Pipe + cmd.Stdout = pw) rather
// than using cmd.StdoutPipe so that cmd.Wait does NOT close the read end
// out from under us. That's a documented stdlib gotcha — when Wait fires
// the moment the child exits, StdoutPipe closes the read end immediately,
// discarding any unread bytes in the kernel pipe buffer (i.e. the last
// chunk of audio the streamer hadn't paced through yet). With a manual
// pipe, reads drain naturally to io.EOF after the child's pw closes.
type ShellReader struct {
	stdout   *os.File
	cmd      *exec.Cmd
	stderr   *utils.RingBuffer
	cancel   context.CancelFunc
	waitErr  atomic.Pointer[error]
	doneCh   chan struct{}
	log      *slog.Logger
	onExit   func(error) // optional hook fired from reap after the process is fully reaped
	waitOnce sync.Once
}

// SetOnExit registers a callback invoked once the subprocess exits and the
// reap goroutine has captured its exit error. Used to eliminate the need
// for a separate watch goroutine in consumers like RTMPCall — instead of
// `go waitForExit(cmd)` you set OnExit and the existing reap goroutine
// dispatches the lifecycle event. Must be set BEFORE the first reap exit
// (i.e. immediately after NewShellReader returns).
func (r *ShellReader) SetOnExit(fn func(error)) {
	r.onExit = fn
}

// NewShellReader spawns program with args and starts the process. The
// returned reader streams stdout. If the program cannot be started,
// ErrFFmpegSpawn is returned wrapped. When streamStderr is true, ffmpeg's
// stderr is also tee'd line-by-line into the logger at Debug level — useful
// for diagnosing "ffmpeg runs, but I hear nothing" symptoms.
func NewShellReader(parent context.Context, program string, args []string, log *slog.Logger, streamStderr bool) (*ShellReader, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, program, args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: pipe: %v", models.ErrFFmpegSpawn, err)
	}
	cmd.Stdout = pw
	stderrRing := utils.NewRingBuffer(4096)
	if streamStderr {
		// Fan-out: ring buffer captures the tail for the exit error message,
		// stderrLineWriter logs each line at Debug level while the process runs.
		cmd.Stderr = stdio.MultiWriter(stderrRing, &stderrLineWriter{log: log, program: program})
	} else {
		cmd.Stderr = stderrRing
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("%w: %v", models.ErrFFmpegSpawn, err)
	}
	// Close our reference to the write end; the child has its own dup'd fd.
	// When the child eventually exits, the kernel write end fully closes and
	// our reads on pr drain the buffer than return io.EOF.
	_ = pw.Close()
	r := &ShellReader{
		cmd:    cmd,
		stdout: pr,
		stderr: stderrRing,
		cancel: cancel,
		doneCh: make(chan struct{}),
		log:    log,
	}
	go r.reap()
	return r, nil
}

// stderrLineWriter buffers ffmpeg's stderr until it sees a newline, then
// emits one Debug log per line. Cheaper than logging each Write call (which
// might contain a partial line) and produces readable output.
type stderrLineWriter struct {
	log     *slog.Logger
	program string
	pending []byte
}

func (w *stderrLineWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		idx := -1
		for i, b := range w.pending {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := w.pending[:idx]
		// Trim trailing \r for CRLF tolerance.
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		if len(line) > 0 {
			w.log.Debug("ffmpeg", "program", w.program, "stderr", string(line))
		}
		w.pending = w.pending[idx+1:]
	}
	// Cap pending growth to guard against pathological no-newline output.
	const maxPending = 8 * 1024
	if len(w.pending) > maxPending {
		w.pending = w.pending[len(w.pending)-maxPending:]
	}
	return len(p), nil
}

func (r *ShellReader) reap() {
	defer close(r.doneCh)
	// Recover so a panic inside cmd.Wait / stderr.Snapshot doesn't leave
	// r.doneCh unclosed — every blocked Close() caller would deadlock.
	defer func() {
		if v := recover(); v != nil {
			r.log.Error("shell_reader: reap panic", slog.Any("recover", v))
			r.waitErr.Store(new(fmt.Errorf("%w: reap panic: %v", models.ErrInternal, v)))
		}
		// Fire OnExit hook (if registered) after the process is fully reaped,
		// so RTMPCall and similar consumers can react without spawning an
		// additional watchEnd goroutine.
		if cb := r.onExit; cb != nil {
			exitErr := error(nil)
			if p := r.waitErr.Load(); p != nil {
				exitErr = *p
			}
			cb(exitErr)
		}
	}()
	err := r.cmd.Wait()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			tail := r.stderr.Snapshot()
			err = fmt.Errorf("%w: exit=%d stderr=%q",
				models.ErrFFmpegCrashed, exitErr.ExitCode(), trimTail(tail))
		}
		r.waitErr.Store(&err)
		r.log.Debug("shell_reader: process exited with error", slog.Any("err", err))
	}
}

func trimTail(b []byte) string {
	const maxShown = 512
	if len(b) > maxShown {
		b = b[len(b)-maxShown:]
	}
	return string(b)
}

// Read pulls bytes from the subprocess stdout.
func (r *ShellReader) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	if err != nil {
		if wErrPtr := r.waitErr.Load(); wErrPtr != nil {
			return n, *wErrPtr
		}
	}
	return n, err
}

// Close terminates the subprocess (if still running) and our copy of the
// read end of the pipe, then waits for the reaper. Closing the read end
// unblocks any pending Read in-flight by another goroutine.
func (r *ShellReader) Close() error {
	r.waitOnce.Do(func() {
		r.cancel()
		_ = r.stdout.Close()
	})
	<-r.doneCh
	if wErrPtr := r.waitErr.Load(); wErrPtr != nil {
		err := *wErrPtr
		// Cancellation produces a context.Canceled error which is not interesting.
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// Done is closed when the subprocess exits.
func (r *ShellReader) Done() <-chan struct{} { return r.doneCh }

// Err returns the process exit error (after Done is closed).
func (r *ShellReader) Err() error {
	if wErrPtr := r.waitErr.Load(); wErrPtr != nil {
		return *wErrPtr
	}
	return nil
}
