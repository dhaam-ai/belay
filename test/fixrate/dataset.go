//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// This file downloads the pinned SWE-bench corpus and refuses anything that is
// not it.
//
// # Why verify at all
//
// swebench.go's Source is a declaration: a caller says which dataset and which
// revision its records came from, and nothing offline can check the claim. A
// published resolve rate is only as good as that claim, so the claim has to
// be checked where the records arrive, which is here.
//
// # What is checked, and against what
//
// The pinned revision of SWE-bench/SWE-bench_Verified holds one data file,
// data/test-00000-of-00001.parquet. Its size and the sha256 of its content are
// pinned in PinnedDataFile, copied from the Hugging Face tree API for that
// revision (TestPinnedDataFile cites the URL). Download fetches the file from
// the revision's resolve URL and checks, before anything is kept:
//
//   - that the server did not say it served another revision. The resolve
//     endpoint answers with X-Repo-Commit, the commit it resolved the request
//     to; one that differs from the pin is refused. A server that sends no
//     such header -- a mirror, say -- is not refused for that, because
//   - the content must be exactly the pinned bytes: the pinned size, and the
//     pinned sha256. That is the check that matters. Content addressed by
//     its hash is the pinned corpus whoever served it; anything else is not,
//     whatever it is called.
//
// A file that fails either check is never kept: it is written to a temporary
// file and removed, so no later step can pick it up as verified. A copy
// already present from an earlier run is verified again before it is used,
// and one that fails is refused rather than quietly fetched over -- a corrupt
// cache is something a person should know about.
//
// # What it does not do
//
// It does not read the parquet file. Go has no parquet reader in its standard
// library and this harness takes no new module for one; converting the file
// to the JSON records swebench.go decodes is the official harness's own
// loader's job (see the harness file), so the records belay reads are the
// records the harness grades.

// DatasetFile is one file of a Hugging Face dataset repository at an exact
// revision, with the size and the sha256 its content must have.
type DatasetFile struct {
	// Source is the dataset and the exact revision the file belongs to.
	Source Source
	// Path is the file's path inside the repository, slash-separated.
	Path string
	// Size is the content's length in bytes.
	Size int64
	// SHA256 is the lowercase hex sha256 of the content.
	SHA256 string
}

// PinnedDataFile is the pinned revision's data file, as the Hugging Face tree
// API reported it on 2026-10-01; see TestPinnedDataFile for the source.
var PinnedDataFile = DatasetFile{
	Source: PinnedSource,
	Path:   "data/test-00000-of-00001.parquet",
	Size:   6304616,
	SHA256: "030cfd7f2a704c4c0226e7f104c725a3b41230b1d3517f9c915ad7ea5be3fa25",
}

// HuggingFaceEndpoint is the Hugging Face Hub, where Download fetches from
// unless a caller names a mirror.
const HuggingFaceEndpoint = "https://huggingface.co"

// ErrDatasetMismatch reports a dataset file that is not the one its pin
// names: different bytes, a different size, or a server that said it served
// a different revision.
var ErrDatasetMismatch = errors.New("fixrate: dataset file does not match its pin")

// errIncompletePin reports a DatasetFile missing something Download needs to
// verify against. It is a caller bug, not a mismatch.
var errIncompletePin = errors.New("fixrate: dataset file pin is incomplete")

// URL returns where endpoint serves f: its resolve URL at the pinned
// revision.
func (f DatasetFile) URL(endpoint string) string {
	return strings.TrimSuffix(endpoint, "/") + "/datasets/" + f.Source.Dataset + "/resolve/" + f.Source.Revision + "/" + f.Path
}

// validate reports whether f names everything Download verifies.
func (f DatasetFile) validate() error {
	if err := f.Source.Validate(); err != nil {
		return err
	}
	switch {
	case f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, ".."):
		return fmt.Errorf("%w: unusable path %q", errIncompletePin, f.Path)
	case f.Size <= 0:
		return fmt.Errorf("%w: no size for %s", errIncompletePin, f.Path)
	case !isSHA256(f.SHA256):
		return fmt.Errorf("%w: %q is not a sha256 for %s", errIncompletePin, f.SHA256, f.Path)
	default:
		return nil
	}
}

// isSHA256 reports whether s is 64 lowercase hexadecimal digits.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Download fetches f from endpoint into dir, verifies it against its pin, and
// returns the path it is kept at: dir joined with the file's base name. See
// this file's header comment for what is checked.
//
// A verified copy already at that path is returned without a request. A copy
// that does not verify is refused, and left where it is. client may be nil,
// for http.DefaultClient; its redirect policy is wrapped, not replaced, so a
// caller's own limit still applies.
func Download(ctx context.Context, client *http.Client, endpoint string, f DatasetFile, dir string) (string, error) {
	if err := f.validate(); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, path.Base(f.Path))
	if _, err := os.Stat(dest); err == nil {
		if err := verifyFile(dest, f); err != nil {
			return "", fmt.Errorf("%w; it was kept from an earlier download: delete it to fetch the pinned file again", err)
		}
		return dest, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("fixrate: create %s: %w", dir, err)
	}

	resp, served, err := fetch(ctx, client, f.URL(endpoint))
	if err != nil {
		return "", err
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		return "", errors.Join(fmt.Errorf("fixrate: fetching %s: %s", f.URL(endpoint), resp.Status), resp.Body.Close())
	case served != "" && served != f.Source.Revision:
		return "", errors.Join(fmt.Errorf("%w: %s says it served revision %s of %s, not the pinned %s",
			ErrDatasetMismatch, endpoint, served, f.Source.Dataset, f.Source.Revision), resp.Body.Close())
	}

	tmp, err := createTemp(dest)
	if err != nil {
		return "", errors.Join(err, resp.Body.Close())
	}
	if err := errors.Join(copyVerified(tmp, resp.Body, f), resp.Body.Close()); err != nil {
		return "", discardTemp(tmp, err)
	}
	if err := commitTemp(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// fetch requests url and returns the response together with the revision
// the endpoint said it resolved the request to, from the X-Repo-Commit header
// of whichever response in the redirect chain carried it first.
func fetch(ctx context.Context, client *http.Client, url string) (*http.Response, string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var served string
	c := *client
	next := client.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if served == "" && req.Response != nil {
			served = req.Response.Header.Get("X-Repo-Commit")
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("fixrate: stopped after 10 redirects")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("fixrate: request %s: %w", url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fixrate: fetching %s: %w", url, err)
	}
	if served == "" {
		served = resp.Header.Get("X-Repo-Commit")
	}
	return resp, served, nil
}

// copyVerified copies src into dst, failing with ErrDatasetMismatch unless
// what it copied is exactly f's size and sha256. It never reads more than one
// byte past the pinned size, so a server that sends a huge wrong file cannot
// fill the disk before it is refused.
func copyVerified(dst io.Writer, src io.Reader, f DatasetFile) error {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, f.Size+1))
	if err != nil {
		return fmt.Errorf("fixrate: reading %s: %w", f.Path, err)
	}
	return checkDigest(f, n, h.Sum(nil))
}

// verifyFile checks that the file at p is exactly f's pinned content.
func verifyFile(p string, f DatasetFile) error {
	file, err := os.Open(p) //nolint:gosec // a path Download itself composed
	if err != nil {
		return fmt.Errorf("fixrate: open %s: %w", p, err)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(file, f.Size+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("fixrate: reading %s: %w", p, err)
	}
	return checkDigest(f, n, h.Sum(nil))
}

// checkDigest compares a read of n bytes hashing to sum with f's pin.
func checkDigest(f DatasetFile, n int64, sum []byte) error {
	switch got := hex.EncodeToString(sum); {
	case n != f.Size:
		return fmt.Errorf("%w: %s is %d byte(s) long, pinned at %d", ErrDatasetMismatch, f.Path, n, f.Size)
	case got != f.SHA256:
		return fmt.Errorf("%w: %s has sha256 %s, pinned at %s", ErrDatasetMismatch, f.Path, got, f.SHA256)
	default:
		return nil
	}
}

// PinnedRecordCount is the number of records the pinned revision's test split
// holds, per the Hugging Face datasets-server: num_examples 500
// (https://datasets-server.huggingface.co/info?dataset=SWE-bench%2FSWE-bench_Verified).
const PinnedRecordCount = 500

// sweBenchConvertScript turns the pinned parquet file into the JSON-lines
// records file the corpus reads and the harness grades against, through the
// harness's own loader: load_swebench_dataset reads a local .parquet file
// with the same datasets library it would load the hub copy with
// (swebench/harness/utils.py at swebench 5.0.2). Keys are sorted, so the same
// records always convert to the same bytes, and the file appears under its
// name only once it is complete.
const sweBenchConvertScript = `import json, os, sys
from swebench.harness.utils import load_swebench_dataset

source, target = sys.argv[1:3]
partial = target + ".partial"
with open(partial, "w", encoding="utf-8") as out:
    for row in load_swebench_dataset(source, "test"):
        out.write(json.dumps(dict(row), ensure_ascii=False, sort_keys=True) + "\n")
    out.flush()
    os.fsync(out.fileno())
os.replace(partial, target)
`

// Records is a converted and checked set of SWE-bench records: the
// JSON-lines file, its sha256, and the records decoded from it.
type Records struct {
	// Path is the records file. The harness is given this same file.
	Path string
	// SHA256 is the lowercase hex sha256 of the file, which a corpus pins
	// so that a resumed corpus can tell its records have not changed.
	SHA256 string
	// Instances are the records, in file order.
	Instances []Instance
}

// ConvertRecords converts source, a dataset file Download verified, into the
// records file target with the harness's own loader, and checks the result:
// every record decodes against the pinned schema (DecodeInstances refuses an
// unknown field), there are exactly want of them, and no instance id appears
// twice. A target that already exists is read and checked without converting
// again, which is what a resumed corpus does.
//
// A failed check wraps ErrDatasetMismatch: a conversion that dropped or
// doubled a record would make a resolve rate over a corpus that is not the
// pinned one, whatever its label says.
func ConvertRecords(ctx context.Context, py Python, source, target string, want int) (Records, error) {
	if want <= 0 {
		return Records{}, fmt.Errorf("fixrate: ConvertRecords needs the number of records to expect, got %d", want)
	}
	switch _, err := os.Stat(target); {
	case errors.Is(err, os.ErrNotExist):
		if _, err := py.output(ctx, "-c", sweBenchConvertScript, source, target); err != nil {
			return Records{}, fmt.Errorf("fixrate: converting %s with the harness's loader: %w", source, err)
		}
	case err != nil:
		return Records{}, fmt.Errorf("fixrate: %w", err)
	}
	return readRecords(target, want)
}

// readRecords reads and checks a records file; see ConvertRecords.
func readRecords(path string, want int) (Records, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a records file this package converted
	if err != nil {
		return Records{}, fmt.Errorf("fixrate: reading %s: %w", path, err)
	}
	instances, err := DecodeInstances(bytes.NewReader(data))
	if err != nil {
		return Records{}, fmt.Errorf("%w: %s: %w", ErrDatasetMismatch, path, err)
	}
	if len(instances) != want {
		return Records{}, fmt.Errorf("%w: %s holds %d record(s), pinned at %d", ErrDatasetMismatch, path, len(instances), want)
	}
	seen := make(map[string]bool, len(instances))
	for _, inst := range instances {
		if seen[inst.InstanceID] {
			return Records{}, fmt.Errorf("%w: %s holds instance %q twice", ErrDatasetMismatch, path, inst.InstanceID)
		}
		seen[inst.InstanceID] = true
	}
	sum := sha256.Sum256(data)
	return Records{Path: path, SHA256: hex.EncodeToString(sum[:]), Instances: instances}, nil
}

// Python is an interpreter with swebench installed: the official harness
// runs under it, and so do this package's two small helpers that ask the
// harness's own code a question.
type Python struct {
	// Path is the interpreter to run, such as "python3" or a virtualenv's
	// bin/python.
	Path string
	// Env is added to the environment it inherits from this process.
	Env []string
}

// output runs the interpreter with args and returns what it printed on
// stdout. A failure carries the end of what it printed on stderr, which is
// where Python puts a traceback.
func (p Python) output(ctx context.Context, args ...string) ([]byte, error) {
	if p.Path == "" {
		return nil, errors.New("no Python interpreter configured")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.Path, args...) //nolint:gosec // the configured interpreter, with this package's own arguments
	cmd.Env = append(os.Environ(), p.Env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, lastLines(stderr.String(), 3))
	}
	return stdout.Bytes(), nil
}
