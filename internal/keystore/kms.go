package keystore

import (
	"context"
	"fmt"

	"github.com/fromScratchStudioGr/ostiary/internal/config"
)

// KMSClient wraps/unwraps a Data Encryption Key (DEK). It never sees the
// provider API key itself — only the small (32-byte) DEK that in turn
// encrypts the provider key. This is what makes envelope encryption cheap:
// KMS calls are on a tiny fixed-size secret, not on arbitrary-length data.
//
// Implementations: LocalKMS (dev only), and later AWSKMSClient /
// GCPKMSClient / VaultKMSClient. Swapping implementations requires no
// changes to crypto.go or envelope.go — only to how the KMSClient is
// constructed at startup.
type KMSClient interface {
	// Encrypt wraps a plaintext DEK, returning the wrapped bytes and an
	// identifier for which key/version did the wrapping. Store both.
	Encrypt(ctx context.Context, plaintextDEK []byte) (wrapped []byte, keyID string, err error)

	// Decrypt unwraps a previously wrapped DEK. keyID tells the
	// implementation which underlying key/version to use — for a real
	// KMS this matters across key rotations; for LocalKMS it's mostly a
	// sanity check.
	Decrypt(ctx context.Context, wrapped []byte, keyID string) (plaintextDEK []byte, err error)
}

// Rewrapper is implemented by the KMS clients whose key can be rotated
// (OpenBao and GCP, not the local one). After a rotation, wrapped DEKs still
// decrypt but sit on the old key version; Rewrap moves one to the key's
// current version.
type Rewrapper interface {
	// Rewrap returns the wrapped DEK re-wrapped under the current version of
	// the key keyID, the one that wrapped it. The key id stays the same.
	Rewrap(ctx context.Context, wrapped []byte, keyID string) (rewrapped []byte, err error)
}

// New builds the KMS client the config asks for: GCP or OpenBao by name, and
// the local one for anything else. The returned func releases it.
func New(cfg config.KMS) (KMSClient, func(), error) {
	switch cfg.Provider {
	case "gcp":
		gcpKMS, err := NewGCPKMSClient(context.Background(), cfg.GCPKeyName)
		if err != nil {
			return nil, nil, fmt.Errorf("gcp: %w", err)
		}
		return gcpKMS, func() { gcpKMS.Close() }, nil
	case "openbao":
		openbaoKMS, err := NewVaultKMSClient(cfg.OpenbaoVaultAddr, cfg.OpenbaoVaultToken, cfg.OpenbaoVaultKeyname)
		if err != nil {
			return nil, nil, fmt.Errorf("openbao: %w", err)
		}
		return openbaoKMS, func() {}, nil
	default:
		localKMS, err := NewLocalKMSFromEnv()
		if err != nil {
			return nil, nil, fmt.Errorf("local: %w", err)
		}
		return localKMS, func() {}, nil
	}
}
