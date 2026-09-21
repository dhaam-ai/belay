package ndjson

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errReject is what the test decoder returns for a line it will not accept.
var errReject = errors.New("rejected")

// acceptUnlessBad is a decoder that accepts every line except one that
// starts with "bad", recording each accepted line.
func acceptUnlessBad(got *[]string) func([]byte) error {
	return func(line []byte) error {
		if bytes.HasPrefix(line, []byte("bad")) {
			return errReject
		}
		*got = append(*got, string(line))
		return nil
	}
}

func TestScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          string
		wantLines      []string
		wantIncomplete bool
		wantLineErr    int // 0 means no *LineError expected
	}{
		{name: "empty input", input: ""},
		{
			name:      "complete lines",
			input:     "a\nb\nc\n",
			wantLines: []string{"a", "b", "c"},
		},
		{
			// The content is intact; only its delimiter was cut off.
			name:      "final line missing only its newline",
			input:     "a\nb",
			wantLines: []string{"a", "b"},
		},
		{
			name:           "torn final line is incomplete, not an error",
			input:          "a\nb\nbad-half-writ",
			wantLines:      []string{"a", "b"},
			wantIncomplete: true,
		},
		{
			name:           "torn final line that kept its newline is still the last line",
			input:          "a\nbad\n",
			wantLines:      []string{"a"},
			wantIncomplete: true,
		},
		{
			// A rejected line with another after it cannot be an interrupted
			// append, since only the end of the file is ever written.
			name:        "rejected middle line is corruption",
			input:       "a\nbad\nc\n",
			wantLines:   []string{"a"},
			wantLineErr: 2,
		},
		{
			name:        "rejected first line followed by more",
			input:       "bad\nb\n",
			wantLineErr: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			incomplete, err := Scan(strings.NewReader(tt.input), acceptUnlessBad(&got))

			if tt.wantLineErr != 0 {
				var lineErr *LineError
				if !errors.As(err, &lineErr) {
					t.Fatalf("Scan() error = %v (%T), want *LineError", err, err)
				}
				if lineErr.Line != tt.wantLineErr {
					t.Errorf("LineError.Line = %d, want %d", lineErr.Line, tt.wantLineErr)
				}
				if !errors.Is(err, errReject) {
					t.Errorf("Scan() error = %v, want it to reach the decoder's own error", err)
				}
			} else if err != nil {
				t.Fatalf("Scan() error = %v, want nil", err)
			}

			if incomplete != tt.wantIncomplete {
				t.Errorf("Scan() incomplete = %v, want %v", incomplete, tt.wantIncomplete)
			}
			if strings.Join(got, "|") != strings.Join(tt.wantLines, "|") {
				t.Errorf("lines handed to each = %q, want %q", got, tt.wantLines)
			}
		})
	}
}

func TestScanRunawayLineIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", MaxLineBytes+1) + "\n"
	calls := 0
	_, err := Scan(strings.NewReader(huge), func([]byte) error { calls++; return nil })
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Scan() error = %v, want it to wrap bufio.ErrTooLong", err)
	}
	if calls != 0 {
		t.Errorf("each was called %d times for an oversized first line, want 0", calls)
	}
}

func TestAppendLine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "log.ndjson")
	// #nosec G304 -- path is under the test's own temp directory.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	for _, line := range []string{`{"n":1}`, `{"n":2}`} {
		if err := AppendLine(f, []byte(line)); err != nil {
			t.Fatalf("AppendLine(%s) error = %v", line, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// #nosec G304 -- path is under the test's own temp directory.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := "{\"n\":1}\n{\"n\":2}\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}

	if err := AppendLine(f, []byte(`{"n":3}`)); err == nil {
		t.Error("AppendLine on a closed file = nil error, want error")
	}
}

func TestRewriteCleanReplacesTheFileWithExactlyTheLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")
	if err := os.WriteFile(path, []byte("a\nb\nhalf-wr"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := RewriteClean(path, [][]byte{[]byte("a"), []byte("b")}); err != nil {
		t.Fatalf("RewriteClean() error = %v", err)
	}

	// #nosec G304 -- path is under the test's own temp directory.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := "a\nb\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %q after the heal, want only the healed file", names)
	}
}

func TestEnsureTrailingNewline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content *string // nil: the file does not exist
		want    *string // nil: the file must still not exist
	}{
		{name: "missing file is left missing"},
		{name: "empty file is left empty", content: ptr(""), want: ptr("")},
		{name: "a delimited file is unchanged", content: ptr("a\nb\n"), want: ptr("a\nb\n")},
		{name: "a cut-off delimiter is completed", content: ptr("a\nb"), want: ptr("a\nb\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "log.ndjson")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}

			if err := EnsureTrailingNewline(path); err != nil {
				t.Fatalf("EnsureTrailingNewline() error = %v", err)
			}

			// #nosec G304 -- path is under the test's own temp directory.
			got, err := os.ReadFile(path)
			if tt.want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("ReadFile() error = %v, want the file still not to exist", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(got) != *tt.want {
				t.Errorf("file = %q, want %q", got, *tt.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }
