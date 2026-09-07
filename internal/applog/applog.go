// Package applog sets up this service's log destination: a plain file
// on disk (so a running instance's logs can be read/grepped/tailed
// after the fact, e.g. from outside the process, without needing to
// have been watching stdout when something happened) plus stdout (so
// `go run`/`docker compose logs` still show everything live during
// local development). Both Echo's per-request access log and this
// service's own log.Printf/log.Fatalf calls (auth failures, startup
// errors, migration status) are routed through the same destination,
// so one file has the full picture of a request and how it was
// handled — see cmd/server/main.go's use of Open and log.SetOutput.
package applog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Writer is an io.Writer that also knows how to close whatever file it
// opened, so the caller can defer a single Close() regardless of
// whether logs are going to a file, stdout only, or both.
type Writer struct {
	io.Writer
	file *os.File
}

// Close closes the underlying log file, if one was opened. Safe to call
// on a stdout-only Writer (Open("")) — it's a no-op in that case.
func (w *Writer) Close() error {
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

// Open returns a Writer that duplicates everything written to it into
// both path and stdout. An empty path means "stdout only" — this is
// what lets LOG_FILE stay optional (see internal/config), rather than
// forcing every environment to manage a log file on disk.
//
// Any directories in path that don't exist yet are created
// automatically (e.g. the default "logs/backend.log" needs a logs/
// directory on a fresh checkout) — the file itself is opened for
// append, so restarting the service doesn't discard its previous run's
// log history.
func Open(path string) (*Writer, error) {
	if path == "" {
		return &Writer{Writer: os.Stdout}, nil
	}

	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating log directory %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening log file %s: %w", path, err)
	}

	return &Writer{Writer: io.MultiWriter(os.Stdout, f), file: f}, nil
}
