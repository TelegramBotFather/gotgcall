package wrtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/models"
)

// Factory creates pion PeerConnections. One Factory per Client; shared
// across all calls, optionally backed by a shared UDP mux for high
// concurrency setups.
type Factory struct {
	udpMux           ice.UDPMux
	api              *webrtc.API
	log              *slog.Logger
	certPool         *CertPool
	monitor          *FactoryMonitor
	settings         webrtc.SettingEngine
	iceServers       []webrtc.ICEServer
	mu               sync.Mutex
	closed           bool
	logICECandidates bool
}

// Monitor returns the per-Factory shared monitor goroutine that drives
// video keepalive padding + RTP liveness watchdog for every PC created
// by this Factory. NewPeerConnection registers itself; Close
// unregisters. Single goroutine handles N concurrent calls.
func (f *Factory) Monitor() *FactoryMonitor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.monitor
}

// defaultSTUNServers provides server-reflexive candidates for users behind
// NAT. Host-only candidates work when the bot is on a public IP or the NAT
// is permissive, but symmetric NAT / cloud VPC setups need srflx to reach
// Telegram's SFU. gortc ships 7 servers; we use a smaller set to keep
// gathering fast while still getting a reflexive candidate.
var defaultSTUNServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
	{URLs: []string{"stun:stun1.l.google.com:19302"}},
}

// ICEServers returns the configured ICE server list (custom from
// FactoryOptions.ICEServers, falling back to the built-in defaults). Used
// by PeerConnection construction to populate webrtc.Configuration.
// A nil iceServers field means "use defaults"; a non-nil empty slice means
// "user explicitly disabled STUN".
func (f *Factory) ICEServers() []webrtc.ICEServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.iceServers != nil {
		out := make([]webrtc.ICEServer, len(f.iceServers))
		copy(out, f.iceServers)
		return out
	}
	out := make([]webrtc.ICEServer, len(defaultSTUNServers))
	copy(out, defaultSTUNServers)
	return out
}

// LogICECandidates reports whether PeerConnection construction should hook
// OnICECandidate to log each gathered candidate at Debug.
func (f *Factory) LogICECandidates() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logICECandidates
}

type FactoryOptions struct {
	Logger *slog.Logger
	// ICEServers overrides ICE server configuration. Default is empty (ICE-lite
	// needs no STUN). Pass TURN servers for users behind symmetric NAT.
	ICEServers []webrtc.ICEServer
	// NetworkTypes overrides the candidate network-type whitelist. Default is
	// UDP4+UDP6 (matching ntgcalls). Use this to restrict to UDP4-only or add
	// TCP if your environment requires it.
	NetworkTypes []webrtc.NetworkType
	CertPoolSize int
	// ICEDisconnectTimeout — pion declares the call disconnected after this
	// long with no traffic. 0 = library default (30 s).
	ICEDisconnectTimeout time.Duration
	// ICEFailedTimeout — pion declares the call failed after this long with
	// no successful connectivity check. 0 = library default (60 s).
	ICEFailedTimeout time.Duration
	// ICEKeepaliveInterval — STUN keepalive cadence. 0 = library default (2 s).
	ICEKeepaliveInterval time.Duration
	SharedUDPMux         bool
	// PionTraceAsDebug remaps pion's Trace level to slog.LevelDebug instead
	// of LevelDebug-4. Surfaces ICE per-check / per-candidate / per-binding-
	// request lines in any standard Debug-level handler — useful for
	// diagnosing "ICE stuck in Checking" failures.
	PionTraceAsDebug bool
	// LogICECandidates logs every locally-gathered candidate at Debug via
	// pc.OnICECandidate. Read by PeerConnection construction to decide
	// whether to install the candidate-logger hook.
	LogICECandidates bool
}

func NewFactory(opts FactoryOptions) (*Factory, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// Bridge pion's internal logging (ICE, DTLS, SCTP, interceptors, etc.) into
	// the user's slog.Logger so WithLogger(...LevelDebug) actually surfaces
	// pion events. Without this, pion writes to its own default factory
	// (stderr via the `log` package), bypassing every gotgcall.WithLogger
	// configuration — the single biggest "debug logs aren't working" complaint.
	settings := webrtc.SettingEngine{LoggerFactory: newSlogPionFactory(log, opts.PionTraceAsDebug)}
	settings.SetIncludeLoopbackCandidate(false)
	// Full ICE with no STUN servers: we gather only host candidates (fast,
	// like ntgcalls) but still actively send connectivity checks to Telegram's
	// SFU. pion's strict ICE-lite doesn't send checks at all (RFC 8445
	// compliant), while libwebrtc's "lite" mode still checks — so we use full
	// ICE to match ntgcalls' actual wire behavior.
	settings.SetLite(false)
	// UDP4+UDP6: ntgcalls enables both via PORTALLOCATOR_ENABLE_IPV6. Telegram's
	// SFU accepts IPv6 candidates, and dual-stack hosts get more candidate pairs
	// to work with. Caller can override via FactoryOptions.NetworkTypes.
	networkTypes := opts.NetworkTypes
	if len(networkTypes) == 0 {
		networkTypes = []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	}
	settings.SetNetworkTypes(networkTypes)
	// ICE timeouts: 60 s disconnect grace, 120 s before declaring failed, 2 s
	// keepalive. With ICE-lite the connect is faster (no STUN round-trips), but
	// we keep generous timeouts for network wobble during Telegram SFU steering.
	// Override via FactoryOptions.ICE* fields.
	disconnect := opts.ICEDisconnectTimeout
	if disconnect == 0 {
		disconnect = 60 * time.Second
	}
	failed := opts.ICEFailedTimeout
	if failed == 0 {
		failed = 120 * time.Second
	}
	keepalive := opts.ICEKeepaliveInterval
	if keepalive == 0 {
		keepalive = 2 * time.Second // unchanged — more-frequent keepalive helps NAT bindings
	}
	settings.SetICETimeouts(disconnect, failed, keepalive)
	// Zero per-candidate-type acceptance windows so pion considers all candidate
	// types immediately. With default STUN we get host+srflx; zeroing the
	// windows matches ntgcalls and avoids any stagger delay.
	settings.SetHostAcceptanceMinWait(0)
	settings.SetSrflxAcceptanceMinWait(0)
	settings.SetPrflxAcceptanceMinWait(0)
	settings.SetRelayAcceptanceMinWait(0)
	// Skip virtual / VPN interfaces — gathering candidates on them slows ICE
	// and produces unreachable pairs. Captured by the closure once; each
	// candidate-gather pass does N substring scans rather than re-walking
	// a literal slice every time.
	settings.SetInterfaceFilter(makeInterfaceFilter())
	settings.SetIPFilter(makeIPFilter())

	f := &Factory{
		settings:         settings,
		log:              log,
		certPool:         NewCertPool(opts.CertPoolSize, log),
		monitor:          NewFactoryMonitor(log),
		iceServers:       opts.ICEServers,
		logICECandidates: opts.LogICECandidates,
	}
	// Single goroutine drives keepalive + liveness for every PC this
	// Factory ever produces; PCs Register at NewPeerConnection and
	// Unregister at Close. With 100 concurrent calls per Client this is
	// 1 goroutine instead of 100 (the v0.6.5 first draft had one per PC).
	f.monitor.Start()
	if opts.SharedUDPMux {
		lc, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			return nil, err
		}
		f.udpMux = webrtc.NewICEUDPMux(nil, lc)
		f.settings.SetICEUDPMux(f.udpMux)
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := registerCodecs(mediaEngine); err != nil {
		return nil, err
	}
	// Telegram's SFU requires the full set of RTP header extensions below;
	// without ssrc-audio-level audio is treated as silence and not forwarded.
	audioExtensions := []string{
		audioLevelURI,
		absSendTimeURI,
		transportCCURI,
		sdesMidURI,
	}
	for _, uri := range audioExtensions {
		if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register audio hdrext %s: %w", uri, err)
		}
	}
	videoExtensions := []string{
		absSendTimeURI,
		transportCCURI,
		videoOrientationURI,
		// sdes-mid is critical for BUNDLE demux on the SFU side: incoming
		// video RTP packets carry the mid value of the video m-section, and
		// Telegram's SFU uses it to associate the inferred video SSRC with
		// the right track. Without sdes-mid registered for video, the SFU
		// can't bind our video SSRC and silently drops frames while the
		// elapsed timer still ticks on the sender side.
		sdesMidURI,
	}
	for _, uri := range videoExtensions {
		if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("register video hdrext %s: %w", uri, err)
		}
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		return nil, err
	}
	interceptors.Add(&audioLevelInterceptorFactory{})
	interceptors.Add(&markerClearInterceptorFactory{})
	// Pion v4 negotiates sdes-mid in SDP but ships no built-in interceptor
	// that actually stamps the extension on outgoing RTP — only TWCC writes
	// extensions. Telegram's SFU may use sdes-mid for BUNDLE demux of
	// incoming participant media, so we stamp it ourselves as
	// defense-in-depth: mid="0" for audio, mid="1" for video, matching the
	// transceiver order in NewPeerConnection.
	interceptors.Add(&midStampInterceptorFactory{})

	f.api = webrtc.NewAPI(
		webrtc.WithSettingEngine(f.settings),
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptors),
	)
	return f, nil
}

// skipInterfaceSubstrings is the package-level fixed list of name fragments
// that mark a virtual / VPN / container interface. Pre-lowered, scanned in
// order with strings.Contains. Avoids re-allocating the slice for every
// interface check during ICE gathering.
var skipInterfaceSubstrings = [...]string{
	"vethernet", "vmware", "virtualbox", "vbox", "hyper-v",
	"loopback", "teredo", "isatap", "tap-",
	"docker", "wsl", "tailscale", "zerotier", "openvpn",
}

func makeInterfaceFilter() func(string) bool {
	return func(name string) bool {
		lower := strings.ToLower(name)
		for _, skip := range skipInterfaceSubstrings {
			if strings.Contains(lower, skip) {
				return false
			}
		}
		return true
	}
}

// makeIPFilter excludes IPs from subnets that produce unreachable ICE
// candidates. Windows ICS (192.168.137.0/24) is the primary one — gortc
// also filters this.
func makeIPFilter() func(ip net.IP) bool {
	icsNet := net.IPNet{IP: net.IP{192, 168, 137, 0}, Mask: net.CIDRMask(24, 32)}
	return func(ip net.IP) bool {
		return !icsNet.Contains(ip)
	}
}

func registerCodecs(m *webrtc.MediaEngine) error {
	// Audio: Opus PT 111 (Telegram standard). The fmtp line declares stereo
	// and a high max bitrate so Telegram's SFU allocates bandwidth for music
	// rather than speech — matching gortc's parameters.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    uint32(models.OpusSampleRate),
			Channels:     2,
			SDPFmtpLine:  "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=510000",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "transport-cc"}},
		},
		PayloadType: models.OpusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	// Video: VP8 PT 100 (Telegram standard).
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "goog-remb"},
				{Type: "transport-cc"},
				{Type: "ccm", Parameter: "fir"},
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: models.VP8PayloadType,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return err
	}
	return nil
}

// NewPeerConnection returns a fresh pion *PeerConnection using a
// certificate from the pool.
func (f *Factory) NewPeerConnection(cfg webrtc.Configuration) (*webrtc.PeerConnection, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, models.ErrClosed
	}
	api := f.api
	pool := f.certPool
	f.mu.Unlock()
	if api == nil {
		return nil, errors.New("wrtc: factory not initialized")
	}
	if pool != nil {
		if cert, err := pool.Take(context.Background()); err == nil && cert != nil {
			cfg.Certificates = []webrtc.Certificate{*cert}
		}
	}
	return api.NewPeerConnection(cfg)
}

// Close releases the shared UDP mux (if any), cert pool, and the per-
// Factory monitor goroutine.
func (f *Factory) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	pool := f.certPool
	mux := f.udpMux
	monitor := f.monitor
	f.mu.Unlock()
	if monitor != nil {
		monitor.Stop()
	}
	if pool != nil {
		pool.Close()
	}
	if mux != nil {
		return mux.Close()
	}
	return nil
}
