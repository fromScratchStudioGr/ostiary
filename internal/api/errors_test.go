package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError(t *testing.T) {
	t.Run("answers with the given status and a JSON content type", func(t *testing.T) {
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				rec := httptest.NewRecorder()

				WriteError(rec, "nope", status)

				if rec.Code != status {
					t.Errorf("status = %d, want %d", rec.Code, status)
				}
				// Read from Result: it's what was sent when the status was
				// written, so a Content-Type set after that fails the test.
				if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
			})
		}
	})

	t.Run("sends the message as the only field, named error", func(t *testing.T) {
		rec := httptest.NewRecorder()

		WriteError(rec, "Forbidden", http.StatusForbidden)

		var body map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decoding the body: %v", err)
		}
		if len(body) != 1 || body["error"] != "Forbidden" {
			t.Errorf("body = %v, want only error: Forbidden", body)
		}
	})

	t.Run("keeps the body valid JSON and the message intact when it has quotes, newlines or markup", func(t *testing.T) {
		const message = "bad \"model\"\nline two <b>&</b> \\ end"
		rec := httptest.NewRecorder()

		WriteError(rec, message, http.StatusBadRequest)

		var body ErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("the body isn't valid JSON: %v", err)
		}
		if body.Error != message {
			t.Errorf("message = %q, want %q", body.Error, message)
		}
	})
}
