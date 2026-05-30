package utils

import "sync"

// RingBuffer is a fixed-capacity byte ring used to capture the tail of
// a stream (e.g. ffmpeg stderr) without growing without bound.
type RingBuffer struct {
	buf  []byte
	head int
	mu   sync.Mutex
	full bool
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 4096
	}
	return &RingBuffer{buf: make([]byte, capacity)}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	capN := len(r.buf)
	if n >= capN {
		copy(r.buf, p[n-capN:])
		r.head = 0
		r.full = true
		return n, nil
	}
	written := copy(r.buf[r.head:], p)
	if written < n {
		copy(r.buf, p[written:])
	}
	r.head = (r.head + n) % capN
	if !r.full && r.head < n {
		r.full = true
	}
	return n, nil
}

// Snapshot returns a copy of the buffered tail bytes in order.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.head)
		copy(out, r.buf[:r.head])
		return out
	}
	out := make([]byte, len(r.buf))
	copy(out, r.buf[r.head:])
	copy(out[len(r.buf)-r.head:], r.buf[:r.head])
	return out
}
