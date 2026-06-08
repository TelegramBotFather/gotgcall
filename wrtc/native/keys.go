package native

import (
	"fmt"

	"github.com/pion/dtls/v3"
	"github.com/pion/srtp/v3"
)

// dtlsSrtpExporterLabel is the RFC 5764 label used to derive SRTP keys
// from the DTLS-SRTP master secret.
const dtlsSrtpExporterLabel = "EXTRACTOR-dtls_srtp"

// deriveSRTPContext extracts SRTP master key + salt from a completed
// DTLS handshake (per RFC 5764) and constructs a send-only SRTP Context.
// We are the DTLS server (setup=passive on the JoinGroupCall payload),
// so the server-side half of the keying material is what feeds OUR
// encryption.
//
// Returned profile is the negotiated DTLS protection profile translated
// into pion/srtp's namespace. Caller closes nothing — Context is
// stateless aside from per-SSRC encryption state.
func deriveSRTPContext(dtlsConn *dtls.Conn) (*srtp.Context, srtp.ProtectionProfile, error) {
	state, ok := dtlsConn.ConnectionState()
	if !ok {
		return nil, 0, fmt.Errorf("dtls handshake not complete")
	}
	rawProfile, ok := dtlsConn.SelectedSRTPProtectionProfile()
	if !ok {
		return nil, 0, fmt.Errorf("no SRTP protection profile negotiated")
	}
	profile, err := translateProfile(rawProfile)
	if err != nil {
		return nil, 0, err
	}
	keyLen, err := profile.KeyLen()
	if err != nil {
		return nil, 0, fmt.Errorf("profile key len: %w", err)
	}
	saltLen, err := profile.SaltLen()
	if err != nil {
		return nil, 0, fmt.Errorf("profile salt len: %w", err)
	}
	material, err := state.ExportKeyingMaterial(dtlsSrtpExporterLabel, nil, 2*(keyLen+saltLen))
	if err != nil {
		return nil, 0, fmt.Errorf("export keying material: %w", err)
	}
	// Layout per RFC 5764 §4.2: client_key || server_key || client_salt || server_salt.
	// We are SERVER → use server_key + server_salt for encryption.
	off := 0
	clientKey := material[off : off+keyLen]
	off += keyLen
	serverKey := material[off : off+keyLen]
	off += keyLen
	clientSalt := material[off : off+saltLen]
	off += saltLen
	serverSalt := material[off : off+saltLen]
	_ = clientKey
	_ = clientSalt

	ctx, err := srtp.CreateContext(serverKey, serverSalt, profile)
	if err != nil {
		return nil, 0, fmt.Errorf("create srtp context: %w", err)
	}
	return ctx, profile, nil
}

// translateProfile maps the IANA-assigned DTLS-SRTP profile constant to
// pion/srtp's ProtectionProfile type. The numerical values are identical
// (both packages use the same IANA registry) but the Go types differ.
func translateProfile(p dtls.SRTPProtectionProfile) (srtp.ProtectionProfile, error) {
	switch p {
	case dtls.SRTP_AEAD_AES_128_GCM:
		return srtp.ProtectionProfileAeadAes128Gcm, nil
	case dtls.SRTP_AES128_CM_HMAC_SHA1_80:
		return srtp.ProtectionProfileAes128CmHmacSha1_80, nil
	default:
		return 0, fmt.Errorf("unsupported srtp profile %v", p)
	}
}
