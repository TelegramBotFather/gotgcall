package media

import (
	"context"
	"errors"
	"fmt"
	stdio "io"
	"os/exec"
	"sync"
	"sync/atomic"

	"gotgcall/models"
	"gotgcall/utils"
)

// stdinShellReader is a minimal exec.Cmd wrapper used by transcodeSource
// when the input is supplied via stdin (FromReader / FromRawPCM / FromRawVideo).
// Mirrors io.ShellReader's surface but also wires Cmd.Stdin.
type stdinShellReader struct {
	cmd     *exec.Cmd
	stdout  stdio.ReadCloser
	stderr  *utils.RingBuffer
	cancel  context.CancelFunc
	waitErr atomic.Pointer[error]
	done    chan struct{}
	once    sync.Once
}

func newStdinShellReader(parent context.Context, program string, args []string, stdin stdio.Reader) (*stdinShellReader, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: stdout pipe: %v", models.ErrFFmpegSpawn, err)
	}
	stderr := utils.NewRingBuffer(4096)
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", models.ErrFFmpegSpawn, err)
	}
	r := &stdinShellReader{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go r.reap()
	return r, nil
}

func (r *stdinShellReader) reap() {
	defer close(r.done)
	err := r.cmd.Wait()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			tail := r.stderr.Snapshot()
			err = fmt.Errorf("%w: exit=%d stderr=%q",
				models.ErrFFmpegCrashed, exitErr.ExitCode(), trimTail(tail, 512))
		}
		r.waitErr.Store(&err)
	}
}

func trimTail(b []byte, max int) string {
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

func (r *stdinShellReader) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	if err != nil {
		if w := r.waitErr.Load(); w != nil {
			return n, *w
		}
	}
	return n, err
}

func (r *stdinShellReader) Close() error {
	r.once.Do(func() { r.cancel() })
	<-r.done
	if w := r.waitErr.Load(); w != nil {
		err := *w
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
