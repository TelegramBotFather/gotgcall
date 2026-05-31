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
//	client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))
//
// See README.md for the full pattern.
package gotgcall

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/instances"
	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
	"github.com/annihilatorrrr/gotgcall/wrtc"
)

// ICEServer is re-exported so callers can configure STUN/TURN without
// importing pion directly.
type ICEServer = webrtc.ICEServer

// NetworkType is re-exported for WithNetworkTypes.
type NetworkType = webrtc.NetworkType

const (
	NetworkTypeUDP4 = webrtc.NetworkTypeUDP4
	NetworkTypeUDP6 = webrtc.NetworkTypeUDP6
	NetworkTypeTCP4 = webrtc.NetworkTypeTCP4
	NetworkTypeTCP6 = webrtc.NetworkTypeTCP6
)

// --- Re-exports for ergonomics -------------------------------------------------

type (
	Source         = media.Source
	SeekableSource = media.SeekableSource
	EncodeOptions  = media.EncodeOptions
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
	FromFile   = media.FromFile
	FromURL    = media.FromURL
	FromShell  = media.FromShell
	FromShells = media.FromShells
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
	logger          *slog.Logger
	ffmpegPath      string
	iceServers      []ICEServer
	networkTypes    []NetworkType
	certPoolSize    int
	dispatchBuf     int
	iceDisconnect   time.Duration
	iceFailed       time.Duration
	iceKeepalive    time.Duration
	sharedUDPMux    bool
	ffmpegStderrLog bool
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

// WithICEServers replaces the default Google STUN servers. Pass TURN entries
// here for users behind symmetric NAT or restrictive firewalls. Pass an
// empty slice to keep the defaults.
//
//	gotgcall.WithICEServers([]gotgcall.ICEServer{
//	    {URLs: []string{"stun:stun.l.google.com:19302"}},
//	    {URLs: []string{"turn:turn.example.com:3478"},
//	     Username: "u", Credential: "p"},
//	})
func WithICEServers(servers []ICEServer) Option {
	return func(c *config) {
		if len(servers) > 0 {
			c.iceServers = servers
		}
	}
}

// WithNetworkTypes overrides the ICE candidate network-type whitelist.
// Default is UDP4 only (Telegram's edge mixers favor IPv4/UDP, and trimming
// the checklist speeds up ICE). Enable IPv6 / TCP for restrictive
// environments where UDP4 is blocked.
//
//	gotgcall.WithNetworkTypes(
//	    gotgcall.NetworkTypeUDP4,
//	    gotgcall.NetworkTypeUDP6,
//	    gotgcall.NetworkTypeTCP4,
//	)
func WithNetworkTypes(types ...NetworkType) Option {
	return func(c *config) {
		if len(types) > 0 {
			c.networkTypes = types
		}
	}
}

// WithICETimeouts overrides pion's ICE timing. Pass 0 for any value to keep
// the library default (30s disconnect grace / 60s failed / 2s keepalive).
// Use longer values on unstable networks where brief connectivity drops
// shouldn't kill the call.
func WithICETimeouts(disconnect, failed, keepalive time.Duration) Option {
	return func(c *config) {
		if disconnect > 0 {
			c.iceDisconnect = disconnect
		}
		if failed > 0 {
			c.iceFailed = failed
		}
		if keepalive > 0 {
			c.iceKeepalive = keepalive
		}
	}
}

// WithDebugLogs is a convenience that installs a Debug-level text handler
// writing to os.Stderr. Equivalent to:
//
//	WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
//
// Use this when reporting bugs — debug-level output covers ICE/DTLS state,
// ffmpeg exit codes, streamer pacing, and pion-internal events bridged
// through the new pion→slog adapter.
func WithDebugLogs() Option {
	return func(c *config) {
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}
}

// WithFFmpegStderrLog tees ffmpeg's stderr output to the library logger at
// Debug level while the process is running. Without this, ffmpeg stderr is
// only surfaced in the final error message (last 512 bytes) when the
// subprocess crashes — useful for crash diagnosis but useless for "ffmpeg
// is running but I see no audio" symptoms. Enable for verbose diagnosis.
func WithFFmpegStderrLog() Option {
	return func(c *config) { c.ffmpegStderrLog = true }
}

// --- Client --------------------------------------------------------------------

// Client multiplexes many concurrent group calls behind a single
// process-wide handle. Safe for concurrent use.
type Client struct {
	factory            *wrtc.Factory
	disp               *utils.Dispatcher
	onStreamEnd        func(chatID int64, t StreamType, d Device, err error)
	onConnectionChange func(chatID int64, info NetworkInfo)
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

// New constructs a Client with the given options. Fails fast if the ffmpeg
// binary isn't on PATH (or wherever WithFFmpegPath points) so callers see
// the error at startup rather than on first stream.
func New(opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if _, err := exec.LookPath(cfg.ffmpegPath); err != nil {
		return nil, fmt.Errorf("ffmpeg binary not found at %q: %w — install ffmpeg or override with WithFFmpegPath",
			cfg.ffmpegPath, err)
	}
	media.SetFFmpegPath(cfg.ffmpegPath)
	media.SetLogger(cfg.logger)
	media.SetStderrLog(cfg.ffmpegStderrLog)

	factory, err := wrtc.NewFactory(wrtc.FactoryOptions{
		Logger:               cfg.logger,
		SharedUDPMux:         cfg.sharedUDPMux,
		CertPoolSize:         cfg.certPoolSize,
		ICEServers:           cfg.iceServers,
		NetworkTypes:         cfg.networkTypes,
		ICEDisconnectTimeout: cfg.iceDisconnect,
		ICEFailedTimeout:     cfg.iceFailed,
		ICEKeepaliveInterval: cfg.iceKeepalive,
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
// Encode options (FPS, tracks, bitrates) ride along with the Source — set
// them on the constructor (FromFile/FromURL).
func (c *Client) SetStreamSources(chatID int64, src Source) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	return call.SetSource(context.Background(), src)
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

// Stop tears down the call and clears every per-chat scrap of state the
// library kept (call instance, create-mutex). After Stop the chatID can be
// re-used cleanly.
func (c *Client) Stop(chatID int64) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	c.calls.Delete(chatID)
	c.createMu.Delete(chatID)
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
