package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gotgcall/media"
	"gotgcall/models"
	"gotgcall/utils"
	"gotgcall/wrtc"
)

// GroupCallEvents is the set of callbacks a GroupCall fires through the
// shared dispatcher; the Client wires these to its public OnXxx callbacks.
type GroupCallEvents struct {
	OnStreamEnd        func(t models.StreamType, d models.Device, err error)
	OnConnectionChange func(info models.NetworkInfo)
}

// GroupCall is the WebRTC call instance for one chat.
type GroupCall struct {
	startedAt time.Time
	ev        GroupCallEvents

	src       media.Source
	pc        *wrtc.PeerConnection
	log       *slog.Logger
	disp      *utils.Dispatcher
	streams   *media.Streams
	audioStr  *media.Streamer
	videoStr  *media.Streamer
	srcEncOpt media.EncodeOptions
	chatID    int64

	mu       sync.RWMutex
	netState atomic.Int32 // models.ConnState
	closed   atomic.Bool
	paused   bool
	muted    bool
	videoOff bool
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

func (g *GroupCall) SetSource(ctx context.Context, src media.Source, opt ...media.EncodeOptions) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// Tear down any existing streamers + streams atomically.
	g.stopStreamersLocked()

	g.src = src
	if len(opt) > 0 {
		g.srcEncOpt = opt[0]
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
	streams, err := g.src.Open(ctx)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	g.streams = streams
	g.startedAt = time.Now()

	if streams.Audio != nil {
		fr, err := media.NewOpusFrameReader(streams.Audio)
		if err != nil {
			_ = streams.Close()
			return err
		}
		g.audioStr = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(err error) {
			g.handleEnd(models.Audio, models.Microphone, err)
		})
		g.audioStr.SetMuted(g.muted)
		g.audioStr.Start()
	}
	if streams.Video != nil {
		fps := g.srcEncOpt.VideoFPS
		if fps <= 0 {
			fps = 30
		}
		fr, err := media.NewVP8FrameReader(streams.Video, fps)
		if err != nil {
			if g.audioStr != nil {
				g.audioStr.Stop()
				g.audioStr = nil
			}
			_ = streams.Close()
			return err
		}
		g.videoStr = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(err error) {
			g.handleEnd(models.Video, models.Camera, err)
		})
		g.videoStr.SetMuted(g.videoOff)
		g.videoStr.Start()
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
	if g.closed.Load() {
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
	g.stopStreamersLocked()
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
	g.mu.Unlock()
	return g.pc.Close()
}

func (g *GroupCall) ElapsedMs() uint64 {
	g.mu.RLock()
	str := g.audioStr
	if str == nil {
		str = g.videoStr
	}
	g.mu.RUnlock()
	if str == nil {
		return 0
	}
	return str.ElapsedMs()
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
