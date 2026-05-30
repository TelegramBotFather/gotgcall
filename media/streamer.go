package media

import (
	"context"
	"errors"
	stdio "io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

// Writer consumes encoded samples. Implemented by pion's TrackLocalStaticSample
// and by test fakes.
type Writer interface {
	WriteSample(s media.Sample) error
}

// Streamer pulls Samples from a FrameReader at the sample's natural cadence
// and pushes them to a Writer. Mute (audio) skips WriteSample but keeps
// the clock advancing.
type Streamer struct {
	src    FrameReader
	writer Writer

	ctx context.Context
	log *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	onEnd  func(err error)
	msSent atomic.Uint64

	once sync.Once

	muted atomic.Bool
}

func NewStreamer(parent context.Context, src FrameReader, writer Writer, log *slog.Logger, onEnd func(error)) *Streamer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(parent)
	return &Streamer{
		src:    src,
		writer: writer,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		onEnd:  onEnd,
	}
}

// Start kicks off the pacing goroutine. Returns immediately.
func (s *Streamer) Start() { go s.run() }

func (s *Streamer) run() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("streamer: panic", slog.Any("recover", r))
			s.fireEnd(panicErr{v: r})
		}
	}()

	next := time.Now()
	for {
		if err := s.ctx.Err(); err != nil {
			s.fireEnd(err)
			return
		}
		sample, err := s.src.Next(s.ctx)
		if err != nil {
			s.fireEnd(err)
			return
		}
		if !s.muted.Load() {
			if writeErr := s.writer.WriteSample(sample); writeErr != nil && !errors.Is(writeErr, stdio.ErrClosedPipe) {
				s.log.Debug("streamer: write sample failed", slog.Any("err", writeErr))
				s.fireEnd(writeErr)
				return
			}
		}
		s.msSent.Add(uint64(sample.Duration / time.Millisecond))

		next = next.Add(sample.Duration)
		wait := time.Until(next)
		if wait < -100*time.Millisecond {
			next = time.Now()
			continue
		}
		if wait <= 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-s.ctx.Done():
			t.Stop()
			s.fireEnd(s.ctx.Err())
			return
		}
	}
}

func (s *Streamer) fireEnd(err error) {
	if s.onEnd == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	s.onEnd(err)
}

// Stop cancels the run loop and closes the underlying frame reader. Safe
// to call from any goroutine. Blocks until the run loop exits.
func (s *Streamer) Stop() {
	s.once.Do(func() {
		s.cancel()
		_ = s.src.Close()
	})
	<-s.done
}

// Done is closed when the streamer has finished (EOF, error, or Stop).
func (s *Streamer) Done() <-chan struct{} { return s.done }

func (s *Streamer) SetMuted(m bool) { s.muted.Store(m) }
func (s *Streamer) Muted() bool     { return s.muted.Load() }

// ElapsedMs returns cumulative ms of samples handed to the pacing loop.
func (s *Streamer) ElapsedMs() uint64 { return s.msSent.Load() }

type panicErr struct{ v any }

func (p panicErr) Error() string { return "streamer panic" }
