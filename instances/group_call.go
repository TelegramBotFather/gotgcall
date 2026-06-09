package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
	"github.com/annihilatorrrr/gotgcall/wrtc"
)

// GroupCallEvents is the set of callbacks a GroupCall fires through the
// shared dispatcher; the Client wires these to its public OnXxx callbacks.
type GroupCallEvents struct {
	OnStreamEnd        func(t models.StreamType, d models.Device, err error)
	OnConnectionChange func(info models.NetworkInfo)
	// OnUpgrade fires on every outgoing state change that flips a
	// MediaState bit: Pause / Resume / Mute / Unmute (each only when the
	// state actually changed — no-op toggles stay silent), plus PC
	// Failed/Closed while video was active. Stream EOF (audio or video)
	// does not fire — OnStreamEnd already signals that. SetSource and
	// Stop also stay silent: the caller chose the new source / brought
	// the call down and can drive MTProto flags directly in the same
	// code path.
	// Mirror of ntgcalls' onUpgrade(MediaState) pattern.
	OnUpgrade func(state models.MediaState)
}

// GroupCall is the WebRTC call instance for one chat.
type GroupCall struct {
	ev GroupCallEvents

	src            media.Source
	pc             *wrtc.PeerConnection
	log            *slog.Logger
	disp           *utils.Dispatcher
	streams        *media.Streams
	audioStr       *media.Streamer
	videoStr       *media.Streamer
	connected      chan struct{}
	srcEncOpt      media.EncodeOptions
	chatID         int64
	connectTimeout time.Duration
	resumeMs       uint64 // seek offset captured on Pause; injected via SeekableSource.OpenAt on Resume

	mu            sync.RWMutex
	connectedOnce sync.Once
	netState      atomic.Int32 // models.ConnState
	closed        atomic.Bool
	switching     atomic.Bool // true while SetSource is replacing the source; suppresses OnStreamEnd for the old streamer
	connectCalled atomic.Bool
	paused        bool
	muted         bool
	videoOff      bool
	// Per-cycle stream-end state. Reset by SetSource / startLocked.
	// endedOnce ensures only the first leg to EOF fires OnStreamEnd; the
	// second leg is force-stopped and its EOF suppressed. hadAudio /
	// hadVideo are captured at cycle-start so the user gets the right
	// events even after fields are cleared.
	endedOnce bool
	hadAudio  bool
	hadVideo  bool
}

// NewGroupCall constructs a fresh call. Caller threads pion factory + logger.
// connectTimeout controls how long SetSource waits for ICE+DTLS; 0 = 10s
// (matches ntgcalls' internal call_interface.cpp:138 timeout).
func NewGroupCall(chatID int64, factory *wrtc.Factory, disp *utils.Dispatcher, log *slog.Logger, connectTimeout time.Duration, ev GroupCallEvents) (*GroupCall, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	pc, err := wrtc.NewPeerConnection(factory, log)
	if err != nil {
		return nil, err
	}
	gc := &GroupCall{
		chatID:         chatID,
		pc:             pc,
		log:            log.With(slog.Int64("chat", chatID)),
		disp:           disp,
		ev:             ev,
		connected:      make(chan struct{}),
		connectTimeout: connectTimeout,
	}
	gc.netState.Store(int32(models.Connecting))
	var wasConnected atomic.Bool
	pc.OnConnectionStateChange(func(s models.ConnState) {
		gc.netState.Store(int32(s))
		if s == models.Connected || s == models.Failed || s == models.Closed {
			gc.connectedOnce.Do(func() { close(gc.connected) })
		}
		if s == models.Connected {
			wasConnected.Store(true)
		}

		// Disconnected is a transient ICE state pion can recover from.
		// Streamers keep running so audio resumes when the connection
		// recovers. Suppress the user callback (ntgcalls does the same —
		// it silently logs "Reconnecting" and returns without notifying).
		if s == models.Disconnected {
			gc.log.Debug("ICE disconnected, waiting for recovery")
			return
		}
		// Connecting after already-connected is a transient reconnection
		// attempt. Suppress the user callback to avoid false-alarm churn.
		if s == models.Connecting && wasConnected.Load() {
			gc.log.Debug("ICE reconnecting")
			return
		}

		// When pion declares the PC Failed or Closed, the underlying
		// transport is gone. Tear streamers down so we stop burning CPU.
		if (s == models.Failed || s == models.Closed) && gc.disp != nil {
			gc.disp.Submit(func() {
				gc.mu.Lock()
				prev := gc.currentStateLocked()
				gc.stopStreamersLocked()
				gc.fireUpgradeIfChangedLocked(prev)
				gc.mu.Unlock()
			})
		}
		if gc.disp != nil && gc.ev.OnConnectionChange != nil {
			gc.disp.Submit(func() { gc.ev.OnConnectionChange(models.NetworkInfo{State: s}) })
		}
	})
	return gc, nil
}

func (*GroupCall) Mode() string { return "webrtc" }

func (g *GroupCall) CreateLocalParams() (string, error) {
	if g.closed.Load() {
		return "", models.ErrClosed
	}
	return g.pc.LocalParams()
}

func (g *GroupCall) Connect(remoteJSON string) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.connectCalled.Store(true)
	g.log.Debug("Connect: setting remote description")
	return g.pc.Connect(remoteJSON)
}

func (g *GroupCall) SetSource(ctx context.Context, src media.Source) error {
	if g.closed.Load() {
		return models.ErrClosed
	}

	// Source-owned encode opts. FromShell sources don't expose them — the
	// prepared video streamer falls back to 30 FPS.
	srcEncOpt := media.EncodeOptions{}
	if sp, ok := src.(media.SourcePath); ok {
		srcEncOpt = sp.EncodeOpts()
	}

	// Snapshot the state needed for the prepare phase. We RLock briefly
	// just for the read; the actual swap happens under a write Lock in
	// phase 2.
	g.mu.RLock()
	muted := g.muted
	videoOff := g.videoOff
	g.mu.RUnlock()

	// Phase 1 (OUTSIDE g.mu): spawn ffmpeg + read OGG/IVF headers. The
	// ivfreader/oggreader header parse blocks reading from ffmpeg's pipe
	// until ffmpeg flushes its first page (200 ms-1 s on cold start), and
	// holding g.mu across that window stalls every concurrent Pause /
	// Mute / GetState / ElapsedMs caller for the same call.
	streams, audioStr, videoStr, err := g.prepareStreamers(ctx, src, srcEncOpt, muted, videoOff)
	if err != nil {
		return err
	}

	// Gate: wait for WebRTC to reach Connected before starting streamers.
	// Samples written before ICE+DTLS completes are silently dropped by
	// pion (no SRTP binding yet), causing silence on first play. The
	// channel also closes on Failed/Closed so Stop() during the wait
	// doesn't hang for the full timeout.
	if models.ConnState(g.netState.Load()) != models.Connected {
		connectTimer := time.NewTimer(g.connectTimeout)
		select {
		case <-g.connected:
			connectTimer.Stop()
		case <-connectTimer.C:
			_ = streams.Close()
			if !g.connectCalled.Load() {
				return fmt.Errorf("%w: timed out waiting for WebRTC — Connect() was never called", models.ErrNotConnected)
			}
			state := models.ConnState(g.netState.Load())
			g.log.Warn("connect gate timed out",
				slog.String("state", state.String()),
				slog.Duration("timeout", g.connectTimeout),
				slog.String("hint", "enable WithDebugLogs()+WithICECandidateLogs() to see candidate exchange details"))
			return fmt.Errorf("%w: ICE/DTLS did not reach Connected within %s (stuck in %s)", models.ErrNotConnected, g.connectTimeout, state)
		case <-ctx.Done():
			connectTimer.Stop()
			_ = streams.Close()
			return ctx.Err()
		}
		if state := models.ConnState(g.netState.Load()); state != models.Connected {
			_ = streams.Close()
			return fmt.Errorf("%w: connection %s during setup", models.ErrConnectionFailed, state)
		}
	}

	// Phase 2 (UNDER g.mu): tear down old streamers, install new ones,
	// start them under the lock so any concurrent Stop/Pause sees a
	// coherent state.
	g.mu.Lock()

	if g.closed.Load() {
		g.mu.Unlock()
		// Streamers prepared but never Started — close the streams
		// (which kills ffmpeg). Streamer.Stop would deadlock because
		// it waits on a done chan that's only closed by the never-spawned run goroutine.
		_ = streams.Close()
		return models.ErrClosed
	}

	g.switching.Store(true)
	defer g.switching.Store(false)

	g.stopStreamersLocked()
	g.src = src
	g.resumeMs = 0
	g.srcEncOpt = srcEncOpt
	g.streams = streams
	g.audioStr = audioStr
	g.videoStr = videoStr
	// Arm the per-cycle stream-end guard for the freshly installed pair.
	g.endedOnce = false
	g.hadAudio = audioStr != nil
	g.hadVideo = videoStr != nil
	paused := g.paused
	if audioStr != nil {
		if paused {
			audioStr.SetPaused(true)
		}
		audioStr.Start()
	}
	if videoStr != nil {
		if paused {
			videoStr.SetPaused(true)
		}
		videoStr.Start()
	}
	g.mu.Unlock()
	// Intentionally no OnUpgrade fire here — SetSource is user-initiated,
	// the caller already knows they're switching to a video source (same
	// principle as Stop). Spontaneous transitions (video leg EOF, ICE
	// failure) still fire.
	return nil
}

// prepareStreamers spawns the source's ffmpeg leg(s) and constructs (but
// does NOT Start) the Streamers. Runs outside g.mu so the ffmpeg-header
// wait doesn't block other concurrent operations on the same call.
func (g *GroupCall) prepareStreamers(ctx context.Context, src media.Source, encOpt media.EncodeOptions, muted, videoOff bool) (*media.Streams, *media.Streamer, *media.Streamer, error) {
	streams, err := src.Open(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open source: %w", err)
	}
	var audioStr, videoStr *media.Streamer
	if streams.Audio != nil {
		fr, frErr := media.NewOpusFrameReader(streams.Audio)
		if frErr != nil {
			// Promoted to Warn so default-Info logging surfaces "/play
			// silently played no audio" without users having to flip to
			// Debug. The video leg (if any) still plays.
			g.log.Warn("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Audio, models.Microphone, endErr, s)
			})
			s.SetMuted(muted)
			audioStr = s
		}
	}
	if streams.Video != nil {
		fps := encOpt.VideoFPS
		if fps <= 0 {
			fps = 30
		}
		fr, frErr := media.NewVP8FrameReader(streams.Video, fps)
		if frErr != nil {
			// Promoted from Debug — likely root cause of "I called
			// /vplay and video never started". Visible at default Info.
			g.log.Warn("video track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Video, models.Camera, endErr, s)
			})
			s.SetMuted(videoOff)
			videoStr = s
		}
	}
	if audioStr == nil && videoStr == nil {
		_ = streams.Close()
		return nil, nil, nil, fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
	return streams, audioStr, videoStr, nil
}

// startLocked is kept as a thin wrapper used by Resume to rehydrate
// streamers after a Pause-time tear-down. (Today Pause keeps streamers
// alive via SetPaused, so startLocked only ever runs from a Resume that
// finds them nil — typically because SetSource was called while paused.)
func (g *GroupCall) startLocked(ctx context.Context) error {
	if g.src == nil {
		return nil
	}
	var (
		streams *media.Streams
		err     error
	)
	if seekable, ok := g.src.(media.SeekableSource); ok && g.resumeMs > 0 {
		streams, err = seekable.OpenAt(ctx, time.Duration(g.resumeMs)*time.Millisecond)
	} else {
		streams, err = g.src.Open(ctx)
	}
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	g.streams = streams

	var audioOK, videoOK bool
	if streams.Audio != nil {
		fr, frErr := media.NewOpusFrameReader(streams.Audio)
		if frErr != nil {
			g.log.Warn("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Audio, models.Microphone, endErr, s)
			})
			s.SetMuted(g.muted)
			if g.paused {
				s.SetPaused(true)
			}
			s.Start()
			g.audioStr = s
			audioOK = true
		}
	}
	if streams.Video != nil {
		fps := g.srcEncOpt.VideoFPS
		if fps <= 0 {
			fps = 30
		}
		fr, frErr := media.NewVP8FrameReader(streams.Video, fps)
		if frErr != nil {
			g.log.Warn("video track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Video, models.Camera, endErr, s)
			})
			s.SetMuted(g.videoOff)
			if g.paused {
				s.SetPaused(true)
			}
			s.Start()
			g.videoStr = s
			videoOK = true
		}
	}
	if !audioOK && !videoOK {
		_ = streams.Close()
		g.streams = nil
		return fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
	// Arm the per-cycle stream-end guard. We hold g.mu, so any EOF
	// dispatched from a streamer that EOF'd between Start and here will
	// block at the closure's g.mu.Lock() until this returns.
	g.endedOnce = false
	g.hadAudio = audioOK
	g.hadVideo = videoOK
	return nil
}

func (g *GroupCall) stopStreamersLocked() {
	if g.audioStr != nil {
		g.audioStr.Stop()
		g.audioStr = nil
	}
	if g.videoStr != nil {
		g.videoStr.Stop()
		g.videoStr = nil
	}
	if g.streams != nil {
		_ = g.streams.Close()
		g.streams = nil
	}
}

// handleStreamerEnd is the per-streamer onEnd callback. Receives the
// streamer pointer captured at construction so the dispatched cleanup
// can verify the ended streamer is still the current one before firing
// (concurrent SetSource may have already replaced it).
//
// For a video+audio source, the first leg to EOF wins the right to fire.
// The sibling streamer is force-stopped and its own EOF is suppressed by
// the endedOnce guard. The user gets two OnStreamEnd events in fixed
// order — Video then Audio — regardless of which leg ended first.
// Audio-only sources fire a single Audio event; video-only sources fire
// a single Video event.
func (g *GroupCall) handleStreamerEnd(t models.StreamType, d models.Device, err error, str *media.Streamer) {
	closed := g.closed.Load()
	switching := g.switching.Load()
	g.log.Debug("streamer end",
		slog.Any("type", t), slog.Any("device", d), slog.Any("err", err),
		slog.Bool("closed", closed), slog.Bool("switching", switching))
	if closed || switching {
		return
	}
	if g.disp == nil {
		return
	}
	g.disp.Submit(func() {
		if g.closed.Load() || g.switching.Load() {
			return
		}
		g.mu.Lock()
		// Stale dispatch: SetSource replaced this streamer with a
		// different one before our closure ran. We detect that by the
		// field being non-nil but pointing at a different Streamer.
		// (PC-Failed cleanup nils the field externally — that path
		// should still fire, so we don't suppress on plain nil.)
		isStale := (t == models.Audio && g.audioStr != nil && g.audioStr != str) ||
			(t == models.Video && g.videoStr != nil && g.videoStr != str)
		if isStale || g.endedOnce {
			g.mu.Unlock()
			return
		}
		g.endedOnce = true
		hadAudio := g.hadAudio
		hadVideo := g.hadVideo
		// Capture the sibling streamer (the one that did NOT end). The
		// streamer that ended naturally needs no Stop call.
		var siblingAudio, siblingVideo *media.Streamer
		if t == models.Video {
			siblingAudio = g.audioStr
		} else {
			siblingVideo = g.videoStr
		}
		g.audioStr = nil
		g.videoStr = nil
		// EOF doesn't fire OnUpgrade — OnStreamEnd already signals
		// end-of-stream and the caller drives any MTProto state change
		// from there.
		fn := g.ev.OnStreamEnd
		g.mu.Unlock()

		// Stop the sibling outside the lock. Streamer.Stop blocks until
		// its run goroutine exits; that exit fires onEnd which dispatches
		// a handleStreamerEnd whose closure sees endedOnce=true (or
		// isCurrent=false because we cleared the field) and returns.
		if siblingVideo != nil {
			siblingVideo.Stop()
		}
		if siblingAudio != nil {
			siblingAudio.Stop()
		}

		if fn == nil {
			return
		}
		if hadVideo {
			fn(models.Video, models.Camera, err)
		}
		if hadAudio {
			fn(models.Audio, models.Microphone, err)
		}
	})
}

// currentStateLocked computes the MediaState a hypothetical OnUpgrade
// callback would report right now. Caller must hold g.mu (read or
// write — read fields only).
//
// Paused and PresentationPaused both follow g.muted||g.paused — the
// outgoing media is not flowing whenever the user muted or paused, so
// the MTProto-mirrored "paused" flags flip together. The library has
// no presentation source, so PresentationPaused has no independent
// state — exposing it keeps downstream MTProto code uniform.
func (g *GroupCall) currentStateLocked() models.MediaState {
	silent := g.muted || g.paused
	return models.MediaState{
		Muted:              g.muted,
		Paused:             silent,
		VideoStopped:       g.videoStr == nil,
		PresentationPaused: silent,
	}
}

// fireUpgradeIfChangedLocked submits an OnUpgrade dispatch only if the
// current state differs from prev. Caller must hold g.mu. Dispatch is
// async via the shared dispatcher so callers can safely re-enter Client
// API from inside the callback.
//
// The nil-check on disp/OnUpgrade runs *before* the second
// currentStateLocked call — when no listener is wired up, we skip the
// struct literal entirely. Cheap, but every Mute/Pause/Resume/Unmute
// goes through here.
func (g *GroupCall) fireUpgradeIfChangedLocked(prev models.MediaState) {
	if g.disp == nil || g.ev.OnUpgrade == nil {
		return
	}
	cur := g.currentStateLocked()
	if prev == cur {
		return
	}
	g.disp.Submit(func() { g.ev.OnUpgrade(cur) })
}

// Pause / Resume / Mute / Unmute fire OnUpgrade on real transitions so
// callers can drive the matching MTProto participant flags off the
// callback. No-op toggles (already in the target state) stay silent —
// fireUpgradeIfChangedLocked skips when prev == cur. Stop and
// SetStreamSources remain silent: Stop tears the call down (no peer
// left to mirror to) and SetStreamSources is the caller's own
// transition (they already know the new VideoStopped). Spontaneous
// transitions (video leg EOF mid-stream, ICE Failed/Closed) still
// fire from handleStreamerEnd / the PC state callback.
func (g *GroupCall) Pause() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.paused = true
	// Block the pull loop on the streamer's gate without killing ffmpeg.
	// The OS pipe absorbs the next ~1s of frames; Resume wakes the loop.
	if g.audioStr != nil {
		g.audioStr.SetPaused(true)
	}
	if g.videoStr != nil {
		g.videoStr.SetPaused(true)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Resume() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.paused = false
	// If streamers exist (gate-paused), just unblock them. Otherwise, the
	// source was never started (e.g. paused before SetStreamSources) — start now.
	if g.audioStr != nil || g.videoStr != nil {
		if g.audioStr != nil {
			g.audioStr.SetPaused(false)
		}
		if g.videoStr != nil {
			g.videoStr.SetPaused(false)
		}
		g.fireUpgradeIfChangedLocked(prev)
		return true, nil
	}
	if g.src == nil {
		g.fireUpgradeIfChangedLocked(prev)
		return true, nil
	}
	err := g.startLocked(context.Background())
	g.fireUpgradeIfChangedLocked(prev)
	return true, err
}

func (g *GroupCall) Mute() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.muted {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.muted = true
	if g.audioStr != nil {
		g.audioStr.SetMuted(true)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Unmute() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.muted {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.muted = false
	if g.audioStr != nil {
		g.audioStr.SetMuted(false)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Stop() error {
	if !g.closed.CompareAndSwap(false, true) {
		return nil
	}
	g.mu.Lock()
	g.stopStreamersLocked()
	g.src = nil
	g.srcEncOpt = media.EncodeOptions{}
	g.resumeMs = 0
	g.paused = false
	g.muted = false
	g.videoOff = false
	g.mu.Unlock()
	// Intentionally no fireUpgradeIfChangedLocked here — Stop is
	// user-initiated, the caller already knows the call is gone (same
	// principle as OnStreamEnd being skipped on Stop). Firing
	// videoStopped=true after a vplay→Stop would be inconsistent with
	// play→Stop (which fires nothing because the state never changed).
	return g.pc.Close()
}

// SeekBy shifts playback by deltaMs relative to the current position
// (positive forward, negative backward). If the resulting position is
// below 0 the seek is treated as an EOF: streamers are stopped and
// OnStreamEnd fires through the normal handleStreamerEnd path. Forward
// overshoots past source duration are detected naturally — ffmpeg's
// `-ss` lands past the end, the OGG/IVF reader sees zero frames, and
// the streamer EOFs on its own.
//
// Returns ErrSeekUnsupported if the source does not implement
// media.SeekableSource and ErrNoSource if no source is currently
// playing. Does not fire OnUpgrade — the user initiated the move
// (same principle as SetSource).
func (g *GroupCall) SeekBy(deltaMs int64) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.mu.Lock()
	if g.src == nil {
		g.mu.Unlock()
		return models.ErrNoSource
	}
	if _, ok := g.src.(media.SeekableSource); !ok {
		g.mu.Unlock()
		return models.ErrSeekUnsupported
	}
	if deltaMs == 0 {
		g.mu.Unlock()
		return nil
	}
	var streamerMs uint64
	switch {
	case g.audioStr != nil:
		streamerMs = g.audioStr.ElapsedMs()
	case g.videoStr != nil:
		streamerMs = g.videoStr.ElapsedMs()
	}
	target := int64(g.resumeMs+streamerMs) + deltaMs
	if target < 0 {
		// Underflow → force EOF. stopStreamersLocked triggers each
		// streamer's onEnd which dispatches OnStreamEnd through the
		// normal handleStreamerEnd path (endedOnce is still false from
		// startLocked, so the first leg fires).
		g.stopStreamersLocked()
		g.mu.Unlock()
		return nil
	}
	// Normal seek: suppress the old streamers' EOF dispatches via
	// switching, kill them, advance resumeMs, reopen via OpenAt. The
	// switching flag is cleared after Unlock by defer (mirrors
	// SetStreamSources — any race window during new-streamer Start is
	// the same one SetStreamSources accepts).
	g.switching.Store(true)
	defer g.switching.Store(false)
	g.stopStreamersLocked()
	g.resumeMs = uint64(target)
	err := g.startLocked(context.Background())
	g.mu.Unlock()
	return err
}

func (g *GroupCall) ElapsedMs() uint64 {
	g.mu.RLock()
	str := g.audioStr
	if str == nil {
		str = g.videoStr
	}
	base := g.resumeMs
	g.mu.RUnlock()
	if str == nil {
		return base
	}
	return base + str.ElapsedMs()
}

func (g *GroupCall) State() models.MediaState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.currentStateLocked()
}

func (g *GroupCall) NetState() models.ConnState {
	return models.ConnState(g.netState.Load())
}

// AudioSSRC is exposed so callers can pass it as the Source param to
// phone.LeaveGroupCall.
func (g *GroupCall) AudioSSRC() uint32 { return g.pc.AudioSSRC() }
