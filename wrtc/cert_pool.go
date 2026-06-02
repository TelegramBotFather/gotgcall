package wrtc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// CertPool pre-generates DTLS certificates so a burst of CreateCall
// requests does not stall the caller behind serial key generation.
type CertPool struct {
	ch     chan *webrtc.Certificate
	closed chan struct{}
	log    *slog.Logger
	wg     sync.WaitGroup
}

// NewCertPool starts a background goroutine that keeps `size` certificates
// available. size <= 0 disables the pool (Take falls back to generating
// on demand).
func NewCertPool(size int, log *slog.Logger) *CertPool {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &CertPool{closed: make(chan struct{}), log: log}
	if size <= 0 {
		return p
	}
	p.ch = make(chan *webrtc.Certificate, size)
	p.wg.Add(1)
	go p.refill()
	return p
}

func (p *CertPool) refill() {
	defer p.wg.Done()
	// errBackoff prevents a tight spin if generateECDSACert fails persistently
	// (entropy exhaustion, syscall pressure). Starts at 100 ms, caps at 5 s.
	const minBackoff, maxBackoff = 100 * time.Millisecond, 5 * time.Second
	backoff := minBackoff
	for {
		select {
		case <-p.closed:
			return
		default:
		}
		cert, err := generateECDSACert()
		if err != nil {
			p.log.Warn("certpool: generation failed", slog.Any("err", err), slog.Duration("backoff", backoff))
			select {
			case <-time.After(backoff):
			case <-p.closed:
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = minBackoff // reset on success
		select {
		case p.ch <- cert:
		case <-p.closed:
			return
		}
	}
}

// Take returns a pre-generated certificate. If the pool is disabled or
// the background producer hasn't caught up, generates a fresh one inline
// under ctx.
func (p *CertPool) Take(ctx context.Context) (*webrtc.Certificate, error) {
	if p.ch != nil {
		select {
		case c := <-p.ch:
			if c != nil {
				return c, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	return generateECDSACert()
}

// Close stops the background refill loop. Already-buffered certificates
// are released.
func (p *CertPool) Close() {
	select {
	case <-p.closed:
		return
	default:
		close(p.closed)
	}
	p.wg.Wait()
}

func generateECDSACert() (*webrtc.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	c, err := webrtc.GenerateCertificate(key)
	if err != nil {
		return nil, err
	}
	return c, nil
}
