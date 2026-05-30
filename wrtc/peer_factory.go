package wrtc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/annihilatorrrr/gotgcall/models"
)

// Factory creates pion PeerConnections. One Factory per Client; shared
// across all calls, optionally backed by a shared UDP mux for high
// concurrency setups.
type Factory struct {
	udpMux   ice.UDPMux
	api      *webrtc.API
	log      *slog.Logger
	certPool *CertPool
	settings webrtc.SettingEngine
	mu       sync.Mutex
	closed   bool
}

type FactoryOptions struct {
	Logger       *slog.Logger
	SharedUDPMux bool
	CertPoolSize int
}

func NewFactory(opts FactoryOptions) (*Factory, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	settings := webrtc.SettingEngine{LoggerFactory: newFilteringLoggerFactory()}
	settings.SetIncludeLoopbackCandidate(false)
	settings.SetLite(false)

	f := &Factory{
		settings: settings,
		log:      log,
		certPool: NewCertPool(opts.CertPoolSize, log),
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
	// Telegram's SFU requires ssrc-audio-level on every outbound audio
	// packet — without it, audio is treated as silence and not forwarded.
	if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: audioLevelURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: absSendTimeURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
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
