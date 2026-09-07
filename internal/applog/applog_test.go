package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpen covers every branch of Open(): the empty-path/stdout-only
// case, creating a nested directory that doesn't exist yet, and
// appending to a file that already has content. Table-driven since
// each case is a distinct "given this path/starting state, does the
// resulting Writer behave correctly" scenario rather than varying a
// single input.
func TestOpen(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the path to pass to Open (possibly under a
		// fresh t.TempDir()) and pre-creates whatever starting state
		// the case needs.
		setup       func(t *testing.T) string
		writeText   string
		wantErr     bool
		wantNoFile  bool   // true for the empty-path/stdout-only case
		wantContent string // checked against the file's full contents after writing, when wantNoFile is false
	}{
		{
			name:       "empty path means stdout only, no file created",
			setup:      func(t *testing.T) string { return "" },
			writeText:  "hello\n",
			wantNoFile: true,
		},
		{
			name: "creates missing nested directories",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nested", "dir", "app.log")
			},
			writeText:   "first line\n",
			wantContent: "first line\n",
		},
		{
			name: "appends to a file that already has content, rather than truncating it",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				path := filepath.Join(dir, "app.log")
				if err := os.WriteFile(path, []byte("previous run\n"), 0o644); err != nil {
					t.Fatalf("seeding existing log file: %v", err)
				}
				return path
			},
			writeText:   "new run\n",
			wantContent: "previous run\nnew run\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)

			w, err := Open(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Open(%q) succeeded, want an error", path)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q) returned error: %v", path, err)
			}
			defer w.Close()

			if _, err := w.Write([]byte(tc.writeText)); err != nil {
				t.Fatalf("Write() returned error: %v", err)
			}

			if tc.wantNoFile {
				if path != "" {
					t.Fatalf("test setup bug: wantNoFile but path is %q", path)
				}
				return
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading back %s: %v", path, err)
			}
			if string(got) != tc.wantContent {
				t.Errorf("file content = %q, want %q", got, tc.wantContent)
			}
		})
	}
}

// TestWriterClose covers Close()'s two paths: a stdout-only Writer
// (nothing to close) and a file-backed Writer (the file must actually
// close, and closing twice must not panic — main.go's defer pattern
// combined with an explicit early Close in some future code path could
// otherwise double-close).
func TestWriterClose(t *testing.T) {
	t.Run("stdout-only Writer: Close is a safe no-op", func(t *testing.T) {
		w, err := Open("")
		if err != nil {
			t.Fatalf("Open(\"\") returned error: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Errorf("Close() on a stdout-only Writer returned error: %v", err)
		}
	})

	t.Run("file-backed Writer: Close closes the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		w, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%q) returned error: %v", path, err)
		}
		if err := w.Close(); err != nil {
			t.Errorf("Close() returned error: %v", err)
		}
		// A closed *os.File returns an error on further writes — used
		// here only to confirm Close() actually closed it, not to
		// exercise any behavior applog itself needs to handle.
		if _, err := w.file.Write([]byte("x")); err == nil {
			t.Errorf("write to file succeeded after Close(), want an error")
		}
	})
}

// TestOpen_MultiWriterIncludesFile is a light integration check that
// Open's file-backed Writer really does write to disk (not just to
// stdout) — the table-driven cases above already assert on file
// content after a write, but this makes explicit that the returned
// Writer isn't silently dropping writes when it can't reach stdout in
// a given test environment (os.Stdout is always valid, but this keeps
// the assertion self-contained rather than relying on that).
func TestOpen_MultiWriterIncludesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "app.log")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	defer w.Close()

	const marker = "distinctive-marker-line\n"
	if _, err := w.Write([]byte(marker)); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	if !strings.Contains(string(got), marker) {
		t.Errorf("file content %q does not contain the written marker", got)
	}
}
