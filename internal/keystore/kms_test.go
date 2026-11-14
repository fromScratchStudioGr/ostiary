package keystore

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/config"
)

// useLocalKMS gives the local KMS the master key it reads from the environment.
func useLocalKMS(t *testing.T) {
	t.Helper()
	t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		provider config.KMS
		prepare  func(t *testing.T)
		want     string // the client's type
	}{
		{"local, when asked for", config.KMS{Provider: "local"}, useLocalKMS, "*keystore.LocalKMS"},
		{"local, when no provider is set", config.KMS{}, useLocalKMS, "*keystore.LocalKMS"},
		{"local, for a provider it doesn't know", config.KMS{Provider: "something-else"}, useLocalKMS, "*keystore.LocalKMS"},
		{
			"gcp", config.KMS{Provider: "gcp", GCPKeyName: testKeyName},
			func(t *testing.T) { t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccountKey(t)) },
			"*keystore.GCPKMSClient",
		},
		{
			"openbao",
			config.KMS{Provider: "openbao", OpenbaoVaultAddr: "http://127.0.0.1:8200", OpenbaoVaultToken: "token", OpenbaoVaultKeyname: "key"},
			func(*testing.T) {},
			"*keystore.VaultKMSClient",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.prepare(t)

			kms, closeKMS, err := New(tt.provider)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer closeKMS()

			if got := typeName(kms); got != tt.want {
				t.Errorf("client is a %s, want %s", got, tt.want)
			}
		})
	}

	t.Run("fails, naming the provider, when its settings are missing", func(t *testing.T) {
		tests := []struct {
			name     string
			provider config.KMS
			prepare  func(t *testing.T)
			want     string
		}{
			{
				"gcp without credentials", config.KMS{Provider: "gcp"},
				func(t *testing.T) {
					t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
				},
				"gcp",
			},
			{"openbao without its address, token and key", config.KMS{Provider: "openbao"}, func(*testing.T) {}, "openbao"},
			{"local without a master key", config.KMS{Provider: "local"}, func(t *testing.T) { t.Setenv("LOCAL_KMS_MASTER_KEY", "") }, "local"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.prepare(t)

				kms, closeKMS, err := New(tt.provider)

				if err == nil || !strings.HasPrefix(err.Error(), tt.want+": ") {
					t.Errorf("error = %v, want one starting with %q", err, tt.want+": ")
				}
				if kms != nil || closeKMS != nil {
					t.Error("returned a client or closer alongside the error")
				}
			})
		}
	})
}

func typeName(kms KMSClient) string {
	switch kms.(type) {
	case *LocalKMS:
		return "*keystore.LocalKMS"
	case *GCPKMSClient:
		return "*keystore.GCPKMSClient"
	case *VaultKMSClient:
		return "*keystore.VaultKMSClient"
	}
	return "an unknown type"
}
