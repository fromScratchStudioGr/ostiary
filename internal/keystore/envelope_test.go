package keystore

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// recordingKMS wraps a real KMSClient and remembers what SealProviderKey and
// OpenProviderKey exchanged with it, so a test can look at the plaintext DEK
// afterwards. Everything is still delegated to the wrapped client.
type recordingKMS struct {
	KMSClient
	ctx       context.Context
	dek       []byte // the slice itself, to see what became of it
	dekAtCall []byte // a copy taken when Encrypt was called

	decryptCtx        context.Context
	unwrapped         []byte // the DEK Decrypt returned, as the slice itself
	unwrappedAtReturn []byte // a copy taken when Decrypt returned
}

func (r *recordingKMS) Decrypt(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	dek, err := r.KMSClient.Decrypt(ctx, wrapped, keyID)
	r.decryptCtx = ctx
	r.unwrapped = dek
	r.unwrappedAtReturn = bytes.Clone(dek)
	return dek, err
}

func (r *recordingKMS) Encrypt(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	r.ctx = ctx
	r.dek = plaintextDEK
	r.dekAtCall = bytes.Clone(plaintextDEK)
	return r.KMSClient.Encrypt(ctx, plaintextDEK)
}

func isZero(b []byte) bool {
	return bytes.Equal(b, make([]byte, len(b)))
}

func TestSealProviderKey(t *testing.T) {
	ctx := context.Background()
	plaintext := []byte("sk-ant-api03-provider-key")
	newKMS := func() *LocalKMS { return &LocalKMS{masterKey: GenerateDEK()} }

	t.Run("seals a provider key that OpenProviderKey recovers", func(t *testing.T) {
		kms := newKMS()

		ciphertext, nonce, encryptedDEK, kmsKeyID, err := SealProviderKey(ctx, kms, plaintext)
		if err != nil {
			t.Fatalf("SealProviderKey: %v", err)
		}
		got, err := OpenProviderKey(ctx, kms, ciphertext, nonce, encryptedDEK, kmsKeyID)
		if err != nil {
			t.Fatalf("OpenProviderKey: %v", err)
		}

		if !bytes.Equal(got, plaintext) {
			t.Fatalf("recovered %q, want %q", got, plaintext)
		}
	})

	t.Run("zeroes the plaintext DEK once the provider key is sealed", func(t *testing.T) {
		rec := &recordingKMS{KMSClient: newKMS()}

		if _, _, _, _, err := SealProviderKey(ctx, rec, plaintext); err != nil {
			t.Fatalf("SealProviderKey: %v", err)
		}

		if len(rec.dekAtCall) != 32 || isZero(rec.dekAtCall) {
			t.Fatal("the KMS wasn't given a real DEK, so the check below would prove nothing")
		}
		if !isZero(rec.dek) {
			t.Fatal("the plaintext DEK was left in memory after sealing")
		}
	})

	t.Run("returns nothing, and still zeroes the DEK, when the KMS fails", func(t *testing.T) {
		// A master key of the wrong size makes the real LocalKMS fail. A
		// ciphertext without its wrapped DEK is unrecoverable, so none of the
		// results may be returned to be stored.
		rec := &recordingKMS{KMSClient: &LocalKMS{masterKey: make([]byte, 31)}}

		ciphertext, nonce, encryptedDEK, kmsKeyID, err := SealProviderKey(ctx, rec, plaintext)

		if err == nil {
			t.Fatal("SealProviderKey returned nil, want the KMS error")
		}
		if ciphertext != nil || nonce != nil || encryptedDEK != nil || kmsKeyID != "" {
			t.Fatal("returned data alongside the error")
		}
		if len(rec.dekAtCall) != 32 || isZero(rec.dekAtCall) {
			t.Fatal("the KMS wasn't given a real DEK, so the check below would prove nothing")
		}
		if !isZero(rec.dek) {
			t.Fatal("the plaintext DEK was left in memory after the failure")
		}
	})

	t.Run("uses a different DEK for every provider key", func(t *testing.T) {
		// Sharing a DEK between provider keys would defeat the point of
		// wrapping one per key.
		kms := newKMS()
		first, second := &recordingKMS{KMSClient: kms}, &recordingKMS{KMSClient: kms}

		if _, _, _, _, err := SealProviderKey(ctx, first, plaintext); err != nil {
			t.Fatalf("first SealProviderKey: %v", err)
		}
		if _, _, _, _, err := SealProviderKey(ctx, second, plaintext); err != nil {
			t.Fatalf("second SealProviderKey: %v", err)
		}

		if bytes.Equal(first.dekAtCall, second.dekAtCall) {
			t.Fatal("two provider keys were sealed under the same DEK")
		}
	})

	t.Run("passes the caller's context on to the KMS", func(t *testing.T) {
		// Cancellation and deadlines have to reach a remote KMS, or a request
		// that's gone would still wait on it.
		type ctxKey struct{}
		callerCtx := context.WithValue(ctx, ctxKey{}, "the caller")
		rec := &recordingKMS{KMSClient: newKMS()}

		if _, _, _, _, err := SealProviderKey(callerCtx, rec, plaintext); err != nil {
			t.Fatalf("SealProviderKey: %v", err)
		}

		if rec.ctx == nil || rec.ctx.Value(ctxKey{}) != "the caller" {
			t.Fatal("the KMS wasn't given the caller's context")
		}
	})
}

func TestOpenProviderKey(t *testing.T) {
	ctx := context.Background()
	plaintext := []byte("sk-ant-api03-provider-key")
	newKMS := func() *LocalKMS { return &LocalKMS{masterKey: GenerateDEK()} }

	// seal stores a provider key the way the admin does, so the tests open
	// something real.
	seal := func(t *testing.T, kms KMSClient) (ciphertext, nonce, encryptedDEK []byte, keyID string) {
		t.Helper()
		ciphertext, nonce, encryptedDEK, keyID, err := SealProviderKey(ctx, kms, plaintext)
		if err != nil {
			t.Fatalf("SealProviderKey: %v", err)
		}
		return ciphertext, nonce, encryptedDEK, keyID
	}

	t.Run("returns the KMS's error and nothing else when the DEK can't be unwrapped", func(t *testing.T) {
		kms := newKMS()
		ciphertext, nonce, encryptedDEK, keyID := seal(t, kms)

		tests := []struct {
			name         string
			encryptedDEK []byte
			keyID        string
			wantInError  string
		}{
			{name: "a key id the KMS didn't issue", encryptedDEK: encryptedDEK, keyID: "local-dev-v2", wantInError: "local-dev-v2"},
			{name: "a wrapped DEK too short to be one", encryptedDEK: []byte{1, 2, 3}, keyID: keyID, wantInError: "too short"},
		}
		for _, tt := range tests {
			got, err := OpenProviderKey(ctx, kms, ciphertext, nonce, tt.encryptedDEK, tt.keyID)

			if err == nil {
				t.Errorf("%s: got nil error", tt.name)
				continue
			}
			if got != nil {
				t.Errorf("%s: returned data alongside the error", tt.name)
			}
			// The error has to be the KMS's own, which says what was wrong,
			// not a generic failure further down.
			if !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("%s: error %q doesn't mention %q", tt.name, err, tt.wantInError)
			}
		}
	})

	t.Run("zeroes the unwrapped DEK once the provider key is opened", func(t *testing.T) {
		kms := newKMS()
		ciphertext, nonce, encryptedDEK, keyID := seal(t, kms)
		rec := &recordingKMS{KMSClient: kms}

		got, err := OpenProviderKey(ctx, rec, ciphertext, nonce, encryptedDEK, keyID)
		if err != nil {
			t.Fatalf("OpenProviderKey: %v", err)
		}

		if !bytes.Equal(got, plaintext) {
			t.Fatalf("opened %q, want %q", got, plaintext)
		}
		if len(rec.unwrappedAtReturn) != 32 || isZero(rec.unwrappedAtReturn) {
			t.Fatal("the KMS didn't return a real DEK, so the check below would prove nothing")
		}
		if !isZero(rec.unwrapped) {
			t.Fatal("the plaintext DEK was left in memory after opening")
		}
	})

	t.Run("zeroes the DEK and returns no plaintext when the provider key can't be decrypted", func(t *testing.T) {
		kms := newKMS()
		ciphertext, nonce, encryptedDEK, keyID := seal(t, kms)
		damaged := bytes.Clone(ciphertext)
		damaged[0] ^= 1
		rec := &recordingKMS{KMSClient: kms}

		got, err := OpenProviderKey(ctx, rec, damaged, nonce, encryptedDEK, keyID)

		if err == nil {
			t.Fatal("OpenProviderKey returned nil for a damaged ciphertext")
		}
		if got != nil {
			t.Fatal("returned data alongside the error")
		}
		if len(rec.unwrappedAtReturn) != 32 || isZero(rec.unwrappedAtReturn) {
			t.Fatal("the KMS didn't return a real DEK, so the check below would prove nothing")
		}
		if !isZero(rec.unwrapped) {
			t.Fatal("the plaintext DEK was left in memory after the failure")
		}
	})

	t.Run("passes the caller's context on to the KMS", func(t *testing.T) {
		// Cancellation and deadlines have to reach a remote KMS, or a request
		// that's gone would still wait on it.
		kms := newKMS()
		ciphertext, nonce, encryptedDEK, keyID := seal(t, kms)
		type ctxKey struct{}
		callerCtx := context.WithValue(ctx, ctxKey{}, "the caller")
		rec := &recordingKMS{KMSClient: kms}

		if _, err := OpenProviderKey(callerCtx, rec, ciphertext, nonce, encryptedDEK, keyID); err != nil {
			t.Fatalf("OpenProviderKey: %v", err)
		}

		if rec.decryptCtx == nil || rec.decryptCtx.Value(ctxKey{}) != "the caller" {
			t.Fatal("the KMS wasn't given the caller's context")
		}
	})
}
