package keystore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// writeServiceAccountKey writes a service account key file made of a freshly
// generated RSA key, so no real credential is involved, and returns its path.
// The client only needs to parse it: a token is requested on first use, not
// when the client is created.
func writeServiceAccountKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	credentials, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "test-key-id",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "kms@test-project.iam.gserviceaccount.com",
		"client_id":      "123456789",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}

	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, credentials, 0o600); err != nil {
		t.Fatalf("writing credentials: %v", err)
	}
	return path
}

const testKeyName = "projects/p/locations/l/keyRings/r/cryptoKeys/k"

// fakeKMS is a Cloud KMS server that records the Encrypt and Decrypt requests
// it gets. It answers them with the canned ciphertext or plaintext, or with
// err when that is set.
type fakeKMS struct {
	kmspb.UnimplementedKeyManagementServiceServer

	ciphertext []byte
	plaintext  []byte
	err        error
	encryptErr error // fails Encrypt only

	mu              sync.Mutex
	requests        []*kmspb.EncryptRequest
	decryptRequests []*kmspb.DecryptRequest
}

func (f *fakeKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	return &kmspb.EncryptResponse{Name: req.Name, Ciphertext: f.ciphertext}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	f.mu.Lock()
	f.decryptRequests = append(f.decryptRequests, req)
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	return &kmspb.DecryptResponse{Plaintext: f.plaintext}, nil
}

func (f *fakeKMS) received() []*kmspb.EncryptRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*kmspb.EncryptRequest(nil), f.requests...)
}

func (f *fakeKMS) receivedDecrypts() []*kmspb.DecryptRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*kmspb.DecryptRequest(nil), f.decryptRequests...)
}

// serveFakeKMS serves fake over a local port and returns a GCPKMSClient
// talking to it. The client is built directly because NewGCPKMSClient takes
// no options to redirect it.
func serveFakeKMS(t *testing.T, fake *fakeKMS) *GCPKMSClient {
	t.Helper()

	lis, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("listening: %v", listenErr)
	}
	server := grpc.NewServer()
	kmspb.RegisterKeyManagementServiceServer(server, fake)
	go server.Serve(lis)
	t.Cleanup(server.Stop)

	client, clientErr := kms.NewKeyManagementClient(context.Background(),
		option.WithEndpoint(lis.Addr().String()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if clientErr != nil {
		t.Fatalf("creating client: %v", clientErr)
	}
	t.Cleanup(func() { client.Close() })

	return &GCPKMSClient{client: client, keyName: testKeyName}
}

func TestGCPKMSClientEncrypt(t *testing.T) {
	dek := []byte("a plaintext data encryption key.")

	t.Run("asks Cloud KMS to encrypt the DEK with the configured key", func(t *testing.T) {
		fake := &fakeKMS{ciphertext: []byte("wrapped")}
		client := serveFakeKMS(t, fake)

		if _, _, err := client.Encrypt(context.Background(), dek); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		requests := fake.received()
		if len(requests) != 1 {
			t.Fatalf("KMS got %d requests, want 1", len(requests))
		}
		if requests[0].Name != testKeyName {
			t.Errorf("key = %q, want %q", requests[0].Name, testKeyName)
		}
		if !bytes.Equal(requests[0].Plaintext, dek) {
			t.Errorf("plaintext = %q, want the DEK %q", requests[0].Plaintext, dek)
		}
	})

	t.Run("returns the ciphertext as the wrapped DEK, and the key name as its key id", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{ciphertext: []byte("wrapped")})

		wrapped, keyID, err := client.Encrypt(context.Background(), dek)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		if string(wrapped) != "wrapped" {
			t.Errorf("wrapped = %q, want %q", wrapped, "wrapped")
		}
		if keyID != testKeyName {
			t.Errorf("key id = %q, want %q", keyID, testKeyName)
		}
	})

	t.Run("returns Cloud KMS's error, and nothing else, when it refuses the request", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{err: status.Error(codes.PermissionDenied, "no cryptoKeyEncrypter role")})

		wrapped, keyID, err := client.Encrypt(context.Background(), dek)

		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("error = %v, want one with code PermissionDenied", err)
		}
		if wrapped != nil || keyID != "" {
			t.Errorf("returned %q and %q alongside the error", wrapped, keyID)
		}
	})

	t.Run("stops without contacting Cloud KMS when the caller's context is already cancelled", func(t *testing.T) {
		fake := &fakeKMS{ciphertext: []byte("wrapped")}
		client := serveFakeKMS(t, fake)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := client.Encrypt(ctx, dek)

		if status.Code(err) != codes.Canceled {
			t.Errorf("error = %v, want one with code Canceled", err)
		}
		if n := len(fake.received()); n != 0 {
			t.Errorf("KMS got %d requests, want none", n)
		}
	})
}

func TestGCPKMSClientDecrypt(t *testing.T) {
	wrapped := []byte("wrapped")
	dek := []byte("a plaintext data encryption key.")
	// A key id other than the client's own, as for a DEK wrapped under a key
	// the client was configured with earlier.
	const keyID = "projects/p/locations/l/keyRings/r/cryptoKeys/older"

	t.Run("asks Cloud KMS to decrypt the wrapped DEK with the key id it was stored with", func(t *testing.T) {
		fake := &fakeKMS{plaintext: dek}
		client := serveFakeKMS(t, fake)

		if _, err := client.Decrypt(context.Background(), wrapped, keyID); err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		requests := fake.receivedDecrypts()
		if len(requests) != 1 {
			t.Fatalf("KMS got %d requests, want 1", len(requests))
		}
		if requests[0].Name != keyID {
			t.Errorf("key = %q, want the stored key id %q", requests[0].Name, keyID)
		}
		if !bytes.Equal(requests[0].Ciphertext, wrapped) {
			t.Errorf("ciphertext = %q, want %q", requests[0].Ciphertext, wrapped)
		}
	})

	t.Run("returns the plaintext DEK", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{plaintext: dek})

		got, err := client.Decrypt(context.Background(), wrapped, keyID)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		if !bytes.Equal(got, dek) {
			t.Errorf("DEK = %q, want %q", got, dek)
		}
	})

	t.Run("returns Cloud KMS's error, and no DEK, when it refuses the request", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{plaintext: dek, err: status.Error(codes.PermissionDenied, "no cryptoKeyDecrypter role")})

		got, err := client.Decrypt(context.Background(), wrapped, keyID)

		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("error = %v, want one with code PermissionDenied", err)
		}
		if got != nil {
			t.Errorf("returned %q alongside the error", got)
		}
	})

	t.Run("stops without contacting Cloud KMS when the caller's context is already cancelled", func(t *testing.T) {
		fake := &fakeKMS{plaintext: dek}
		client := serveFakeKMS(t, fake)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := client.Decrypt(ctx, wrapped, keyID)

		if status.Code(err) != codes.Canceled {
			t.Errorf("error = %v, want one with code Canceled", err)
		}
		if n := len(fake.receivedDecrypts()); n != 0 {
			t.Errorf("KMS got %d requests, want none", n)
		}
	})
}

func TestGCPKMSClientRewrap(t *testing.T) {
	wrapped := []byte("old wrapped")
	dek := []byte("a plaintext data encryption key.")
	// A key id other than the client's own, as for a DEK wrapped under a key
	// the client was configured with earlier.
	const keyID = "projects/p/locations/l/keyRings/r/cryptoKeys/older"

	t.Run("decrypts the wrapped DEK and encrypts it again, both with the key id it was stored with", func(t *testing.T) {
		fake := &fakeKMS{plaintext: dek, ciphertext: []byte("new wrapped")}
		client := serveFakeKMS(t, fake)

		if _, err := client.Rewrap(context.Background(), wrapped, keyID); err != nil {
			t.Fatalf("Rewrap: %v", err)
		}

		decrypts, encrypts := fake.receivedDecrypts(), fake.received()
		if len(decrypts) != 1 || len(encrypts) != 1 {
			t.Fatalf("KMS got %d decrypts and %d encrypts, want 1 of each", len(decrypts), len(encrypts))
		}
		if decrypts[0].Name != keyID || !bytes.Equal(decrypts[0].Ciphertext, wrapped) {
			t.Errorf("decrypt = (%q, %q), want (%q, %q)", decrypts[0].Name, decrypts[0].Ciphertext, keyID, wrapped)
		}
		// Encrypting under the configured key instead would move the DEK to
		// a key whose id isn't the one stored with it.
		if encrypts[0].Name != keyID {
			t.Errorf("encrypt key = %q, want the stored key id %q", encrypts[0].Name, keyID)
		}
		if !bytes.Equal(encrypts[0].Plaintext, dek) {
			t.Errorf("encrypted plaintext = %q, want the DEK %q", encrypts[0].Plaintext, dek)
		}
	})

	t.Run("returns the new ciphertext as the wrapped DEK", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{plaintext: dek, ciphertext: []byte("new wrapped")})

		got, err := client.Rewrap(context.Background(), wrapped, keyID)
		if err != nil {
			t.Fatalf("Rewrap: %v", err)
		}

		if string(got) != "new wrapped" {
			t.Errorf("got %q, want %q", got, "new wrapped")
		}
	})

	t.Run("returns Cloud KMS's error, and encrypts nothing, when the DEK can't be decrypted", func(t *testing.T) {
		fake := &fakeKMS{err: status.Error(codes.PermissionDenied, "no cryptoKeyDecrypter role"), ciphertext: []byte("new wrapped")}
		client := serveFakeKMS(t, fake)

		got, err := client.Rewrap(context.Background(), wrapped, keyID)

		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("error = %v, want one with code PermissionDenied", err)
		}
		if got != nil {
			t.Errorf("returned %q alongside the error", got)
		}
		if n := len(fake.received()); n != 0 {
			t.Errorf("KMS got %d encrypts, want none", n)
		}
	})

	t.Run("returns Cloud KMS's error, and no ciphertext, when the DEK can't be encrypted again", func(t *testing.T) {
		client := serveFakeKMS(t, &fakeKMS{plaintext: dek, encryptErr: status.Error(codes.PermissionDenied, "no cryptoKeyEncrypter role")})

		got, err := client.Rewrap(context.Background(), wrapped, keyID)

		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("error = %v, want one with code PermissionDenied", err)
		}
		if got != nil {
			t.Errorf("returned %q alongside the error", got)
		}
	})

	t.Run("stops without contacting Cloud KMS when the caller's context is already cancelled", func(t *testing.T) {
		fake := &fakeKMS{plaintext: dek, ciphertext: []byte("new wrapped")}
		client := serveFakeKMS(t, fake)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := client.Rewrap(ctx, wrapped, keyID)

		if status.Code(err) != codes.Canceled {
			t.Errorf("error = %v, want one with code Canceled", err)
		}
		if n := len(fake.receivedDecrypts()) + len(fake.received()); n != 0 {
			t.Errorf("KMS got %d requests, want none", n)
		}
	})
}

func TestNewGCPKMSClient(t *testing.T) {
	const keyName = "projects/p/locations/l/keyRings/r/cryptoKeys/k"

	t.Run("returns an error, and no client, when no credentials can be loaded", func(t *testing.T) {
		// The client looks for credentials as it's created. Pointing
		// GOOGLE_APPLICATION_CREDENTIALS at a file that isn't there makes
		// that fail the same way on every machine, whatever gcloud login or
		// metadata server it happens to have.
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))

		client, err := NewGCPKMSClient(context.Background(), keyName)

		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if client != nil {
			t.Fatal("returned a client alongside the error")
		}
		if !strings.Contains(err.Error(), "creating KMS client") {
			t.Errorf("error %q doesn't say what was being created", err)
		}
	})

	t.Run("creates a client for the given key from a service account key file", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccountKey(t))

		client, err := NewGCPKMSClient(context.Background(), keyName)
		if err != nil {
			t.Fatalf("NewGCPKMSClient: %v", err)
		}

		if client.keyName != keyName {
			t.Errorf("key name = %q, want %q", client.keyName, keyName)
		}
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}
