package wrtc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Tunables for the FactoryMonitor. The keepalive cadence is short enough
// that Telegram's SFU never sees a long enough "no-video" gap to GC the
// SSRC binding, and long enough that the bandwidth overhead is negligible
// (one padding packet per ~60 real VP8 frames at 30 fps). The liveness
// timeout is conservative — pion's own ICE failed timer is 120 s by
// default, so 60 s gives a faster escape hatch for the "ICE keepalive
// succeeds but RTP forwarding died" failure mode without false-tripping
// during normal jitter.
const (
	keepaliveTickInterval = 2 * time.Second
	livenessTimeout       = 60 * time.Second
	monitorPollInterval   = time.Second
	// iceCheckingTimeout is the maximum time ICE may remain in Checking
	// before the monitor force-closes the PC. Telegram's STUN servers
	// typically respond in 1-3 s, but cross-DC rejoins and busy SFU
	// edges can push ICE negotiation to 5-8 s under real traffic.
	iceCheckingTimeout = 10 * time.Second
)

// FactoryMonitor is a SINGLE long-running goroutine, shared across every
// PeerConnection a Factory has ever produced, that performs two duties
// for each registered PC per tick:
//
//  1. **Video keepalive**: every keepaliveTickInterval, write one RTP
//     padding packet on each PC's video TrackLocalStaticSample. Padding
//     carries the video SSRC and PT but no decodable VP8 payload —
//     receivers strip the padding before passing the empty payload to
//     the decoder. Effect: Telegram's SFU keeps the video SSRC binding
//     warm, so a /vplay arriving long after /play on the SAME PC has
//     its VP8 frames actually forwarded instead of dropped against a
//     GC'd binding.
//
//  2. **Liveness watchdog**: sample pion's selected ICE candidate-pair
//     BytesReceived via GetStats. If pion reports the PC Connected but
//     BytesReceived has not increased for livenessTimeout, force-close
//     the PC so the caller sees a Failed transition and can rejoin.
//     Catches "ICE keepalive succeeds but SFU stopped forwarding" cases
//     that pion's own state timers miss (those are bound to the ICE
//     state machine, not actual media flow).
//
// The shared-goroutine design replaces the v0.6.5 first draft where
// every PC owned its own ticker goroutine. With 100+ concurrent calls
// per Client, that meant 100+ idle tickers; the consolidated form has
// exactly ONE goroutine per Factory regardless of call count. Per-PC
// state lives in pcMonitorEntry and is accessed via per-entry atomics,
// so the tick loop never holds the registry lock across GetStats
// (which is potentially expensive on busy candidate-pair sets).
type FactoryMonitor struct {
	log     *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	entries map[*PeerConnection]*pcMonitorEntry
	mu      sync.RWMutex
	started atomic.Bool
	stopped atomic.Bool
}

// NewFactoryMonitor constructs a stopped monitor. Call Start exactly
// once (typically from NewFactory).
func NewFactoryMonitor(log *slog.Logger) *FactoryMonitor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &FactoryMonitor{
		log:     log,
		ctx:     ctx,
		cancel:  cancel,
		entries: make(map[*PeerConnection]*pcMonitorEntry),
	}
}

func (m *FactoryMonitor) Start() {
	if m == nil {
		return
	}
	if m.started.CompareAndSwap(false, true) {
		go m.run()
	}
}

func (m *FactoryMonitor) Stop() {
	if m == nil {
		return
	}
	if m.stopped.CompareAndSwap(false, true) {
		m.cancel()
	}
}

// Register adds pc to the monitor's working set. Idempotent. Called from
// NewPeerConnection. The pcMonitorEntry's lastBytes / lastProgressNs
// reset themselves whenever the PC isn't Connected, so re-Registering
// after a PC bounce isn't needed (and we don't).
func (m *FactoryMonitor) Register(pc *PeerConnection) {
	if m == nil || pc == nil {
		return
	}
	entry := &pcMonitorEntry{pc: pc.pc, video: pc.video, log: pc.log}
	m.mu.Lock()
	m.entries[pc] = entry
	m.mu.Unlock()
}

// Unregister removes pc from the monitor's working set. Idempotent.
// Called from PeerConnection.Close.
func (m *FactoryMonitor) Unregister(pc *PeerConnection) {
	if m == nil || pc == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, pc)
	m.mu.Unlock()
}

func (m *FactoryMonitor) run() {
	t := time.NewTicker(monitorPollInterval)
	defer t.Stop()
	var tick uint64
	// Pre-allocate; grows as needed but typically stays at the high-water
	// mark of concurrent calls, avoiding per-tick allocation.
	var snapshot []*pcMonitorEntry
	keepaliveEvery := uint64(keepaliveTickInterval / monitorPollInterval)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			tick++
			doKeepalive := tick%keepaliveEvery == 0
			// Snapshot the registry under a short RLock, then iterate
			// outside the lock — GetStats can take hundreds of µs per
			// PC and we don't want to block Register/Unregister callers
			// (NewPeerConnection / Close) for the whole pass.
			m.mu.RLock()
			if cap(snapshot) < len(m.entries) {
				snapshot = make([]*pcMonitorEntry, 0, len(m.entries))
			}
			snapshot = snapshot[:0]
			for _, e := range m.entries {
				snapshot = append(snapshot, e)
			}
			m.mu.RUnlock()
			for _, e := range snapshot {
				e.tick(doKeepalive)
			}
		}
	}
}

// pcMonitorEntry is one PC's per-tick state. lastBytes / lastProgressNs
// are atomics so the tick loop reads them without a per-entry mutex.
// The pc / video / log pointers are written once at Register and never
// mutate.
type pcMonitorEntry struct {
	pc             *webrtc.PeerConnection
	video          *webrtc.TrackLocalStaticSample
	log            *slog.Logger
	lastBytes      atomic.Uint64
	lastProgressNs atomic.Int64
	checkingNs     atomic.Int64 // monotonic UnixNano when Checking was first seen; 0 = not checking or already settled
}

func (e *pcMonitorEntry) tick(doKeepalive bool) {
	state := e.pc.ConnectionState()

	// ICE checking-stuck detection: if the PC has been in a non-Connected
	// state continuously for iceCheckingTimeout, the ICE negotiation is
	// stuck. Force-close so the caller sees Failed and can reconnect.
	if state == webrtc.PeerConnectionStateConnecting || state == webrtc.PeerConnectionStateNew {
		now := time.Now().UnixNano()
		if start := e.checkingNs.Load(); start == 0 {
			e.checkingNs.Store(now)
		} else if time.Duration(now-start) > iceCheckingTimeout {
			e.log.Warn("ICE stuck in Checking, forcing PC close",
				slog.Duration("timeout", iceCheckingTimeout))
			e.checkingNs.Store(-1) // prevent re-firing
			_ = e.pc.Close()
			return
		}
	} else {
		e.checkingNs.Store(0)
	}

	if state != webrtc.PeerConnectionStateConnected {
		e.lastBytes.Store(0)
		e.lastProgressNs.Store(0)
		return
	}
	e.checkLiveness()
	if doKeepalive && e.video != nil {
		if err := e.video.GeneratePadding(1); err != nil {
			e.log.Debug("video keepalive padding", slog.Any("err", err))
		}
	}
}

func (e *pcMonitorEntry) checkLiveness() {
	bytes := selectedPairBytesReceived(e.pc)
	if bytes == 0 {
		return
	}
	now := time.Now().UnixNano()
	prev := e.lastBytes.Load()
	if bytes != prev {
		e.lastBytes.Store(bytes)
		e.lastProgressNs.Store(now)
		return
	}
	progress := e.lastProgressNs.Load()
	if progress == 0 {
		e.lastProgressNs.Store(now)
		return
	}
	if age := time.Since(time.Unix(0, progress)); age > livenessTimeout {
		e.log.Warn("liveness: candidate-pair bytes stalled while Connected, forcing PC close",
			slog.Duration("age", age),
			slog.Duration("timeout", livenessTimeout))
		_ = e.pc.Close()
	}
}

// selectedPairBytesReceived returns the BytesReceived counter of the
// nominated ICE candidate pair, or 0 if none is selected yet. pion v4
// doesn't expose the selected pair directly — it's marked via Nominated
// on each pair's stats.
func selectedPairBytesReceived(pc *webrtc.PeerConnection) uint64 {
	for _, s := range pc.GetStats() {
		pair, ok := s.(webrtc.ICECandidatePairStats)
		if !ok || !pair.Nominated {
			continue
		}
		return pair.BytesReceived
	}
	return 0
}
