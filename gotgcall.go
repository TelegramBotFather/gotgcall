// Package gotgcall is a pure-Go library for streaming audio and video
// into Telegram group calls. The public API mirrors ntgcalls method
// names so bot code translates one-to-one.
//
// The library is blob-only: signaling JSON is exchanged through your
// own MTProto client (typically gogram). Two calls are required:
//
//	params, _ := client.CreateCall(chatID)
//	resp, _   := tg.PhoneJoinGroupCall(... Params: &DataJson{Data: params})
//	client.Connect(chatID, resp.Updates[...].Call.Params.Data)
//	client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3"))
//
// See README.md for the full pattern.
package gotgcall

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/annihilatorrrr/gotgcall/instances"
	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
	"github.com/annihilatorrrr/gotgcall/wrtc"
)

// --- Re-exports for ergonomics -------------------------------------------------

type (
	Source         = media.Source
	SeekableSource = media.SeekableSource
	EncodeOptions  = media.EncodeOptions
	RawAudioFormat = media.RawAudioFormat
	RawVideoFormat = media.RawVideoFormat
	Track          = media.Track

	StreamType  = models.StreamType
	Device      = models.Device
	MediaState  = models.MediaState
	NetworkInfo = models.NetworkInfo
	ConnState   = models.ConnState
	CallInfo    = models.CallInfo
)

const (
	TrackAudio = media.TrackAudio
	TrackVideo = media.TrackVideo

	Audio      = models.Audio
	Video      = models.Video
	Microphone = models.Microphone
	Camera     = models.Camera

	Connecting = models.Connecting
	Connected  = models.Connected
	Failed     = models.Failed
	Closed     = models.Closed
	Timeout    = models.Timeout
)

var (
	FromFile       = media.FromFile
	FromURL        = media.FromURL
	FromReader     = media.FromReader
	FromOggOpus    = media.FromOggOpus
	FromIVF        = media.FromIVF
	FromEncoded    = media.FromEncoded
	FromRawPCM     = media.FromRawPCM
	FromRawVideo   = media.FromRawVideo
	FromShell      = media.FromShell
	FromFFmpegArgs = media.FromFFmpegArgs
)

// --- Errors (re-export for branchable errors.Is) -------------------------------

var (
	ErrConnectionExists   = models.ErrConnectionExists
	ErrConnectionNotFound = models.ErrConnectionNotFound
	ErrConnectionTimeout  = models.ErrConnectionTimeout
	ErrConnectionFailed   = models.ErrConnectionFailed
	ErrInvalidParams      = models.ErrInvalidParams
	ErrFFmpegSpawn        = models.ErrFFmpegSpawn
	ErrFFmpegCrashed      = models.ErrFFmpegCrashed
	ErrFile               = models.ErrFile
	ErrClosed             = models.ErrClosed
	ErrInternal           = models.ErrInternal
	ErrNotConnected       = models.ErrNotConnected
	ErrWrongMode          = models.ErrWrongMode
)

// --- Options -------------------------------------------------------------------

type Option func(*config)

type config struct {
	logger       *slog.Logger
	ffmpegPath   string
	sharedUDPMux bool
	certPoolSize int
	dispatchBuf  int
}

func defaultConfig() config {
	return config{
		logger:       slog.New(slog.DiscardHandler),
		ffmpegPath:   "ffmpeg",
		sharedUDPMux: false,
		certPoolSize: 8,
		dispatchBuf:  256,
	}
}

// WithLogger sets a structured logger for internal events.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithFFmpegPath overrides the ffmpeg binary path (default "ffmpeg").
func WithFFmpegPath(p string) Option {
	return func(c *config) {
		if p != "" {
			c.ffmpegPath = p
		}
	}
}

// WithSharedUDPMux makes all calls share one UDP socket for ICE traffic.
// Useful for high-concurrency setups (100+ simultaneous calls).
func WithSharedUDPMux() Option {
	return func(c *config) { c.sharedUDPMux = true }
}

// WithDTLSCertPool sets the size of the pre-generated DTLS certificate
// pool. Larger pools absorb bigger call-creation bursts without keygen
// latency. 0 disables pre-generation.
func WithDTLSCertPool(n int) Option {
	return func(c *config) { c.certPoolSize = n }
}

// WithDispatchBuffer sizes the event dispatcher's channel. Default 256.
func WithDispatchBuffer(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.dispatchBuf = n
		}
	}
}

// --- Client --------------------------------------------------------------------

// Client multiplexes many concurrent group calls behind a single
// process-wide handle. Safe for concurrent use.
type Client struct {
	factory            *wrtc.Factory
	disp               *utils.Dispatcher
	onStreamEnd        func(chatID int64, t StreamType, d Device, err error)
	onConnectionChange func(chatID int64, info NetworkInfo)
	onUpgrade          func(chatID int64, state MediaState)
	calls              sync.Map // map[int64]instances.Call
	createMu           sync.Map // map[int64]*sync.Mutex — gates CreateCall/StartRTMP per chat
	cfg                config
	cbMu               sync.RWMutex
	closed             atomic.Bool
}

// acquireCreate serialises the construction phase of CreateCall and
// StartRTMP for a single chat. Returns the unlock function. The per-chat
// mutex is kept in the map for the lifetime of the process (sizeof
// sync.Mutex per unique chatID ever started — negligible).
func (c *Client) acquireCreate(chatID int64) func() {
	v, _ := c.createMu.LoadOrStore(chatID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// New constructs a Client with the given options.
func New(opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	media.SetFFmpegPath(cfg.ffmpegPath)
	media.SetLogger(cfg.logger)

	factory, err := wrtc.NewFactory(wrtc.FactoryOptions{
		Logger:       cfg.logger,
		SharedUDPMux: cfg.sharedUDPMux,
		CertPoolSize: cfg.certPoolSize,
	})
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:     cfg,
		factory: factory,
		disp:    utils.NewDispatcher(cfg.dispatchBuf, cfg.logger),
	}, nil
}

// Close stops every call and releases resources. Idempotent.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.calls.Range(func(_, v any) bool {
		_ = v.(instances.Call).Stop()
		return true
	})
	if c.factory != nil {
		_ = c.factory.Close()
	}
	if c.disp != nil {
		c.disp.Close()
	}
	return nil
}

// --- Lifecycle: WebRTC mode ----------------------------------------------------

// CreateCall starts a new WebRTC group-call instance for chatID and
// returns the JSON params the caller must pass to phone.JoinGroupCall.
//
// Concurrent CreateCall / StartRTMP calls for the same chat are
// serialized; the first one wins, others get ErrConnectionExists
// without allocating a pion PeerConnection.
func (c *Client) CreateCall(chatID int64) (string, error) {
	if c.closed.Load() {
		return "", ErrClosed
	}
	unlock := c.acquireCreate(chatID)
	defer unlock()
	if _, exists := c.calls.Load(chatID); exists {
		return "", ErrConnectionExists
	}
	gc, err := instances.NewGroupCall(chatID, c.factory, c.disp, c.cfg.logger, c.eventsFor(chatID))
	if err != nil {
		return "", err
	}
	c.calls.Store(chatID, instances.Call(gc))
	return gc.CreateLocalParams()
}

// Connect finishes the WebRTC handshake using Telegram's response JSON.
func (c *Client) Connect(chatID int64, telegramParams string) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	return call.Connect(telegramParams)
}

// --- Lifecycle: RTMP mode ------------------------------------------------------

// StartRTMP creates an RTMP-push call for chatID. The caller obtains
// rtmpURL via phone.GetGroupCallStreamRtmpUrl gogram-side. Serialised
// with CreateCall via the same per-chat creation mutex.
func (c *Client) StartRTMP(chatID int64, rtmpURL string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	unlock := c.acquireCreate(chatID)
	defer unlock()
	if _, exists := c.calls.Load(chatID); exists {
		return ErrConnectionExists
	}
	rc := instances.NewRTMPCall(chatID, rtmpURL, c.disp, c.cfg.logger, c.eventsFor(chatID))
	c.calls.Store(chatID, instances.Call(rc))
	return nil
}

// --- Lifecycle: source control --------------------------------------------------

// SetStreamSources installs or replaces the streaming source for chatID.
func (c *Client) SetStreamSources(chatID int64, src Source, opt ...EncodeOptions) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	return call.SetSource(context.Background(), src, opt...)
}

func (c *Client) Pause(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Pause()
}

func (c *Client) Resume(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Resume()
}

func (c *Client) Mute(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Mute()
}

func (c *Client) Unmute(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Unmute()
}

// Stop tears down the call. After Stop the chatID can be re-used.
func (c *Client) Stop(chatID int64) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	c.calls.Delete(chatID)
	return call.Stop()
}

// --- Introspection -------------------------------------------------------------

// Time returns elapsed ms of media pushed.
func (c *Client) Time(chatID int64) (uint64, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return 0, err
	}
	return call.ElapsedMs(), nil
}

// GetState returns the current media-state (mute/pause flags).
func (c *Client) GetState(chatID int64) (MediaState, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return MediaState{}, err
	}
	return call.State(), nil
}

// Calls returns a snapshot of all active calls.
func (c *Client) Calls() map[int64]CallInfo {
	out := make(map[int64]CallInfo)
	c.calls.Range(func(k, v any) bool {
		id := k.(int64)
		call := v.(instances.Call)
		out[id] = CallInfo{CaptureTimeMs: call.ElapsedMs()}
		return true
	})
	return out
}

// AudioSSRC returns the audio SSRC of a WebRTC call. Pass as Source to
// phone.LeaveGroupCall. Returns ErrWrongMode for RTMP calls.
func (c *Client) AudioSSRC(chatID int64) (uint32, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return 0, err
	}
	gc, ok := call.(*instances.GroupCall)
	if !ok {
		return 0, ErrWrongMode
	}
	return gc.AudioSSRC(), nil
}

// --- Callbacks -----------------------------------------------------------------

// OnStreamEnd registers a callback fired when a track ends (EOF, crash,
// stop). Called on the dispatcher goroutine so it is safe to re-enter
// the Client API from within.
func (c *Client) OnStreamEnd(fn func(chatID int64, t StreamType, d Device, err error)) {
	c.cbMu.Lock()
	c.onStreamEnd = fn
	c.cbMu.Unlock()
}

// OnConnectionChange registers a callback for ICE/DTLS state transitions.
func (c *Client) OnConnectionChange(fn func(chatID int64, info NetworkInfo)) {
	c.cbMu.Lock()
	c.onConnectionChange = fn
	c.cbMu.Unlock()
}

// OnUpgrade fires when the server reports a change in our media state
// (e.g. an admin muted us, video disabled, etc.). Because the library is
// blob-only it cannot observe MTProto updates on its own; the caller
// forwards them via NotifyUpgrade from their own gogram update handler.
func (c *Client) OnUpgrade(fn func(chatID int64, state MediaState)) {
	c.cbMu.Lock()
	c.onUpgrade = fn
	c.cbMu.Unlock()
}

// NotifyUpgrade forwards a server-side media-state change to OnUpgrade.
// Call this from your gogram updates handler when you see an
// UpdateGroupCallParticipants event for your own peer with new
// muted/video_stopped flags. The library will fan the event out to the
// dispatcher so the callback runs without blocking your update loop.
func (c *Client) NotifyUpgrade(chatID int64, state MediaState) {
	if c.closed.Load() {
		return
	}
	c.cbMu.RLock()
	fn := c.onUpgrade
	c.cbMu.RUnlock()
	if fn == nil || c.disp == nil {
		return
	}
	c.disp.Submit(func() { fn(chatID, state) })
}

// --- internals -----------------------------------------------------------------

func (c *Client) lookup(chatID int64) (instances.Call, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	v, ok := c.calls.Load(chatID)
	if !ok {
		return nil, ErrConnectionNotFound
	}
	return v.(instances.Call), nil
}

func (c *Client) eventsFor(chatID int64) instances.GroupCallEvents {
	return instances.GroupCallEvents{
		OnStreamEnd: func(t models.StreamType, d models.Device, err error) {
			c.cbMu.RLock()
			fn := c.onStreamEnd
			c.cbMu.RUnlock()
			if fn != nil {
				fn(chatID, t, d, err)
			}
		},
		OnConnectionChange: func(info models.NetworkInfo) {
			c.cbMu.RLock()
			fn := c.onConnectionChange
			c.cbMu.RUnlock()
			if fn != nil {
				fn(chatID, info)
			}
		},
	}
}
