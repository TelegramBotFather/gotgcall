package media

import (
	"context"
	stdio "io"
)

// Loop repeats src forever (until the caller cancels or stops the call).
// Pre-encoded passthrough sources cannot loop since their reader is
// consumed once; wrap a FromFile/FromURL/FromReader instead.
func Loop(src Source) Source {
	return &seqSource{tracks: src.Tracks(), next: func() Source { return src }}
}

// Concat plays sources back to back. All sources should advertise the
// same tracks; mixing audio-only with audio+video sources mid-chain is
// not supported.
func Concat(srcs ...Source) Source {
	var tracks Track
	for _, s := range srcs {
		tracks |= s.Tracks()
	}
	i := 0
	next := func() Source {
		if i >= len(srcs) {
			return nil
		}
		s := srcs[i]
		i++
		return s
	}
	return &seqSource{tracks: tracks, next: next}
}

// seqSource concatenates streams from successive Sources behind one set
// of Reader handles. The chainReader handles ffmpeg-process lifecycle
// across boundaries so consumers see a single seamless stream.
type seqSource struct {
	next   func() Source
	tracks Track
}

func (s *seqSource) Tracks() Track { return s.tracks }

func (s *seqSource) Open(ctx context.Context) (*Streams, error) {
	first := s.next()
	if first == nil {
		return &Streams{}, nil
	}
	st, err := first.Open(ctx)
	if err != nil {
		return nil, err
	}
	cr := &chainReader{ctx: ctx, next: s.next, cur: st}
	out := &Streams{close: cr.close}
	if s.tracks.Has(TrackAudio) {
		out.Audio = &trackChainReader{chain: cr, pick: pickAudio}
	}
	if s.tracks.Has(TrackVideo) {
		out.Video = &trackChainReader{chain: cr, pick: pickVideo}
	}
	return out, nil
}

func pickAudio(st *Streams) stdio.Reader { return st.Audio }
func pickVideo(st *Streams) stdio.Reader { return st.Video }

type chainReader struct {
	ctx  context.Context
	next func() Source
	cur  *Streams
}

func (c *chainReader) advance() bool {
	if c.cur != nil {
		_ = c.cur.Close()
	}
	src := c.next()
	if src == nil {
		c.cur = nil
		return false
	}
	st, err := src.Open(c.ctx)
	if err != nil {
		c.cur = nil
		return false
	}
	c.cur = st
	return true
}

func (c *chainReader) close() error {
	if c.cur != nil {
		return c.cur.Close()
	}
	return nil
}

type trackChainReader struct {
	chain *chainReader
	pick  func(*Streams) stdio.Reader
}

func (t *trackChainReader) Read(p []byte) (int, error) {
	for {
		if t.chain.cur == nil {
			return 0, stdio.EOF
		}
		r := t.pick(t.chain.cur)
		if r == nil {
			if !t.chain.advance() {
				return 0, stdio.EOF
			}
			continue
		}
		n, err := r.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == stdio.EOF {
			if !t.chain.advance() {
				return 0, stdio.EOF
			}
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}
