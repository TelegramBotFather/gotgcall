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
	udpMux     ice.UDPMux
	api        *webrtc.API
	log        *slog.Logger
	certPool   *CertPool
	settings   webrtc.SettingEngine
	iceServers []webrtc.ICEServer
	mu         sync.Mutex
	closed     bool
}

// ICEServers returns the configured ICE server list (custom from
// FactoryOptions.ICEServers, falling back to the built-in defaults). Used
// by PeerConnection construction to populate webrtc.Configuration.
func (f *Factory) ICEServers() []webrtc.ICEServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.iceServers) > 0 {
		out := make([]webrtc.ICEServer, len(f.iceServers))
		copy(out, f.iceServers)
		return out
	}
	return nil
}

type FactoryOptions struct {
	Logger *slog.Logger
	// ICEServers replaces the default STUN list. Pass nil/empty to use the
	// built-in Google STUN servers. Use this to add TURN for users behind
	// symmetric NAT / restrictive firewalls.
	ICEServers []webrtc.ICEServer
	// NetworkTypes overrides the candidate network-type whitelist. Default
	// is UDP4 only (Telegram's edge mixers favor IPv4/UDP). Use this to
	// enable IPv6 or TCP if your environment requires it.
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
	settings := webrtc.SettingEngine{LoggerFactory: newSlogPionFactory(log)}
	settings.SetIncludeLoopbackCandidate(false)
	settings.SetLite(false)
	// Telegram's edge mixers favor IPv4/UDP. Restricting candidate types here
	// trims the ICE checklist (faster connect) and avoids spurious failed
	// pairings over IPv6 / TCP that Telegram doesn't accept anyway. Caller
	// can override via FactoryOptions.NetworkTypes for restrictive environments
	// where IPv6 or TCP is the only viable path.
	networkTypes := opts.NetworkTypes
	if len(networkTypes) == 0 {
		networkTypes = []webrtc.NetworkType{webrtc.NetworkTypeUDP4}
	}
	settings.SetNetworkTypes(networkTypes)
	// ICE timeouts: 30 s disconnect grace, 60 s before declaring failed, 2 s
	// keepalive — matches gortc's production values. Override any/all of them
	// via FactoryOptions.ICE* fields for unstable network environments.
	disconnect := opts.ICEDisconnectTimeout
	if disconnect == 0 {
		disconnect = 30 * time.Second
	}
	failed := opts.ICEFailedTimeout
	if failed == 0 {
		failed = 60 * time.Second
	}
	keepalive := opts.ICEKeepaliveInterval
	if keepalive == 0 {
		keepalive = 2 * time.Second
	}
	settings.SetICETimeouts(disconnect, failed, keepalive)
	// Skip virtual / VPN interfaces — gathering candidates on them slows ICE
	// and produces unreachable pairs. Captured by the closure once; each
	// candidate-gather pass does N substring scans rather than re-walking
	// a literal slice every time.
	settings.SetInterfaceFilter(makeInterfaceFilter())

	f := &Factory{
		settings:   settings,
		log:        log,
		certPool:   NewCertPool(opts.CertPoolSize, log),
		iceServers: opts.ICEServers,
	}
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

func registerCodecs(m *webrtc.MediaEngine) error {
	// Audio: Opus PT 111 (Telegram standard).
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    uint32(models.OpusSampleRate),
			Channels:     2,
			SDPFmtpLine:  "minptime=10;useinbandfec=1",
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

// Close releases the shared UDP mux (if any) and cert pool.
func (f *Factory) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	pool := f.certPool
	mux := f.udpMux
	f.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	if mux != nil {
		return mux.Close()
	}
	return nil
}
