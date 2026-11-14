package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestRequireToken(t *testing.T) {
	const token = "health-secret"

	// send puts a request carrying the given headers through
	// RequireToken, and returns the response and how many times the
	// wrapped handler ran.
	send := func(headers map[string]string) (*httptest.ResponseRecorder, int) {
		calls := 0
		handler := RequireToken(token, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte("healthy"))
		})
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec, calls
	}

	t.Run("passes a request with the token on to the wrapped handler", func(t *testing.T) {
		rec, calls := send(map[string]string{"x-api-key": token})

		if calls != 1 {
			t.Errorf("the wrapped handler ran %d times, want once", calls)
		}
		if rec.Code != http.StatusAccepted || rec.Body.String() != "healthy" {
			t.Errorf("response = %d %q, want the wrapped handler's own", rec.Code, rec.Body.String())
		}
	})

	t.Run("answers 401, without running the wrapped handler, to anything else", func(t *testing.T) {
		tests := []struct {
			name    string
			headers map[string]string
		}{
			{"no token", nil},
			{"an empty token", map[string]string{"x-api-key": ""}},
			{"a wrong token", map[string]string{"x-api-key": "wrong"}},
			{"a prefix of the token", map[string]string{"x-api-key": token[:len(token)-1]}},
			{"the token with something added", map[string]string{"x-api-key": token + "x"}},
			{"the token in another header", map[string]string{"Authorization": "Bearer " + token}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec, calls := send(tt.headers)

				if calls != 0 {
					t.Errorf("the wrapped handler ran %d times for a rejected request", calls)
				}
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", rec.Code)
				}
				if rec.Body.Len() != 0 {
					t.Errorf("body = %q, want it empty, so nothing leaks about why", rec.Body.String())
				}
			})
		}
	})
}

// healthResponse is the JSON HealthHandler answers with.
type healthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
}

func TestHealthHandler(t *testing.T) {
	// get runs the handler for a request with the given context and decodes
	// its answer.
	get := func(t *testing.T, ctx context.Context, checks ...HealthCheck) (*httptest.ResponseRecorder, healthResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		HealthHandler("1.2.3", checks...)(rec, req)

		var body healthResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decoding the body: %v", err)
		}
		return rec, body
	}
	passing := func(name string) HealthCheck {
		return HealthCheck{Name: name, Check: func(context.Context) error { return nil }}
	}

	t.Run("reports ok, with every check, when they all pass", func(t *testing.T) {
		rec, body := get(t, context.Background(), passing("postgres"), passing("redis"))

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		want := healthResponse{Status: "ok", Version: "1.2.3", Checks: map[string]string{"postgres": "ok", "redis": "ok"}}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("body = %+v, want %+v", body, want)
		}
	})

	t.Run("reports ok, with no checks, when there are none to run", func(t *testing.T) {
		rec, body := get(t, context.Background())

		if rec.Code != http.StatusOK || body.Status != "ok" || len(body.Checks) != 0 {
			t.Errorf("response = %d %+v, want 200 and ok with no checks", rec.Code, body)
		}
	})

	t.Run("answers 503, with the error of the check that failed, when one fails", func(t *testing.T) {
		ranAfter := false
		failing := HealthCheck{Name: "redis", Check: func(context.Context) error { return errors.New("connection refused") }}
		later := HealthCheck{Name: "kms", Check: func(context.Context) error { ranAfter = true; return nil }}

		rec, body := get(t, context.Background(), passing("postgres"), failing, later)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		want := healthResponse{
			Status:  "unhealthy",
			Version: "1.2.3",
			Checks:  map[string]string{"postgres": "ok", "redis": "connection refused", "kms": "ok"},
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("body = %+v, want %+v", body, want)
		}
		if !ranAfter {
			t.Error("a check after the failing one didn't run, so its state is unknown")
		}
	})

	t.Run("gives the checks a deadline of about three seconds", func(t *testing.T) {
		var deadline time.Time
		var hasDeadline bool
		probe := HealthCheck{Name: "probe", Check: func(ctx context.Context) error {
			deadline, hasDeadline = ctx.Deadline()
			return nil
		}}

		get(t, context.Background(), probe)

		if !hasDeadline {
			t.Fatal("the checks' context has no deadline, so a hung dependency would hang the endpoint")
		}
		if remaining := time.Until(deadline); remaining <= 2*time.Second || remaining > 3*time.Second {
			t.Errorf("deadline is %v away, want about 3s", remaining)
		}
	})

	t.Run("stops the checks when the request is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		waiting := HealthCheck{Name: "waiting", Check: func(ctx context.Context) error { return ctx.Err() }}

		rec, body := get(t, ctx, waiting)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if got := body.Checks["waiting"]; got != context.Canceled.Error() {
			t.Errorf("check result = %q, want %q", got, context.Canceled.Error())
		}
	})
}
