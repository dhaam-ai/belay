//go:build fixrate

package fixrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPinnedDataFile pins the one data file of the pinned revision, exactly
// as the Hugging Face tree API reported it at that revision on 2026-10-01:
// https://huggingface.co/api/datasets/SWE-bench/SWE-bench_Verified/tree/78f471bf655a3137b2e8a75af1501690ec009ec3?recursive=true&expand=true
// lists data/test-00000-of-00001.parquet with size 6304616 and an LFS oid
// (the sha256 of its content) of 030cfd7f...fa25. The resolve endpoint's
// own headers agree: x-linked-size 6304616, x-linked-etag the same sha256,
// and x-repo-commit the pinned revision.
func TestPinnedDataFile(t *testing.T) {
	t.Parallel()

	want := DatasetFile{
		Source: PinnedSource,
		Path:   "data/test-00000-of-00001.parquet",
		Size:   6304616,
		SHA256: "030cfd7f2a704c4c0226e7f104c725a3b41230b1d3517f9c915ad7ea5be3fa25",
	}
	if PinnedDataFile != want {
		t.Errorf("PinnedDataFile = %+v\nwant %+v", PinnedDataFile, want)
	}
	wantURL := "https://huggingface.co/datasets/SWE-bench/SWE-bench_Verified/resolve/" +
		"78f471bf655a3137b2e8a75af1501690ec009ec3/data/test-00000-of-00001.parquet"
	if got := PinnedDataFile.URL(HuggingFaceEndpoint); got != wantURL {
		t.Errorf("URL() = %q\nwant %q", got, wantURL)
	}
}

// syntheticDataFile is a stand-in for the pinned parquet file. Its bytes are
// invented, so these tests exercise the verification and nothing about the
// dataset itself.
func syntheticDataFile(t *testing.T, body []byte) DatasetFile {
	t.Helper()
	sum := sha256.Sum256(body)
	return DatasetFile{
		Source: PinnedSource,
		Path:   "data/test-00000-of-00001.parquet",
		Size:   int64(len(body)),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

// hubServer imitates huggingface.co's resolve endpoint: the request for the
// file at a revision is answered with a redirect carrying X-Repo-Commit, and
// the redirect target serves the bytes, as the real endpoint's CDN does.
type hubServer struct {
	*httptest.Server
	requests atomic.Int32
	paths    chan string
}

func newHubServer(t *testing.T, body []byte, commit string, status int) *hubServer {
	t.Helper()
	h := &hubServer{paths: make(chan string, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/datasets/", func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.paths <- r.URL.Path
		if commit != "" {
			w.Header().Set("X-Repo-Commit", commit)
		}
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		http.Redirect(w, r, "/cdn/blob", http.StatusFound)
	})
	mux.HandleFunc("/cdn/blob", func(w http.ResponseWriter, _ *http.Request) {
		h.requests.Add(1)
		if _, err := w.Write(body); err != nil {
			t.Errorf("serving the blob: %v", err)
		}
	})
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)
	return h
}

func TestDownload_VerifiesAndKeepsTheFile(t *testing.T) {
	t.Parallel()

	body := []byte("PAR1 a synthetic stand-in for a dataset file PAR1")
	f := syntheticDataFile(t, body)
	hub := newHubServer(t, body, SWEBenchRevision, http.StatusOK)
	dir := t.TempDir()

	got, err := Download(t.Context(), hub.Client(), hub.URL, f, dir)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if want := filepath.Join(dir, "test-00000-of-00001.parquet"); got != want {
		t.Errorf("Download() = %q, want %q", got, want)
	}
	data, err := os.ReadFile(got) //nolint:gosec // a path this test's own temp directory produced
	if err != nil {
		t.Fatalf("reading the download: %v", err)
	}
	if string(data) != string(body) {
		t.Errorf("downloaded %q, want %q", data, body)
	}
	if path := <-hub.paths; path != "/datasets/SWE-bench/SWE-bench_Verified/resolve/"+SWEBenchRevision+"/data/test-00000-of-00001.parquet" {
		t.Errorf("requested %q, want the file at the pinned revision", path)
	}
	assertNoTempFiles(t, dir)
}

// TestDownload_RefusesWhatDoesNotMatchThePin is the point of the file: a
// corpus is only the pinned corpus if its bytes are the pinned bytes. Every
// mismatch is refused, and nothing is left behind for a later run to pick up
// as if it had been verified.
func TestDownload_RefusesWhatDoesNotMatchThePin(t *testing.T) {
	t.Parallel()

	body := []byte("PAR1 a synthetic stand-in for a dataset file PAR1")
	f := syntheticDataFile(t, body)
	other := "0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name     string
		served   []byte
		commit   string
		status   int
		mismatch bool
	}{
		{"different bytes of the same length", []byte(strings.ToUpper(string(body))), SWEBenchRevision, http.StatusOK, true},
		{"more bytes than pinned", append(append([]byte{}, body...), 'x'), SWEBenchRevision, http.StatusOK, true},
		{"fewer bytes than pinned", body[:len(body)-1], SWEBenchRevision, http.StatusOK, true},
		{"the server says it served another revision", body, other, http.StatusOK, true},
		{"the file is not there", body, "", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newHubServer(t, tt.served, tt.commit, tt.status)
			dir := t.TempDir()

			got, err := Download(t.Context(), hub.Client(), hub.URL, f, dir)
			if err == nil {
				t.Fatalf("Download() = %q, nil error; want a refusal", got)
			}
			if errors.Is(err, ErrDatasetMismatch) != tt.mismatch {
				t.Errorf("Download() error = %v; errors.Is(ErrDatasetMismatch) = %v, want %v",
					err, errors.Is(err, ErrDatasetMismatch), tt.mismatch)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "test-00000-of-00001.parquet")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused download left a file where a verified one belongs (stat error %v)", statErr)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

// TestDownload_ReusesAVerifiedCopy: a resumed corpus must not fetch again,
// but it must not trust a file merely for being there either.
func TestDownload_ReusesAVerifiedCopy(t *testing.T) {
	t.Parallel()

	body := []byte("PAR1 a synthetic stand-in for a dataset file PAR1")
	f := syntheticDataFile(t, body)

	t.Run("a copy that verifies is used as it is", func(t *testing.T) {
		t.Parallel()
		hub := newHubServer(t, body, SWEBenchRevision, http.StatusOK)
		dir := t.TempDir()
		dest := filepath.Join(dir, "test-00000-of-00001.parquet")
		if err := os.WriteFile(dest, body, 0o600); err != nil {
			t.Fatalf("seeding the copy: %v", err)
		}
		got, err := Download(t.Context(), hub.Client(), hub.URL, f, dir)
		if err != nil {
			t.Fatalf("Download() error = %v", err)
		}
		if got != dest {
			t.Errorf("Download() = %q, want %q", got, dest)
		}
		if n := hub.requests.Load(); n != 0 {
			t.Errorf("Download() made %d request(s) for a file it already had, verified", n)
		}
	})

	t.Run("a copy that does not verify is refused and left alone", func(t *testing.T) {
		t.Parallel()
		hub := newHubServer(t, body, SWEBenchRevision, http.StatusOK)
		dir := t.TempDir()
		dest := filepath.Join(dir, "test-00000-of-00001.parquet")
		corrupt := []byte("PAR1 not the pinned bytes at all PAR1")
		if err := os.WriteFile(dest, corrupt, 0o600); err != nil {
			t.Fatalf("seeding the copy: %v", err)
		}
		if _, err := Download(t.Context(), hub.Client(), hub.URL, f, dir); !errors.Is(err, ErrDatasetMismatch) {
			t.Fatalf("Download() error = %v, want it to wrap ErrDatasetMismatch", err)
		}
		if n := hub.requests.Load(); n != 0 {
			t.Errorf("Download() made %d request(s); a copy that fails verification needs a person, not a quiet re-fetch", n)
		}
		data, err := os.ReadFile(dest) //nolint:gosec // this test's own temp directory
		if err != nil || string(data) != string(corrupt) {
			t.Errorf("the unverified copy was changed (err %v): %q", err, data)
		}
	})
}

// assertNoTempFiles fails t if dir holds anything other than a verified
// download: a temporary file left by an interrupted or refused one would be
// clutter at best, and at worst a file a careless later step picks up.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("%s holds a leftover temporary file %q", dir, e.Name())
		}
	}
}

// syntheticRecords returns the synthetic fixture records as JSON lines, the
// shape the conversion helper writes.
func syntheticRecords(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, inst := range swebenchFixture(t, "instances-array.json") {
		line, err := json.Marshal(inst)
		if err != nil {
			t.Fatalf("marshal fixture instance: %v", err)
		}
		out = append(append(out, line...), '\n')
	}
	return out
}

// fakePythonPath returns this test binary, for use as a Python interpreter
// together with the fakePythonEnv variable.
func fakePythonPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// TestConvertRecords: the records the arms read and the records the harness
// grades must be the same records, so they are produced by the harness's own
// loader and then checked here -- every record decodes against the pinned
// schema, the count is the pinned count, and no instance appears twice. A
// conversion that dropped or doubled a record would otherwise make a resolve
// rate over a corpus that is not the pinned one.
func TestConvertRecords(t *testing.T) {
	t.Parallel()

	good := syntheticRecords(t)
	tests := []struct {
		name    string
		source  []byte
		want    int
		wantErr bool
	}{
		{name: "the pinned count of well-formed records", source: good, want: 2},
		{name: "fewer records than pinned", source: good, want: 3, wantErr: true},
		{name: "a record twice", source: append(append([]byte{}, good...), bytes.SplitAfter(good, []byte("\n"))[0]...), want: 3, wantErr: true},
		{name: "a field the pinned schema does not have", source: []byte(`{"instance_id":"x","FAIL_TO_FAIL":[]}` + "\n"), want: 1, wantErr: true},
		{name: "a loader that fails", source: []byte("CONVERT-FAILS\n"), want: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			source := filepath.Join(dir, "test-00000-of-00001.parquet")
			if err := os.WriteFile(source, tt.source, 0o600); err != nil {
				t.Fatalf("write source: %v", err)
			}
			py := Python{Path: fakePythonPath(t), Env: []string{fakePythonEnv + "=1"}}
			recs, err := ConvertRecords(t.Context(), py, source, filepath.Join(dir, "instances.jsonl"), tt.want)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ConvertRecords() = %d record(s), nil error; want a refusal", len(recs.Instances))
				}
				return
			}
			if err != nil {
				t.Fatalf("ConvertRecords() error = %v", err)
			}
			if len(recs.Instances) != tt.want {
				t.Errorf("ConvertRecords() = %d record(s), want %d", len(recs.Instances), tt.want)
			}
			sum := sha256.Sum256(tt.source)
			if recs.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("SHA256 = %s, want the digest of the records file", recs.SHA256)
			}
		})
	}
}

// TestConvertRecords_ConvertsOnce: a resumed corpus reads the records it
// converted the first time, and must not convert again -- the records file is
// part of what the corpus pinned.
func TestConvertRecords_ConvertsOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "test-00000-of-00001.parquet")
	if err := os.WriteFile(source, syntheticRecords(t), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	target := filepath.Join(dir, "instances.jsonl")
	py := Python{Path: fakePythonPath(t), Env: []string{fakePythonEnv + "=1"}}
	first, err := ConvertRecords(t.Context(), py, source, target, 2)
	if err != nil {
		t.Fatalf("first ConvertRecords() error = %v", err)
	}
	second, err := ConvertRecords(t.Context(), py, source, target, 2)
	if err != nil {
		t.Fatalf("second ConvertRecords() error = %v", err)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("the two reads disagree: %s and %s", first.SHA256, second.SHA256)
	}
	runs, err := os.ReadFile(target + ".runs") //nolint:gosec // this test's own temp directory
	if err != nil {
		t.Fatalf("reading the run count: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("the loader ran %d time(s), want 1", len(runs))
	}
}
