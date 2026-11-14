package logging

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// isolateLogging points os.Stderr at a file and restores both it and slog's
// default logger when the test ends. It returns a func that reads what was
// written to stderr so far. Setup picks up os.Stderr when it's called, so this
// has to happen before it.
func isolateLogging(t *testing.T) (stderr func() string) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original, previous := os.Stderr, slog.Default()
	os.Stderr = file
	t.Cleanup(func() {
		os.Stderr = original
		slog.SetDefault(previous)
		file.Close()
	})

	return func() string {
		content, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
}

// fakeSeq is a Seq server that keeps what it's sent.
type fakeSeq struct {
	*httptest.Server

	mu      sync.Mutex
	apiKeys []string
	bodies  []string
}

func newFakeSeq(t *testing.T) *fakeSeq {
	t.Helper()
	f := &fakeSeq{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.apiKeys = append(f.apiKeys, r.Header.Get("X-Seq-ApiKey"))
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSeq) received() (apiKeys, bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.apiKeys...), append([]string(nil), f.bodies...)
}

func TestSetup(t *testing.T) {
	t.Run("logs only to stderr when no Seq URL is set", func(t *testing.T) {
		stderr := isolateLogging(t)

		closeLogging := Setup("", "")
		slog.Info("hello", "key", "value")
		closeLogging()

		if out := stderr(); !strings.Contains(out, "msg=hello") || !strings.Contains(out, "key=value") {
			t.Errorf("stderr = %q, want the record with its attribute", out)
		}
	})

	t.Run("logs to both stderr and Seq when a Seq URL is set", func(t *testing.T) {
		stderr := isolateLogging(t)
		seq := newFakeSeq(t)

		closeLogging := Setup(seq.URL, "secret")
		slog.Info("shipped", "key", "value")
		// Seq events are sent in batches, which closing flushes.
		closeLogging()

		if out := stderr(); !strings.Contains(out, "msg=shipped") {
			t.Errorf("stderr = %q, want the record", out)
		}
		apiKeys, bodies := seq.received()
		if len(bodies) == 0 {
			t.Fatal("Seq received nothing")
		}
		if !strings.Contains(strings.Join(bodies, "\n"), "shipped") {
			t.Errorf("Seq bodies = %q, want the record", bodies)
		}
		for _, key := range apiKeys {
			if key != "secret" {
				t.Errorf("Seq API key header = %q, want secret", key)
			}
		}
	})

	t.Run("still ships to Seq when writing to stderr fails", func(t *testing.T) {
		isolateLogging(t)
		seq := newFakeSeq(t)
		// A closed file makes every write to stderr fail.
		broken, err := os.CreateTemp(t.TempDir(), "stderr")
		if err != nil {
			t.Fatal(err)
		}
		broken.Close()
		os.Stderr = broken

		closeLogging := Setup(seq.URL, "")
		slog.Info("shipped anyway")
		closeLogging()

		_, bodies := seq.received()
		if !strings.Contains(strings.Join(bodies, "\n"), "shipped anyway") {
			t.Errorf("Seq bodies = %q, want the record", bodies)
		}
	})

	t.Run("reports a failure to reach Seq on stderr, without losing the local log", func(t *testing.T) {
		stderr := isolateLogging(t)

		closeLogging := Setup("http://127.0.0.1:1/ingest/clef", "")
		slog.Info("local")
		closeLogging()

		out := stderr()
		if !strings.Contains(out, "msg=local") {
			t.Errorf("stderr = %q, want the record", out)
		}
		if !strings.Contains(out, "seq logging:") {
			t.Errorf("stderr = %q, want a \"seq logging:\" error", out)
		}
	})
}
