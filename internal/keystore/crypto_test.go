package keystore

import (
	"bytes"
	"testing"
)

func TestGenerateDEK(t *testing.T) {
	t.Run("returns a 32-byte key, the size AES-256 needs", func(t *testing.T) {
		dek := GenerateDEK()

		if len(dek) != 32 {
			t.Fatalf("key length = %d, want 32", len(dek))
		}
	})
}

func TestEncryptWithDEK(t *testing.T) {
	t.Run("round trips through DecryptWithDEK", func(t *testing.T) {
		everyByte := make([]byte, 256)
		for i := range everyByte {
			everyByte[i] = byte(i)
		}
		plaintexts := map[string][]byte{
			"an api key":       []byte("sk-ant-api03-abcdef"),
			"empty":            {},
			"every byte value": everyByte,
			"large":            bytes.Repeat([]byte("x"), 1<<20),
		}

		for name, plaintext := range plaintexts {
			t.Run(name, func(t *testing.T) {
				dek := GenerateDEK()

				ciphertext, nonce, err := EncryptWithDEK(dek, plaintext)
				if err != nil {
					t.Fatalf("EncryptWithDEK: %v", err)
				}
				got, err := DecryptWithDEK(dek, nonce, ciphertext)
				if err != nil {
					t.Fatalf("DecryptWithDEK: %v", err)
				}
				if !bytes.Equal(got, plaintext) {
					t.Fatalf("round trip returned %d bytes, want the original %d", len(got), len(plaintext))
				}
			})
		}
	})

	t.Run("rejects a key that isn't a valid AES size and returns nothing", func(t *testing.T) {
		for _, size := range []int{0, 15, 31, 33, 64} {
			ciphertext, nonce, err := EncryptWithDEK(make([]byte, size), []byte("sk-provider-api-key"))
			if err == nil {
				t.Errorf("a %d-byte key was accepted", size)
			}
			if ciphertext != nil || nonce != nil {
				t.Errorf("a %d-byte key returned data alongside the error", size)
			}
		}
	})

	t.Run("doesn't modify the plaintext or the key", func(t *testing.T) {
		dek := GenerateDEK()
		plaintext := []byte("sk-provider-api-key")
		dekBefore, plaintextBefore := bytes.Clone(dek), bytes.Clone(plaintext)

		if _, _, err := EncryptWithDEK(dek, plaintext); err != nil {
			t.Fatalf("EncryptWithDEK: %v", err)
		}

		if !bytes.Equal(dek, dekBefore) || !bytes.Equal(plaintext, plaintextBefore) {
			t.Fatal("EncryptWithDEK changed one of its inputs")
		}
	})
}

func TestDecryptWithDEK(t *testing.T) {
	t.Run("rejects a key that isn't a valid AES size and returns nothing", func(t *testing.T) {
		ciphertext, nonce, err := EncryptWithDEK(GenerateDEK(), []byte("sk-provider-api-key"))
		if err != nil {
			t.Fatalf("EncryptWithDEK: %v", err)
		}

		for _, size := range []int{0, 15, 31, 33, 64} {
			plaintext, err := DecryptWithDEK(make([]byte, size), nonce, ciphertext)
			if err == nil {
				t.Errorf("a %d-byte key was accepted", size)
			}
			if plaintext != nil {
				t.Errorf("a %d-byte key returned data alongside the error", size)
			}
		}
	})

	t.Run("leaves the stored ciphertext intact, even after the caller zeroes the plaintext", func(t *testing.T) {
		// The ciphertext and nonce come from a session entry the proxy keeps
		// in memory and shares between requests, while callers zero each
		// decrypted key once they've used it. Decrypting in place, or handing
		// back a slice that shares the ciphertext's memory, would corrupt the
		// entry for every later request.
		dek := GenerateDEK()
		want := []byte("sk-provider-api-key")
		ciphertext, nonce, err := EncryptWithDEK(dek, want)
		if err != nil {
			t.Fatalf("EncryptWithDEK: %v", err)
		}
		dekBefore, nonceBefore, ciphertextBefore := bytes.Clone(dek), bytes.Clone(nonce), bytes.Clone(ciphertext)

		first, err := DecryptWithDEK(dek, nonce, ciphertext)
		if err != nil {
			t.Fatalf("first DecryptWithDEK: %v", err)
		}
		Zero(first)

		if !bytes.Equal(dek, dekBefore) || !bytes.Equal(nonce, nonceBefore) || !bytes.Equal(ciphertext, ciphertextBefore) {
			t.Fatal("DecryptWithDEK or Zero changed the key, nonce or ciphertext")
		}
		second, err := DecryptWithDEK(dek, nonce, ciphertext)
		if err != nil {
			t.Fatalf("second DecryptWithDEK: %v", err)
		}
		if !bytes.Equal(second, want) {
			t.Fatalf("second decryption returned %q, want %q", second, want)
		}
	})
}

func TestZero(t *testing.T) {
	t.Run("overwrites every byte of the slice and nothing beyond it", func(t *testing.T) {
		buf := bytes.Repeat([]byte{0xff}, 8)

		Zero(buf[2:6])

		want := []byte{0xff, 0xff, 0, 0, 0, 0, 0xff, 0xff}
		if !bytes.Equal(buf, want) {
			t.Fatalf("buffer = %x, want %x", buf, want)
		}
	})

	t.Run("accepts nil and empty slices", func(t *testing.T) {
		// Callers zero a key on error paths too, where it may not exist.
		Zero(nil)
		Zero([]byte{})
	})
}
