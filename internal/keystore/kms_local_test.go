package keystore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestNewLocalKMSFromEnv(t *testing.T) {
	t.Run("reads a base64-encoded 32-byte key from LOCAL_KMS_MASTER_KEY", func(t *testing.T) {
		key := GenerateDEK()
		t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(key))

		kms, err := NewLocalKMSFromEnv()
		if err != nil {
			t.Fatalf("NewLocalKMSFromEnv: %v", err)
		}

		if !bytes.Equal(kms.masterKey, key) {
			t.Fatal("the master key isn't the one that was set in the environment")
		}
	})

	t.Run("rejects a value that isn't usable, naming the setting without echoing the key", func(t *testing.T) {
		keyOf := func(size int) string {
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, size))
		}
		tests := []struct {
			name  string
			value string
		}{
			{name: "not set", value: ""},
			{name: "not base64", value: "this is not base64!"},
			{name: "a 32-byte key in base64 without its padding", value: strings.TrimRight(keyOf(32), "=")},
			{name: "16 bytes", value: keyOf(16)},
			{name: "24 bytes (a valid AES size, but not the 32 we require)", value: keyOf(24)},
			{name: "33 bytes", value: keyOf(33)},
			{name: "64 bytes", value: keyOf(64)},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Setenv("LOCAL_KMS_MASTER_KEY", tt.value)

				kms, err := NewLocalKMSFromEnv()

				if err == nil {
					t.Fatal("got nil error, want one")
				}
				if kms != nil {
					t.Fatal("returned a LocalKMS alongside the error")
				}
				// The operator reading the startup log has to know which
				// setting to fix.
				if !strings.Contains(err.Error(), "LOCAL_KMS_MASTER_KEY") {
					t.Errorf("error %q doesn't name LOCAL_KMS_MASTER_KEY", err)
				}
				// The value is the secret itself, so it must never reach a log.
				if tt.value != "" && strings.Contains(err.Error(), tt.value) {
					t.Errorf("error %q contains the key value", err)
				}
			})
		}
	})

	t.Run("says how many bytes a wrong-sized key decoded to", func(t *testing.T) {
		t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 16)))

		_, err := NewLocalKMSFromEnv()

		// "Valid base64 but wrong size" is the easy mistake to make (e.g. a
		// 128-bit key), and the length is what tells you that's what happened.
		if err == nil || !strings.Contains(err.Error(), "16") || !strings.Contains(err.Error(), "32") {
			t.Fatalf("error = %v, want one mentioning both the 16 bytes found and the 32 required", err)
		}
	})
}

func TestLocalKMSEncrypt(t *testing.T) {
	newKMS := func() *LocalKMS { return &LocalKMS{masterKey: GenerateDEK()} }

	t.Run("labels the wrapped DEK with the key id that's stored beside it", func(t *testing.T) {
		// The id is persisted with every wrapped DEK (provider_keys.kms_key_id)
		// and Decrypt refuses any other, so changing it would strand every
		// key already stored. It's spelled out here on purpose.
		_, keyID, err := newKMS().Encrypt(context.Background(), GenerateDEK())
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		if keyID != "local-dev-v1" {
			t.Fatalf("key id = %q, want %q", keyID, "local-dev-v1")
		}
	})

	t.Run("wraps a DEK that Decrypt unwraps with the key id it returned", func(t *testing.T) {
		kms := newKMS()
		dek := GenerateDEK()

		wrapped, keyID, err := kms.Encrypt(context.Background(), dek)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got, err := kms.Decrypt(context.Background(), wrapped, keyID)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		if !bytes.Equal(got, dek) {
			t.Fatal("the unwrapped DEK differs from the one that was wrapped")
		}
	})

	t.Run("doesn't modify the DEK it wraps", func(t *testing.T) {
		// The caller still needs the DEK after wrapping it, to encrypt the
		// provider key, and zeroes it only when it's done.
		dek := GenerateDEK()
		before := bytes.Clone(dek)

		if _, _, err := newKMS().Encrypt(context.Background(), dek); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		if !bytes.Equal(dek, before) {
			t.Fatal("Encrypt changed the DEK")
		}
	})

	t.Run("returns an error and nothing else when the master key isn't a valid AES size", func(t *testing.T) {
		for _, size := range []int{0, 31, 33} {
			kms := &LocalKMS{masterKey: make([]byte, size)}

			wrapped, keyID, err := kms.Encrypt(context.Background(), GenerateDEK())

			if err == nil {
				t.Errorf("a %d-byte master key was accepted", size)
			}
			if wrapped != nil || keyID != "" {
				t.Errorf("a %d-byte master key returned data alongside the error", size)
			}
		}
	})
}

func TestLocalKMSDecrypt(t *testing.T) {
	ctx := context.Background()
	newKMS := func() *LocalKMS { return &LocalKMS{masterKey: GenerateDEK()} }

	t.Run("rejects a key id other than the one it issues", func(t *testing.T) {
		kms := newKMS()
		// The wrapped DEK is genuine and the master key is the right one, so
		// the only thing wrong is the id.
		wrapped, _, err := kms.Encrypt(ctx, GenerateDEK())
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		for _, other := range []string{"", "local-dev-v2", "projects/p/locations/l/keyRings/r/cryptoKeys/k"} {
			dek, err := kms.Decrypt(ctx, wrapped, other)

			if err == nil {
				t.Errorf("key id %q was accepted", other)
				continue
			}
			if dek != nil {
				t.Errorf("key id %q returned data alongside the error", other)
			}
			if other != "" && !strings.Contains(err.Error(), other) {
				t.Errorf("error %q doesn't say which key id was rejected", err)
			}
		}
	})

	t.Run("rejects wrapped data too short to hold a nonce and a ciphertext, without panicking", func(t *testing.T) {
		inputs := map[string][]byte{"nil": nil, "empty": {}}
		// 12 bytes is exactly a nonce: it gets past the length check, with an
		// empty ciphertext after it.
		for _, size := range []int{1, 11, 12} {
			inputs[fmt.Sprintf("%d bytes", size)] = make([]byte, size)
		}

		for name, wrapped := range inputs {
			dek, err := newKMS().Decrypt(ctx, wrapped, localKMSKeyID)

			if err == nil {
				t.Errorf("%s was accepted", name)
			}
			if dek != nil {
				t.Errorf("%s returned data alongside the error", name)
			}
		}
	})

	t.Run("returns an error and nothing else when the master key isn't a valid AES size", func(t *testing.T) {
		for _, size := range []int{0, 31, 33} {
			kms := &LocalKMS{masterKey: make([]byte, size)}

			dek, err := kms.Decrypt(ctx, make([]byte, 60), localKMSKeyID)

			if err == nil {
				t.Errorf("a %d-byte master key was accepted", size)
			}
			if dek != nil {
				t.Errorf("a %d-byte master key returned data alongside the error", size)
			}
		}
	})

	t.Run("unwraps a DEK wrapped in the layout already stored", func(t *testing.T) {
		// A known value, built straight from the standard library with fixed
		// inputs, independently of this package: master key 00..1f, nonce
		// a0..ab, DEK 60..7f, laid out as nonce followed by the sealed DEK,
		// with no additional data. Every DEK wrapped so far has this layout,
		// so if this stops decrypting, the stored provider keys can no
		// longer be unwrapped.
		mustHex := func(s string) []byte {
			b, err := hex.DecodeString(s)
			if err != nil {
				t.Fatalf("bad test vector: %v", err)
			}
			return b
		}
		kms := &LocalKMS{masterKey: mustHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")}
		wrapped := mustHex("a0a1a2a3a4a5a6a7a8a9aaab86791e4e21ae64d80a0cedb86b17aeb100dd2b63e6c2341be4775cfd03d60b7e19885ecea9ebaa715ad5bc421504a526")
		want := mustHex("606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f")

		got, err := kms.Decrypt(ctx, wrapped, "local-dev-v1")
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		if !bytes.Equal(got, want) {
			t.Fatalf("unwrapped DEK = %x, want %x", got, want)
		}
	})

	t.Run("leaves the stored wrapped DEK intact, even after the caller zeroes the result", func(t *testing.T) {
		// The wrapped DEK comes from a session entry the proxy shares between
		// requests, and callers zero each unwrapped DEK once they're done with
		// it. Neither may reach back into the stored bytes.
		kms := newKMS()
		dek := GenerateDEK()
		wrapped, keyID, err := kms.Encrypt(ctx, dek)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		before := bytes.Clone(wrapped)

		first, err := kms.Decrypt(ctx, wrapped, keyID)
		if err != nil {
			t.Fatalf("first Decrypt: %v", err)
		}
		Zero(first)

		if !bytes.Equal(wrapped, before) {
			t.Fatal("Decrypt or Zero changed the wrapped DEK")
		}
		second, err := kms.Decrypt(ctx, wrapped, keyID)
		if err != nil {
			t.Fatalf("second Decrypt: %v", err)
		}
		if !bytes.Equal(second, dek) {
			t.Fatal("the second unwrap returned a different DEK")
		}
	})
}
