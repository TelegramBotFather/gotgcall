package native

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
)

// CertPool pre-generates self-signed DTLS certificates so a burst of
// CreateCall does not stall the caller behind serial keygen. The
// certificates carry an ECDSA-P256 key + a self-signed cert valid for a
// year; both are reused per group call (one cert per call).
type CertPool struct {
	ch     chan *tls.Certificate
	closed chan struct{}
	log    *slog.Logger
	wg     sync.WaitGroup
}

// NewCertPool starts a background goroutine that keeps size certificates
// available. size <= 0 disables the pool — Take generates inline.
func NewCertPool(size int, log *slog.Logger) *CertPool {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &CertPool{closed: make(chan struct{}), log: log}
	if size <= 0 {
		return p
	}
	p.ch = make(chan *tls.Certificate, size)
	p.wg.Add(1)
	go p.refill()
	return p
}

func (p *CertPool) refill() {
	defer p.wg.Done()
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
		backoff = minBackoff
		select {
		case p.ch <- cert:
		case <-p.closed:
			return
		}
	}
}

// Take returns a pre-generated certificate, or generates one inline if
// the pool is disabled / drained.
func (p *CertPool) Take(ctx context.Context) (*tls.Certificate, error) {
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

func (p *CertPool) Close() {
	select {
	case <-p.closed:
		return
	default:
		close(p.closed)
	}
	p.wg.Wait()
}

// generateECDSACert returns a fresh ECDSA-P256 self-signed certificate
// suitable for DTLS-SRTP. The cert is valid for 30 days — calls don't
// last that long, and ntgcalls regenerates per call too.
func generateECDSACert() (*tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ecdsa keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gotgcall"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("x509 create: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("x509 parse: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

// CertFingerprintSHA256 returns the RFC 4572 SHA-256 fingerprint string
// (colon-separated upper-case hex) that goes in the JoinGroupCall payload.
func CertFingerprintSHA256(cert *tls.Certificate) (string, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return "", fmt.Errorf("empty certificate")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	hexStr := hex.EncodeToString(sum[:])
	var b strings.Builder
	b.Grow(len(hexStr) + len(hexStr)/2)
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strings.ToUpper(hexStr[i : i+2]))
	}
	return b.String(), nil
}
