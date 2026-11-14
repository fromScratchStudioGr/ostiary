package keystore

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	vault "github.com/openbao/openbao/api/v2"
)

// VaultKMSClient wraps/unwraps DEKs through an OpenBao (or Vault-API-
// compatible) Transit secrets engine key. Unlike GCPKMSClient, OpenBao's
// ciphertext is a self-describing string (e.g. "vault:v1:..."), not raw
// bytes, so it's carried as []byte purely to satisfy the KMSClient
// interface - no binary encoding happens.
type VaultKMSClient struct {
	client  *vault.Client
	keyName string // name of the transit key, e.g. "ostiary-dek-wrap"
}

var _ Rewrapper = (*VaultKMSClient)(nil)

// NewVaultKMSClient connects to an OpenBao server at addr, authenticating
// with token, and targets the named transit key for all Encrypt/Decrypt
// calls. It does not verify the key exists - that surfaces as an error on
// first use. A missing address, token or key name is rejected up front
// instead, so a missing setting stops startup rather than the first request.
//
// Only the arguments choose the server: BAO_AGENT_ADDR, which the OpenBao
// client would otherwise prefer over the address it's given, is ignored.
func NewVaultKMSClient(addr, token, keyName string) (*VaultKMSClient, error) {
	var missing []string
	if addr == "" {
		missing = append(missing, "OPENBAO_VAULT_ADDR")
	}
	if token == "" {
		missing = append(missing, "OPENBAO_VAULT_TOKEN")
	}
	if keyName == "" {
		missing = append(missing, "OPENBAO_VAULT_KEYNAME")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("OpenBao KMS is not configured: %s not set", strings.Join(missing, ", "))
	}

	config := vault.DefaultConfig()
	config.Address = addr
	// DefaultConfig fills this from BAO_AGENT_ADDR, and the client then
	// prefers it to Address, which would silently redirect every request.
	config.AgentAddress = ""

	client, err := vault.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("creating OpenBao client: %w", err)
	}
	client.SetToken(token)

	return &VaultKMSClient{client: client, keyName: keyName}, nil
}

func (c *VaultKMSClient) Encrypt(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	secret, err := c.client.Logical().WriteWithContext(ctx, "transit/encrypt/"+c.keyName, map[string]any{
		"plaintext": base64.StdEncoding.EncodeToString(plaintextDEK),
	})
	if err != nil {
		return nil, "", fmt.Errorf("transit encrypt: %w", err)
	}

	if secret == nil {
		// The OpenBao client returns a nil secret, and no error, for a
		// response without a body.
		return nil, "", fmt.Errorf("transit encrypt: empty response")
	}

	ciphertext, ok := secret.Data["ciphertext"].(string)
	if !ok {
		return nil, "", fmt.Errorf("transit encrypt: response missing ciphertext")
	}

	return []byte(ciphertext), c.keyName, nil
}

func (c *VaultKMSClient) Decrypt(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	secret, err := c.client.Logical().WriteWithContext(ctx, "transit/decrypt/"+keyID, map[string]any{
		"ciphertext": string(wrapped),
	})
	if err != nil {
		return nil, fmt.Errorf("transit decrypt: %w", err)
	}

	if secret == nil {
		// The OpenBao client returns a nil secret, and no error, for a
		// response without a body.
		return nil, fmt.Errorf("transit decrypt: empty response")
	}

	plaintextB64, ok := secret.Data["plaintext"].(string)
	if !ok {
		return nil, fmt.Errorf("transit decrypt: response missing plaintext")
	}

	plaintextDEK, err := base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		return nil, fmt.Errorf("transit decrypt: decoding plaintext: %w", err)
	}

	return plaintextDEK, nil
}

// Rewrap has the transit engine re-encrypt the wrapped DEK under the key's
// latest version. The DEK itself never leaves OpenBao.
func (c *VaultKMSClient) Rewrap(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	secret, err := c.client.Logical().WriteWithContext(ctx, "transit/rewrap/"+keyID, map[string]any{
		"ciphertext": string(wrapped),
	})
	if err != nil {
		return nil, fmt.Errorf("transit rewrap: %w", err)
	}

	if secret == nil {
		// The OpenBao client returns a nil secret, and no error, for a
		// response without a body.
		return nil, fmt.Errorf("transit rewrap: empty response")
	}

	ciphertext, ok := secret.Data["ciphertext"].(string)
	if !ok {
		return nil, fmt.Errorf("transit rewrap: response missing ciphertext")
	}

	return []byte(ciphertext), nil
}
