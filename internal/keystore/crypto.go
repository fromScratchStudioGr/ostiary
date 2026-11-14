package keystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
)

// GenerateDEK returns a fresh random 32-byte data encryption key.
// A new DEK should be generated per provider key, not reused.
func GenerateDEK() []byte {
	dek := make([]byte, 32)
	rand.Read(dek)
	return dek
}

// EncryptWithDEK encrypts plaintext using AES-256-GCM with the given DEK.
// Returns ciphertext and the nonce used — store both, the nonce is not
// secret but is required to decrypt.
func EncryptWithDEK(dek, plaintext []byte) (ciphertext, nonce []byte, err error) {
	gcm, err := gcmFor(dek)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// DecryptWithDEK reverses EncryptWithDEK.
func DecryptWithDEK(dek, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := gcmFor(dek)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
