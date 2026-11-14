package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithHSTS(t *testing.T) {
	const header = "Strict-Transport-Security"

	// serve sends a request through WithHSTS around a handler that answers
	// with the given status and body, and returns the response and how many
	// times the handler ran. Headers are read from rec.Result(), which is
	// what was sent when the status was written: rec.Header() would also show
	// a header set afterwards, which a real server drops.
	serve := func(enabled bool, status int, body string) (*httptest.ResponseRecorder, int) {
		calls := 0
		handler := WithHSTS(enabled, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))
		return rec, calls
	}

	t.Run("tells browsers to use HTTPS for a year, subdomains included, when enabled", func(t *testing.T) {
		rec, _ := serve(true, http.StatusOK, "")

		if got, want := rec.Result().Header.Get(header), "max-age=31536000; includeSubDomains"; got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	})

	t.Run("sends nothing when disabled, so plain-HTTP development keeps working", func(t *testing.T) {
		rec, _ := serve(false, http.StatusOK, "")

		if values, sent := rec.Result().Header[header]; sent {
			t.Errorf("%s = %q, want it absent", header, values)
		}
	})

	t.Run("sends it on an error response too", func(t *testing.T) {
		rec, _ := serve(true, http.StatusInternalServerError, "boom")

		if rec.Result().Header.Get(header) == "" {
			t.Errorf("%s missing from a 500 response", header)
		}
	})

	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled]+": passes the request on and keeps the response", func(t *testing.T) {
			rec, calls := serve(enabled, http.StatusCreated, "created")

			if calls != 1 {
				t.Errorf("the wrapped handler ran %d times, want once", calls)
			}
			if rec.Code != http.StatusCreated || rec.Body.String() != "created" {
				t.Errorf("response = %d %q, want 201 \"created\"", rec.Code, rec.Body.String())
			}
		})
	}
}
