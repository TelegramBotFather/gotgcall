package utils

import (
	"log/slog"
	"sync"
)

// Dispatcher serializes callback invocations onto a single goroutine so
// callers can safely re-enter the API from inside a callback without
// deadlocking against locks held by the goroutine that produced the event.
type Dispatcher struct {
	ch     chan func()
	closed chan struct{}
	log    *slog.Logger
	wg     sync.WaitGroup
	once   sync.Once
}

func NewDispatcher(bufSize int, log *slog.Logger) *Dispatcher {
	if bufSize <= 0 {
		bufSize = 256
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Dispatcher{
		ch:     make(chan func(), bufSize),
		closed: make(chan struct{}),
		log:    log,
	}
	d.wg.Add(1)
	go d.loop()
	return d
}

func (d *Dispatcher) loop() {
	defer d.wg.Done()
	for fn := range d.ch {
		func() {
			defer func() {
				if r := recover(); r != nil {
					d.log.Error("dispatcher: callback panic", slog.Any("recover", r))
				}
			}()
			fn()
		}()
	}
}

// Submit enqueues fn for execution on the dispatcher goroutine. If the
// queue is full, the oldest queued event is dropped to make room for the
// new one; this prevents a slow user callback from stalling producers.
// Submit never blocks.
func (d *Dispatcher) Submit(fn func()) {
	if fn == nil {
		return
	}
	select {
	case <-d.closed:
		return
	default:
	}
	select {
	case d.ch <- fn:
	default:
		// Queue full. Drop oldest, retry once.
		select {
		case <-d.ch:
			d.log.Warn("dispatcher: queue full, dropped oldest event")
		default:
		}
		select {
		case d.ch <- fn:
		default:
			d.log.Warn("dispatcher: queue still full, dropped new event")
		}
	}
}

// Close stops the dispatcher and waits for the loop goroutine to drain
// any pending events.
func (d *Dispatcher) Close() {
	d.once.Do(func() {
		close(d.closed)
		close(d.ch)
	})
	d.wg.Wait()
}
