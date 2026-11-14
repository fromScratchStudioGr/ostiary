// Package payload reads what the gateway needs from a request body, once, by
// walking only its top level: the model it's for, and where the members it
// may patch sit. Nothing else in the body is decoded. A body the provider
// can't parse further down is passed on, and the provider turns it down.
package payload

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/jsonscan"
	"github.com/fromScratchStudioGr/ostiary/internal/metrics"
)

// Payload is what was read from a request body.
type Payload struct {
	// Model is the exact "model" member, as a provider reads it.
	Model string
	// Body is the request body itself.
	Body []byte
	// Stream and StreamOptions are where those members sit in Body.
	Stream, StreamOptions Member
}

// Member is where a top-level member's value sits in a body, body[Start:End],
// and how many times its key appears. With a repeated key, Start and End
// point at the last copy.
type Member struct {
	Start, End, Seen int
}

type contextKey struct{}

// Read reads the request body, finds its model, and passes the request on
// with the Payload in its context and the body ready to be read again. A
// request whose model can't be told for certain is turned down: a body that
// isn't a JSON object, or has no model, more than one, or one that isn't a
// non-empty string.
func Read(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Sized up front when the length is known, so the buffer isn't grown
		// (and copied) as a large body comes in.
		stopReadBody := metrics.Observe("read_payload_body")
		buf := bytes.NewBuffer(make([]byte, 0, max(r.ContentLength, 0)+bytes.MinRead))
		_, err := buf.ReadFrom(r.Body)
		stopReadBody()
		if err != nil {
			slog.Warn("read-payload: could not read request body", "error", err)
			api.WriteError(w, "Could not read request body", http.StatusBadRequest)
			return
		}
		body := buf.Bytes()
		r.Body.Close()

		p := Payload{Body: body}
		var model Member
		stopScan := metrics.Observe("read_payload_scan")
		err = jsonscan.TopLevel(body, func(key []byte, start, end int) {
			switch string(key) {
			case "model":
				model = Member{Start: start, End: end, Seen: model.Seen + 1}
			case "stream":
				p.Stream = Member{Start: start, End: end, Seen: p.Stream.Seen + 1}
			case "stream_options":
				p.StreamOptions = Member{Start: start, End: end, Seen: p.StreamOptions.Seen + 1}
			}
		})
		stopScan()
		if err != nil {
			slog.Warn("read-payload: body is not a JSON object", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if problem := readModel(body, model, &p.Model); problem != "" {
			slog.Warn("read-payload: no usable model", "problem", problem)
			api.WriteError(w, "Invalid payload: "+problem, http.StatusBadRequest)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, p)))
	}
}

// readModel decodes the model member into model, or says why it can't be
// used.
func readModel(body []byte, member Member, model *string) string {
	switch {
	case member.Seen == 0:
		return "the model is missing"
	case member.Seen > 1:
		// A provider might read either copy, and bill one while running the
		// other.
		return "the model must be given only once"
	}
	if err := json.Unmarshal(body[member.Start:member.End], model); err != nil {
		return "the model must be a string"
	}
	if *model == "" {
		return "the model is missing"
	}
	return ""
}

// FromContext returns the Payload Read put in the request's context.
func FromContext(r *http.Request) (Payload, bool) {
	p, ok := r.Context().Value(contextKey{}).(Payload)
	return p, ok
}
