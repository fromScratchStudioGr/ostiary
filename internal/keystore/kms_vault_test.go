package keystore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// isolateEnv clears the BAO_* variables the OpenBao client reads from the
// environment, so a developer's own setup can't change what a test sees.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BAO_ADDR", "BAO_AGENT_ADDR", "BAO_TOKEN", "BAO_NAMESPACE"} {
		t.Setenv(name, "")
	}
}

func TestNewVaultKMSClient(t *testing.T) {
	t.Run("targets the given address, token and transit key", func(t *testing.T) {
		isolateEnv(t)

		client, err := NewVaultKMSClient("http://openbao.example:8200", "the-token", "ostiary-dek-wrap")
		if err != nil {
			t.Fatalf("NewVaultKMSClient: %v", err)
		}

		if got := client.client.Address(); got != "http://openbao.example:8200" {
			t.Errorf("address = %q, want %q", got, "http://openbao.example:8200")
		}
		if got := client.client.Token(); got != "the-token" {
			t.Errorf("token = %q, want %q", got, "the-token")
		}
		if client.keyName != "ostiary-dek-wrap" {
			t.Errorf("transit key = %q, want %q", client.keyName, "ostiary-dek-wrap")
		}
	})

	t.Run("uses the token it was given even when BAO_TOKEN is set in the environment", func(t *testing.T) {
		// The OpenBao client picks BAO_TOKEN up on its own when it's created.
		// The configured token has to win, or the gateway would authenticate
		// as whoever the host environment says.
		isolateEnv(t)
		t.Setenv("BAO_TOKEN", "token-from-the-environment")

		client, err := NewVaultKMSClient("http://openbao.example:8200", "the-configured-token", "ostiary-dek-wrap")
		if err != nil {
			t.Fatalf("NewVaultKMSClient: %v", err)
		}

		if got := client.client.Token(); got != "the-configured-token" {
			t.Fatalf("token = %q, want the configured one", got)
		}
	})

	t.Run("doesn't contact the server while constructing", func(t *testing.T) {
		// A missing key or a bad token only shows up on first use, as the
		// function's comment says, so startup doesn't depend on OpenBao
		// being reachable at that moment.
		isolateEnv(t)
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		t.Cleanup(server.Close)

		if _, err := NewVaultKMSClient(server.URL, "the-token", "ostiary-dek-wrap"); err != nil {
			t.Fatalf("NewVaultKMSClient: %v", err)
		}

		if n := requests.Load(); n != 0 {
			t.Fatalf("%d requests were sent while constructing, want none", n)
		}
	})

	t.Run("rejects an address that can't be parsed and returns no client", func(t *testing.T) {
		isolateEnv(t)

		client, err := NewVaultKMSClient("http://[::1:8200", "the-token", "ostiary-dek-wrap")

		if err == nil {
			t.Fatal("got nil error, want one")
		}
		if client != nil {
			t.Fatal("returned a client alongside the error")
		}
		if !strings.Contains(err.Error(), "creating OpenBao client") {
			t.Errorf("error %q doesn't say what was being created", err)
		}
	})

	t.Run("rejects missing settings, naming exactly the ones that are missing", func(t *testing.T) {
		// An empty setting would otherwise be accepted here and only fail on
		// the first encrypt or decrypt, after the service had started.
		isolateEnv(t)
		const addr, token, keyName = "http://openbao.example:8200", "the-token", "ostiary-dek-wrap"
		allSettings := []string{"OPENBAO_VAULT_ADDR", "OPENBAO_VAULT_TOKEN", "OPENBAO_VAULT_KEYNAME"}

		tests := []struct {
			name                 string
			addr, token, keyName string
			missing              []string
		}{
			{name: "no address", addr: "", token: token, keyName: keyName, missing: []string{"OPENBAO_VAULT_ADDR"}},
			{name: "no token", addr: addr, token: "", keyName: keyName, missing: []string{"OPENBAO_VAULT_TOKEN"}},
			{name: "no key name", addr: addr, token: token, keyName: "", missing: []string{"OPENBAO_VAULT_KEYNAME"}},
			{name: "nothing set", addr: "", token: "", keyName: "", missing: allSettings},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				client, err := NewVaultKMSClient(tt.addr, tt.token, tt.keyName)

				if err == nil {
					t.Fatal("got nil error, want one")
				}
				if client != nil {
					t.Fatal("returned a client alongside the error")
				}
				for _, setting := range allSettings {
					want := slices.Contains(tt.missing, setting)
					if got := strings.Contains(err.Error(), setting); got != want {
						t.Errorf("error %q mentions %s: %v, want %v", err, setting, got, want)
					}
				}
			})
		}
	})

	t.Run("uses the given address even when BAO_AGENT_ADDR is set in the environment", func(t *testing.T) {
		// The OpenBao client prefers an agent address from the environment
		// to the address it's configured with, which would silently send
		// every request somewhere else than OPENBAO_VAULT_ADDR.
		isolateEnv(t)
		t.Setenv("BAO_AGENT_ADDR", "http://agent.example:8100")

		client, err := NewVaultKMSClient("http://openbao.example:8200", "the-token", "ostiary-dek-wrap")
		if err != nil {
			t.Fatalf("NewVaultKMSClient: %v", err)
		}

		if got := client.client.Address(); got != "http://openbao.example:8200" {
			t.Fatalf("address = %q, want the configured one", got)
		}
	})
}

// fakeOpenBao is a stand-in OpenBao server. It answers every request with one
// canned response and records what it was sent.
type fakeOpenBao struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	path  string
	token string
	body  map[string]any
}

func newFakeOpenBao(t *testing.T, status int, response string) *fakeOpenBao {
	t.Helper()
	f := &fakeOpenBao{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{path: r.URL.Path, token: r.Header.Get("X-Vault-Token"), body: body})
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response) // a 204 has no body to write
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOpenBao) received() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// client returns a VaultKMSClient pointed at the fake, with the token and
// transit key the tests expect to see on the wire.
func (f *fakeOpenBao) client(t *testing.T) *VaultKMSClient {
	t.Helper()
	isolateEnv(t)
	client, err := NewVaultKMSClient(f.server.URL, "the-token", "ostiary-dek-wrap")
	if err != nil {
		t.Fatalf("NewVaultKMSClient: %v", err)
	}
	return client
}

func TestVaultKMSClientEncrypt(t *testing.T) {
	ctx := context.Background()
	const okResponse = `{"data":{"ciphertext":"vault:v1:wrapped-dek"}}`

	t.Run("asks the transit engine to encrypt the base64-encoded DEK with the configured key and token", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)
		dek := GenerateDEK()

		if _, _, err := fake.client(t).Encrypt(ctx, dek); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		requests := fake.received()
		if len(requests) != 1 {
			t.Fatalf("%d requests sent, want 1", len(requests))
		}
		req := requests[0]
		if req.path != "/v1/transit/encrypt/ostiary-dek-wrap" {
			t.Errorf("path = %q, want %q", req.path, "/v1/transit/encrypt/ostiary-dek-wrap")
		}
		if req.token != "the-token" {
			t.Errorf("token header = %q, want %q", req.token, "the-token")
		}
		// Transit takes the plaintext as standard base64, and nothing else
		// that would change how it's encrypted.
		if want := base64.StdEncoding.EncodeToString(dek); req.body["plaintext"] != want || len(req.body) != 1 {
			t.Errorf("request body = %v, want only plaintext = %q", req.body, want)
		}
	})

	t.Run("returns the ciphertext as the wrapped DEK, and the transit key name as its key id", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)

		wrapped, keyID, err := fake.client(t).Encrypt(ctx, GenerateDEK())
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}

		// The key id is what Decrypt later puts in its URL, so it has to be
		// the transit key's name.
		if string(wrapped) != "vault:v1:wrapped-dek" || keyID != "ostiary-dek-wrap" {
			t.Fatalf("got (%q, %q), want (%q, %q)", wrapped, keyID, "vault:v1:wrapped-dek", "ostiary-dek-wrap")
		}
	})

	t.Run("returns OpenBao's own message, and nothing else, when it refuses the request", func(t *testing.T) {
		tests := []struct {
			name     string
			status   int
			response string
			want     string
		}{
			{name: "token not allowed", status: http.StatusForbidden, response: `{"errors":["permission denied"]}`, want: "permission denied"},
			{name: "key doesn't exist", status: http.StatusBadRequest, response: `{"errors":["encryption key not found"]}`, want: "encryption key not found"},
			{name: "transit engine not enabled", status: http.StatusNotFound, response: `{"errors":["no handler for route 'transit/encrypt/ostiary-dek-wrap'. route entry not found."]}`, want: "route entry not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wrapped, keyID, err := newFakeOpenBao(t, tt.status, tt.response).client(t).Encrypt(ctx, GenerateDEK())

				if err == nil {
					t.Fatal("got nil error, want one")
				}
				if wrapped != nil || keyID != "" {
					t.Error("returned data alongside the error")
				}
				// What the operator sees in the log has to say what OpenBao
				// objected to, and which call it was.
				if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "transit encrypt") {
					t.Errorf("error %q should mention %q and %q", err, tt.want, "transit encrypt")
				}
			})
		}
	})

	t.Run("returns an error and nothing else when the response has no usable ciphertext", func(t *testing.T) {
		for name, response := range map[string]string{
			"no ciphertext field":     `{"data":{}}`,
			"ciphertext not a string": `{"data":{"ciphertext":123}}`,
		} {
			t.Run(name, func(t *testing.T) {
				wrapped, keyID, err := newFakeOpenBao(t, http.StatusOK, response).client(t).Encrypt(ctx, GenerateDEK())

				if err == nil || !strings.Contains(err.Error(), "missing ciphertext") {
					t.Fatalf("error = %v, want one saying the ciphertext is missing", err)
				}
				if wrapped != nil || keyID != "" {
					t.Error("returned data alongside the error")
				}
			})
		}
	})

	t.Run("returns an error instead of panicking when OpenBao answers with nothing to read", func(t *testing.T) {
		// The OpenBao client reports a response with no body as a nil secret
		// and no error. A panic here would take the request down with it.
		tests := []struct {
			name     string
			status   int
			response string
		}{
			{name: "an empty 200", status: http.StatusOK, response: ""},
			{name: "a 204", status: http.StatusNoContent, response: ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				client := newFakeOpenBao(t, tt.status, tt.response).client(t)

				var (
					wrapped []byte
					keyID   string
					err     error
				)
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("Encrypt panicked: %v", r)
						}
					}()
					wrapped, keyID, err = client.Encrypt(ctx, GenerateDEK())
				}()

				if err == nil {
					t.Error("got nil error, want one")
				}
				if wrapped != nil || keyID != "" {
					t.Error("returned data alongside the error")
				}
			})
		}
	})

	t.Run("stops without contacting OpenBao when the caller's context is already cancelled", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		_, _, err := fake.client(t).Encrypt(cancelled, GenerateDEK())

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want one wrapping context.Canceled", err)
		}
		if n := len(fake.received()); n != 0 {
			t.Fatalf("%d requests were sent, want none", n)
		}
	})
}

func TestVaultKMSClientDecrypt(t *testing.T) {
	ctx := context.Background()
	wrapped := []byte("vault:v1:wrapped-dek")
	dek := GenerateDEK()
	okResponse := fmt.Sprintf(`{"data":{"plaintext":%q}}`, base64.StdEncoding.EncodeToString(dek))

	t.Run("asks the transit engine to decrypt the wrapped DEK with the key id it was given", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)

		// A key id other than the configured key name, as after a change of
		// key: the one stored with the wrapped DEK decides which key unwraps it.
		if _, err := fake.client(t).Decrypt(ctx, wrapped, "an-older-key"); err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		requests := fake.received()
		if len(requests) != 1 {
			t.Fatalf("%d requests sent, want 1", len(requests))
		}
		req := requests[0]
		if req.path != "/v1/transit/decrypt/an-older-key" {
			t.Errorf("path = %q, want %q", req.path, "/v1/transit/decrypt/an-older-key")
		}
		if req.token != "the-token" {
			t.Errorf("token header = %q, want %q", req.token, "the-token")
		}
		if req.body["ciphertext"] != string(wrapped) || len(req.body) != 1 {
			t.Errorf("request body = %v, want only ciphertext = %q", req.body, wrapped)
		}
	})

	t.Run("returns the DEK decoded from the base64 plaintext", func(t *testing.T) {
		got, err := newFakeOpenBao(t, http.StatusOK, okResponse).client(t).Decrypt(ctx, wrapped, "ostiary-dek-wrap")
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		if string(got) != string(dek) {
			t.Fatalf("got %x, want %x", got, dek)
		}
	})

	t.Run("returns OpenBao's own message, and nothing else, when it refuses the request", func(t *testing.T) {
		tests := []struct {
			name     string
			status   int
			response string
			want     string
		}{
			{name: "token not allowed", status: http.StatusForbidden, response: `{"errors":["permission denied"]}`, want: "permission denied"},
			{name: "ciphertext not accepted", status: http.StatusBadRequest, response: `{"errors":["invalid ciphertext: no prefix"]}`, want: "invalid ciphertext"},
			{name: "transit engine not enabled", status: http.StatusNotFound, response: `{"errors":["no handler for route 'transit/decrypt/ostiary-dek-wrap'. route entry not found."]}`, want: "route entry not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := newFakeOpenBao(t, tt.status, tt.response).client(t).Decrypt(ctx, wrapped, "ostiary-dek-wrap")

				if err == nil {
					t.Fatal("got nil error, want one")
				}
				if got != nil {
					t.Error("returned data alongside the error")
				}
				// What the operator sees in the log has to say what OpenBao
				// objected to, and which call it was.
				if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "transit decrypt") {
					t.Errorf("error %q should mention %q and %q", err, tt.want, "transit decrypt")
				}
			})
		}
	})

	t.Run("returns an error and nothing else when the response has no usable plaintext", func(t *testing.T) {
		tests := []struct {
			name     string
			response string
			want     string
		}{
			{name: "no plaintext field", response: `{"data":{}}`, want: "missing plaintext"},
			{name: "plaintext not a string", response: `{"data":{"plaintext":123}}`, want: "missing plaintext"},
			{name: "plaintext not valid base64", response: `{"data":{"plaintext":"not base64!"}}`, want: "decoding plaintext"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := newFakeOpenBao(t, http.StatusOK, tt.response).client(t).Decrypt(ctx, wrapped, "ostiary-dek-wrap")

				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want one mentioning %q", err, tt.want)
				}
				if got != nil {
					t.Error("returned data alongside the error")
				}
			})
		}
	})

	t.Run("returns an error instead of panicking when OpenBao answers with nothing to read", func(t *testing.T) {
		// The OpenBao client reports a response with no body as a nil secret
		// and no error. A panic here would take the request down with it.
		tests := []struct {
			name     string
			status   int
			response string
		}{
			{name: "an empty 200", status: http.StatusOK, response: ""},
			{name: "a 204", status: http.StatusNoContent, response: ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				client := newFakeOpenBao(t, tt.status, tt.response).client(t)

				var (
					got []byte
					err error
				)
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("Decrypt panicked: %v", r)
						}
					}()
					got, err = client.Decrypt(ctx, wrapped, "ostiary-dek-wrap")
				}()

				if err == nil {
					t.Error("got nil error, want one")
				}
				if got != nil {
					t.Error("returned data alongside the error")
				}
			})
		}
	})

	t.Run("stops without contacting OpenBao when the caller's context is already cancelled", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := fake.client(t).Decrypt(cancelled, wrapped, "ostiary-dek-wrap")

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want one wrapping context.Canceled", err)
		}
		if n := len(fake.received()); n != 0 {
			t.Fatalf("%d requests were sent, want none", n)
		}
	})
}

func TestVaultKMSClientRewrap(t *testing.T) {
	ctx := context.Background()
	wrapped := []byte("vault:v1:wrapped-dek")
	const okResponse = `{"data":{"ciphertext":"vault:v2:rewrapped-dek"}}`

	t.Run("asks the transit engine to rewrap the wrapped DEK with the key id it was given", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)

		// A key id other than the configured key name: the key that wrapped
		// the DEK is the one that has to rewrap it.
		if _, err := fake.client(t).Rewrap(ctx, wrapped, "an-older-key"); err != nil {
			t.Fatalf("Rewrap: %v", err)
		}

		requests := fake.received()
		if len(requests) != 1 {
			t.Fatalf("%d requests sent, want 1", len(requests))
		}
		req := requests[0]
		if req.path != "/v1/transit/rewrap/an-older-key" {
			t.Errorf("path = %q, want %q", req.path, "/v1/transit/rewrap/an-older-key")
		}
		if req.token != "the-token" {
			t.Errorf("token header = %q, want %q", req.token, "the-token")
		}
		if req.body["ciphertext"] != string(wrapped) || len(req.body) != 1 {
			t.Errorf("request body = %v, want only ciphertext = %q", req.body, wrapped)
		}
	})

	t.Run("returns the new ciphertext as the wrapped DEK", func(t *testing.T) {
		got, err := newFakeOpenBao(t, http.StatusOK, okResponse).client(t).Rewrap(ctx, wrapped, "ostiary-dek-wrap")
		if err != nil {
			t.Fatalf("Rewrap: %v", err)
		}

		if string(got) != "vault:v2:rewrapped-dek" {
			t.Fatalf("got %q, want %q", got, "vault:v2:rewrapped-dek")
		}
	})

	t.Run("returns OpenBao's own message, and nothing else, when it refuses the request", func(t *testing.T) {
		tests := []struct {
			name     string
			status   int
			response string
			want     string
		}{
			{name: "token not allowed", status: http.StatusForbidden, response: `{"errors":["permission denied"]}`, want: "permission denied"},
			{name: "ciphertext not accepted", status: http.StatusBadRequest, response: `{"errors":["invalid ciphertext: no prefix"]}`, want: "invalid ciphertext"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := newFakeOpenBao(t, tt.status, tt.response).client(t).Rewrap(ctx, wrapped, "ostiary-dek-wrap")

				if err == nil {
					t.Fatal("got nil error, want one")
				}
				if got != nil {
					t.Error("returned data alongside the error")
				}
				if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "transit rewrap") {
					t.Errorf("error %q should mention %q and %q", err, tt.want, "transit rewrap")
				}
			})
		}
	})

	t.Run("returns an error and nothing else when the response has no usable ciphertext", func(t *testing.T) {
		for name, response := range map[string]string{
			"no ciphertext field":     `{"data":{}}`,
			"ciphertext not a string": `{"data":{"ciphertext":123}}`,
		} {
			t.Run(name, func(t *testing.T) {
				got, err := newFakeOpenBao(t, http.StatusOK, response).client(t).Rewrap(ctx, wrapped, "ostiary-dek-wrap")

				if err == nil || !strings.Contains(err.Error(), "missing ciphertext") {
					t.Fatalf("error = %v, want one saying the ciphertext is missing", err)
				}
				if got != nil {
					t.Error("returned data alongside the error")
				}
			})
		}
	})

	t.Run("returns an error instead of panicking when OpenBao answers with nothing to read", func(t *testing.T) {
		client := newFakeOpenBao(t, http.StatusNoContent, "").client(t)

		var (
			got []byte
			err error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Rewrap panicked: %v", r)
				}
			}()
			got, err = client.Rewrap(ctx, wrapped, "ostiary-dek-wrap")
		}()

		if err == nil {
			t.Error("got nil error, want one")
		}
		if got != nil {
			t.Error("returned data alongside the error")
		}
	})

	t.Run("stops without contacting OpenBao when the caller's context is already cancelled", func(t *testing.T) {
		fake := newFakeOpenBao(t, http.StatusOK, okResponse)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := fake.client(t).Rewrap(cancelled, wrapped, "ostiary-dek-wrap")

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want one wrapping context.Canceled", err)
		}
		if n := len(fake.received()); n != 0 {
			t.Fatalf("%d requests were sent, want none", n)
		}
	})
}
