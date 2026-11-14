package keystore

import (
	"context"
	"fmt"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// GCPKMSClient wraps/unwraps DEKs through a GCP Cloud KMS symmetric key.
// Decrypt only ever needs the CryptoKey resource name (never a specific
// version). Cloud KMS resolves which key version encrypted a given
// ciphertext from metadata embedded in the ciphertext itself. So rotating
// the underlying key in GCP requires no code change here.
type GCPKMSClient struct {
	client  *kms.KeyManagementClient
	keyName string // full CryptoKey resource name
}

var _ Rewrapper = (*GCPKMSClient)(nil)

func NewGCPKMSClient(ctx context.Context, keyName string) (*GCPKMSClient, error) {
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating KMS client: %w", err)
	}

	return &GCPKMSClient{client: client, keyName: keyName}, nil
}

func (c *GCPKMSClient) Encrypt(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	resp, err := c.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:      c.keyName,
		Plaintext: plaintextDEK,
	})
	if err != nil {
		return nil, "", err
	}

	return resp.Ciphertext, c.keyName, nil
}

func (c *GCPKMSClient) Decrypt(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	resp, err := c.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:       keyID,
		Ciphertext: wrapped,
	})
	if err != nil {
		return nil, err
	}

	return resp.Plaintext, nil
}

// Rewrap has no server-side equivalent in Cloud KMS, so the DEK is decrypted
// here and encrypted again, which wraps it under the key's primary version.
// The plaintext DEK is held in memory only between the two calls.
func (c *GCPKMSClient) Rewrap(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	dek, err := c.Decrypt(ctx, wrapped, keyID)
	if err != nil {
		return nil, err
	}
	defer Zero(dek)

	resp, err := c.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:      keyID,
		Plaintext: dek,
	})
	if err != nil {
		return nil, err
	}

	return resp.Ciphertext, nil
}

func (c *GCPKMSClient) Close() error {
	return c.client.Close()
}
