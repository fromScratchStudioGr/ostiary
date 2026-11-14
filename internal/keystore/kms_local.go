package keystore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// localKMSKeyID is a fixed identifier since there's only ever one "key
// version" in local dev. A real KMS implementation would return a
// key/version ARN or resource name here instead.
const localKMSKeyID = "local-dev-v1"

// IMPORTANT: This is only for demonstration purposes. Use it only
// during development. In production, switch to an actual KMS, like
// GCP KMS.
type LocalKMS struct {
	masterKey []byte // 32 bytes, AES-256
}

func NewLocalKMSFromEnv() (*LocalKMS, error) {
	encoded := os.Getenv("LOCAL_KMS_MASTER_KEY")
	if encoded == "" {
		return nil, errors.New("LOCAL_KMS_MASTER_KEY not set")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("Decoding LOCAL_KMS_MASTER_KEY: %w", err)
	}

	if len(key) != 32 {
		return nil, fmt.Errorf("LOCAL_KMS_MASTER_KEY must decode to 32 bytes, but got %d", len(key))
	}

	return &LocalKMS{masterKey: key}, nil
}

func (l *LocalKMS) Encrypt(_ context.Context, plaintextDEK []byte) ([]byte, string, error) {
	gcm, err := l.gcm()
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)

	wrapped := gcm.Seal(nonce, nonce, plaintextDEK, nil)
	return wrapped, localKMSKeyID, nil
}

func (l *LocalKMS) Decrypt(_ context.Context, wrapped []byte, keyID string) ([]byte, error) {
	if keyID != localKMSKeyID {
		return nil, fmt.Errorf("Unknown local KMS key id: %s", keyID)
	}
	gcm, err := l.gcm()
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(wrapped) < nonceSize {
		return nil, errors.New("Wrapped DEK too short")
	}

	nonce, ciphertext := wrapped[:nonceSize], wrapped[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (l *LocalKMS) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(l.masterKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
