package logging

import (
	"fmt"
	"log/slog"
	"os"

	slogseq "github.com/sokkalf/slog-seq"
)

// Setup wires up slog's default logger: always to local stderr, and
// additionally to a Seq server when seqURL is set (neither replaces the
// other). Returns a close func — callers should defer it, so any Seq
// events still buffered at shutdown get flushed rather than lost.
func Setup(seqURL, seqAPIKey string) func() {
	textHandler := slog.NewTextHandler(os.Stderr, nil)

	if seqURL == "" {
		slog.SetDefault(slog.New(textHandler))
		return func() {}
	}

	_, seqHandler := slogseq.NewLogger(seqURL,
		slogseq.WithAPIKey(seqAPIKey),
		slogseq.WithErrorHandlerFunc(func(err error) {
			fmt.Fprintf(os.Stderr, "seq logging: %v\n", err)
		}),
	)

	slog.SetDefault(slog.New(slog.NewMultiHandler(textHandler, seqHandler)))
	return func() {
		if err := seqHandler.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "seq logging: could not close: %v\n", err)
		}
	}
}
