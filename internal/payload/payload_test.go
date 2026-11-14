package payload

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
)

// served is what a request through Read did.
type served struct {
	rec     *httptest.ResponseRecorder
	calls   int
	payload Payload
	found   bool
	body    string
}

func serve(body io.Reader) served {
	var s served
	handler := Read(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.payload, s.found = FromContext(r)
		b, _ := io.ReadAll(r.Body)
		s.body = string(b)
		w.WriteHeader(http.StatusAccepted)
	})
	s.rec = httptest.NewRecorder()
	handler(s.rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))
	return s
}

func (s served) value(m Member) string {
	return string(s.payload.Body[m.Start:m.End])
}

// errReader fails every read, like a client that drops mid-upload.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestRead(t *testing.T) {
	t.Run("passes the request on with its model and body in the context, and the body ready to read again", func(t *testing.T) {
		body := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`

		s := serve(strings.NewReader(body))

		if s.calls != 1 || s.rec.Code != http.StatusAccepted {
			t.Fatalf("calls = %d, status = %d; want one call: %s", s.calls, s.rec.Code, s.rec.Body.String())
		}
		if !s.found || s.payload.Model != "gpt-6-luna" || string(s.payload.Body) != body {
			t.Errorf("payload = %+v (found %v), want model gpt-6-luna and the body", s.payload, s.found)
		}
		if s.body != body {
			t.Errorf("the next handler read %q, want %q", s.body, body)
		}
	})

	t.Run("reads the model only from the exact key, as a provider does", func(t *testing.T) {
		s := serve(strings.NewReader(`{"Model":"gpt-6-luna","model":"gpt-6-astra","MODEL":"gpt-6-luna"}`))

		if s.payload.Model != "gpt-6-astra" {
			t.Errorf("model = %q, want gpt-6-astra", s.payload.Model)
		}
	})

	t.Run("decodes an escaped model, and finds one under an escaped key", func(t *testing.T) {
		s := serve(strings.NewReader(`{"model":"gpt-6-luna"}`))

		if s.payload.Model != "gpt-6-luna" {
			t.Errorf("model = %q, want gpt-6-luna", s.payload.Model)
		}
	})

	t.Run("says where stream and stream_options are, and how often they appear", func(t *testing.T) {
		s := serve(strings.NewReader(`{"model":"m","stream":false,"stream_options":{"a":1},"stream":true}`))

		if got := s.payload.Stream; got.Seen != 2 || s.value(got) != "true" {
			t.Errorf("stream = %+v (%q), want seen twice, pointing at the last copy, true", got, s.value(got))
		}
		if got := s.payload.StreamOptions; got.Seen != 1 || s.value(got) != `{"a":1}` {
			t.Errorf("stream_options = %+v (%q), want seen once, {\"a\":1}", got, s.value(got))
		}
	})

	t.Run("says neither was seen when they're absent", func(t *testing.T) {
		s := serve(strings.NewReader(`{"model":"m"}`))

		if s.payload.Stream.Seen != 0 || s.payload.StreamOptions.Seen != 0 {
			t.Errorf("stream = %+v, stream_options = %+v; want neither seen", s.payload.Stream, s.payload.StreamOptions)
		}
	})

	t.Run("turns the request down, without passing it on, when", func(t *testing.T) {
		tests := []struct {
			name    string
			body    io.Reader
			message string
		}{
			{"the body can't be read", errReader{}, "Could not read request body"},
			{"the body isn't JSON", strings.NewReader("not json"), "Invalid payload"},
			{"the body isn't an object", strings.NewReader(`[{"model":"m"}]`), "Invalid payload"},
			{"there's data after the object", strings.NewReader(`{"model":"m"}{"model":"n"}`), "Invalid payload"},
			{"there's no model", strings.NewReader(`{"messages":[]}`), "Invalid payload: the model is missing"},
			{"the model is only under another case", strings.NewReader(`{"Model":"m"}`), "Invalid payload: the model is missing"},
			{"the model is empty", strings.NewReader(`{"model":""}`), "Invalid payload: the model is missing"},
			{"the model is null", strings.NewReader(`{"model":null}`), "Invalid payload: the model is missing"},
			{"the model is given twice", strings.NewReader(`{"model":"gpt-6-astra","model":"gpt-6-luna"}`), "Invalid payload: the model must be given only once"},
			{"the model is given twice, once escaped", strings.NewReader(`{"model":"gpt-6-astra","model":"gpt-6-luna"}`), "Invalid payload: the model must be given only once"},
			{"the model is a number", strings.NewReader(`{"model":4}`), "Invalid payload: the model must be a string"},
			{"the model is an object", strings.NewReader(`{"model":{"id":"m"}}`), "Invalid payload: the model must be a string"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s := serve(tt.body)

				if s.calls != 0 {
					t.Error("the request was passed on")
				}
				var got api.ErrorResponse
				json.Unmarshal(s.rec.Body.Bytes(), &got)
				if s.rec.Code != http.StatusBadRequest || got.Error != tt.message {
					t.Errorf("response = %d %q, want 400 %q", s.rec.Code, s.rec.Body.String(), tt.message)
				}
			})
		}
	})
}

func TestFromContext(t *testing.T) {
	t.Run("finds nothing in a request that didn't go through Read", func(t *testing.T) {
		if _, ok := FromContext(httptest.NewRequest(http.MethodPost, "/", nil)); ok {
			t.Error("found a payload")
		}
	})
}
