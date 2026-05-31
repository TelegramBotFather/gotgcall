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
}

// GroupCall is the WebRTC call instance for one chat.
type GroupCall struct {
	ev GroupCallEvents

	src       media.Source
	pc        *wrtc.PeerConnection
	log       *slog.Logger
	disp      *utils.Dispatcher
	streams   *media.Streams
	audioStr  *media.Streamer
	videoStr  *media.Streamer
	srcEncOpt media.EncodeOptions
	chatID    int64
	resumeMs  uint64 // seek offset captured on Pause; injected via SeekableSource.OpenAt on Resume

	mu        sync.RWMutex
	netState  atomic.Int32 // models.ConnState
	closed    atomic.Bool
	switching atomic.Bool // true while SetSource is replacing the source; suppresses OnStreamEnd for the old streamer
	paused    bool
	muted     bool
	videoOff  bool
}

// NewGroupCall constructs a fresh call. Caller threads pion factory + logger.
func NewGroupCall(chatID int64, factory *wrtc.Factory, disp *utils.Dispatcher, log *slog.Logger, ev GroupCallEvents) (*GroupCall, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pc, err := wrtc.NewPeerConnection(factory, log)
	if err != nil {
		return nil, err
	}
	gc := &GroupCall{
		chatID: chatID,
		pc:     pc,
		log:    log.With(slog.Int64("chat", chatID)),
		disp:   disp,
		ev:     ev,
	}
	gc.netState.Store(int32(models.Connecting))
	pc.OnConnectionStateChange(func(s models.ConnState) {
		gc.netState.Store(int32(s))
		// When pion declares the PC Failed or Closed, the underlying transport
		// is gone and any further WriteSample on the audio/video tracks is
		// discarded internally. Tear the streamers down so we stop burning CPU
		// + pipe-IO pumping samples into the void. Without this, a 3-minute
		// song after an ICE Failed keeps ffmpeg + the streamer running for the
		// full 3 minutes before EOF — observed as msSent climbing into the
		// hundreds of thousands while the PC is already dead.
		//
		// Routed through the dispatcher so we don't take g.mu from pion's
		// callback goroutine (which might race against SetSource holding it).
		// onStreamEnd fires naturally from the streamer's run() defer as it
		// exits, so the user's OnStreamEnd handler still triggers.
		if (s == models.Failed || s == models.Closed) && gc.disp != nil {
			gc.disp.Submit(func() {
				gc.mu.Lock()
				gc.stopStreamersLocked()
				gc.mu.Unlock()
			})
		}
		if gc.disp != nil && gc.ev.OnConnectionChange != nil {
			gc.disp.Submit(func() { gc.ev.OnConnectionChange(models.NetworkInfo{State: s}) })
		}
	})
	return gc, nil
}

func (g *GroupCall) Mode() string { return "webrtc" }

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
	return g.pc.Connect(remoteJSON)
}

func (g *GroupCall) SetSource(ctx context.Context, src media.Source) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// Suppress OnStreamEnd from the streamer we're about to cancel — the
	// caller is replacing the source, not signalling EOF. Without this,
	// playlist auto-advance reacts to a phantom end event and races the swap.
	g.switching.Store(true)
	defer g.switching.Store(false)

	// Tear down any existing streamers + streams atomically.
	g.stopStreamersLocked()

	g.src = src
	g.resumeMs = 0 // new source = fresh playback, drop any pending pause offset
	// Source-owned encode opts (FPS for the VP8 reader's pacing). FromShell
	// sources don't expose them — startLocked falls back to 30 FPS.
	if sp, ok := src.(media.SourcePath); ok {
		g.srcEncOpt = sp.EncodeOpts()
	} else {
		g.srcEncOpt = media.EncodeOptions{}
	}

	if g.paused {
		// Streamers will start on Resume.
		return nil
	}
	return g.startLocked(ctx)
}

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
			// Source had no audio stream (or it was malformed). Skip the
			// audio track silently — the video leg (if any) still plays.
			g.log.Debug("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			g.audioStr = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(err error) {
				g.handleEnd(models.Audio, models.Microphone, err)
			})
			g.audioStr.SetMuted(g.muted)
			g.audioStr.Start()
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
			g.log.Debug("video track unavailable, skipping", slog.Any("err", frErr))
		} else {
			g.videoStr = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(err error) {
				g.handleEnd(models.Video, models.Camera, err)
			})
			g.videoStr.SetMuted(g.videoOff)
			g.videoStr.Start()
			videoOK = true
		}
	}
	if !audioOK && !videoOK {
		_ = streams.Close()
		g.streams = nil
		return fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
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

func (g *GroupCall) handleEnd(t models.StreamType, d models.Device, err error) {
	closed := g.closed.Load()
	switching := g.switching.Load()
	g.log.Debug("handleEnd",
		slog.Any("type", t), slog.Any("device", d), slog.Any("err", err),
		slog.Bool("closed", closed), slog.Bool("switching", switching))
	if closed || switching {
		return
	}
	if g.disp != nil && g.ev.OnStreamEnd != nil {
		g.disp.Submit(func() { g.ev.OnStreamEnd(t, d, err) })
	}
}

func (g *GroupCall) Pause() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return false, nil
	}
	g.paused = true
	// Block the pull loop on the streamer's gate without killing ffmpeg.
	// The OS pipe absorbs the next ~1s of frames; Resume wakes the loop.
	if g.audioStr != nil {
		g.audioStr.SetPaused(true)
	}
	if g.videoStr != nil {
		g.videoStr.SetPaused(true)
	}
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
	g.paused = false
	// If streamers exist (gate-paused), just unblock them. Otherwise the
	// source was never started (e.g. paused before SetStreamSources) — start now.
	if g.audioStr != nil || g.videoStr != nil {
		if g.audioStr != nil {
			g.audioStr.SetPaused(false)
		}
		if g.videoStr != nil {
			g.videoStr.SetPaused(false)
		}
		return true, nil
	}
	if g.src == nil {
		return true, nil
	}
	return true, g.startLocked(context.Background())
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
	g.muted = true
	if g.audioStr != nil {
		g.audioStr.SetMuted(true)
	}
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
	g.muted = false
	if g.audioStr != nil {
		g.audioStr.SetMuted(false)
	}
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
	return g.pc.Close()
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
	return models.MediaState{Muted: g.muted, Paused: g.paused, VideoStopped: g.videoOff}
}

func (g *GroupCall) NetState() models.ConnState {
	return models.ConnState(g.netState.Load())
}

// AudioSSRC is exposed so callers can pass it as the Source param to
// phone.LeaveGroupCall.
func (g *GroupCall) AudioSSRC() uint32 { return g.pc.AudioSSRC() }
