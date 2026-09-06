//go:build unix

package exec

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Secret-shaped fixtures, assembled at run time.
//
// Testing a redactor requires strings shaped like real credentials, and a
// public repository runs a secret scanner over every file. Written as plain
// literals these trip it: GitHub opened alerts on the generic sk- key and the
// AWS session key below, which are an alphabet and AWS's own documentation
// placeholder respectively. Nothing here is a real credential and nothing
// needs rotating -- but an alert a maintainer has to dismiss on every push is
// a tax on everyone, and it trains people to wave through the one that matters.
//
// Splitting each value means no contiguous secret-shaped string exists in the
// source, while the tests still exercise the whole value at run time.
// TestSourceHasNoContiguousSecretLiterals keeps it that way.
var (
	fxAnthropicKey       = "sk-" + "ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	fxGenericSKKey       = "sk-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd"
	fxSonarUserToken     = "squ" + "_9f2c1ab34de5f6789012345678901234abcd0011"
	fxSonarProjectToken  = "sqp" + "_9f2c1ab34de5f6789012345678901234abcd0011"
	fxSonarAnalysisToken = "sqa" + "_9f2c1ab34de5f6789012345678901234abcd0011"
	fxGitHubPAT          = "ghp" + "_1234567890abcdefghijklmnopqrstuvwx"
	fxGitHubOAuth        = "gho" + "_1234567890abcdefghijklmnopqrstuvwx"
	fxGitHubFineGrained  = "github" + "_pat_11ABCDEFG0abcdefghij_KLMNOPQRSTUVWXYZ012345"
	fxAWSLongTermKey     = "AKIA" + "IOSFODNN7EXAMPLE"
	fxAWSSessionKey      = "ASIA" + "IOSFODNN7EXAMPLE"
)

// filler is padding with internal whitespace. Tests that stream a secret need
// enough surrounding bytes to push the writer past its hold-back threshold, so
// that the incremental flush path is exercised rather than a single flush at
// Close.
var filler = strings.Repeat("scanning module alpha beta gamma ", 8)

func TestRedactPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		want  string
		leaks string
	}{
		{
			name:  "anthropic key",
			in:    "using " + fxAnthropicKey + " now",
			want:  "using [REDACTED:ANTHROPIC_API_KEY] now",
			leaks: fxAnthropicKey,
		},
		{
			name:  "generic sk key",
			in:    "key " + fxGenericSKKey + " end",
			want:  "key [REDACTED:API_KEY] end",
			leaks: fxGenericSKKey,
		},
		{
			name:  "sonar squ token",
			in:    "token " + fxSonarUserToken + " ok",
			want:  "token [REDACTED:SONAR_TOKEN] ok",
			leaks: fxSonarUserToken,
		},
		{
			name:  "sonar sqp token",
			in:    "token " + fxSonarProjectToken + " ok",
			want:  "token [REDACTED:SONAR_TOKEN] ok",
			leaks: fxSonarProjectToken,
		},
		{
			name:  "sonar sqa token",
			in:    "token " + fxSonarAnalysisToken + " ok",
			want:  "token [REDACTED:SONAR_TOKEN] ok",
			leaks: fxSonarAnalysisToken,
		},
		{
			name:  "sonar token flag",
			in:    "-Dsonar.token=notaknownshape123456 -Dsonar.host.url=https://s",
			want:  "-Dsonar.token=[REDACTED:SONAR_TOKEN] -Dsonar.host.url=https://s",
			leaks: "notaknownshape123456",
		},
		{
			name:  "sonar login flag",
			in:    "sonar.login=abcdefghijklmnop",
			want:  "sonar.login=[REDACTED:SONAR_TOKEN]",
			leaks: "abcdefghijklmnop",
		},
		{
			name:  "github classic token",
			in:    "remote " + fxGitHubPAT + " set",
			want:  "remote [REDACTED:GITHUB_TOKEN] set",
			leaks: fxGitHubPAT,
		},
		{
			name:  "github oauth token",
			in:    "remote " + fxGitHubOAuth + " set",
			want:  "remote [REDACTED:GITHUB_TOKEN] set",
			leaks: fxGitHubOAuth,
		},
		{
			name:  "github fine grained pat",
			in:    "auth " + fxGitHubFineGrained + " done",
			want:  "auth [REDACTED:GITHUB_TOKEN] done",
			leaks: fxGitHubFineGrained,
		},
		//nolint:gosec // the AWS fixture is that vendor's own documentation placeholder.
		{
			name:  "aws access key",
			in:    "id " + fxAWSLongTermKey + " region us-east-1",
			want:  "id [REDACTED:AWS_ACCESS_KEY_ID] region us-east-1",
			leaks: fxAWSLongTermKey,
		},
		{
			name:  "aws session key",
			in:    "id " + fxAWSSessionKey + " region us-east-1",
			want:  "id [REDACTED:AWS_ACCESS_KEY_ID] region us-east-1",
			leaks: fxAWSSessionKey,
		},
		{
			name:  "bearer keeps scheme",
			in:    "Authorization: Bearer abcdefghijklmnopqrstuvwx trailing",
			want:  "Authorization: Bearer [REDACTED:BEARER_TOKEN] trailing",
			leaks: "abcdefghijklmnopqrstuvwx",
		},
		{
			name:  "jwt",
			in:    "cookie eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r end",
			want:  "cookie [REDACTED:JWT] end",
			leaks: "eyJzdWIiOiIxMjM0NTY3ODkwIn0",
		},
		{
			name:  "generic token flag",
			in:    "cli --api-key=hunter2hunter2hunter2 --verbose",
			want:  "cli --api-key=[REDACTED:TOKEN] --verbose",
			leaks: "hunter2hunter2hunter2",
		},
		{
			name:  "generic password flag",
			in:    "cli --password=correcthorsebattery",
			want:  "cli --password=[REDACTED:TOKEN]",
			leaks: "correcthorsebattery",
		},
		{
			name: "clean output untouched",
			in:   "ok  github.com/dhaam-ai/belay/internal/exec  0.412s",
			want: "ok  github.com/dhaam-ai/belay/internal/exec  0.412s",
		},
		{
			name: "version strings untouched",
			in:   "go version go1.26.3 darwin/arm64 sk-lite",
			want: "go version go1.26.3 darwin/arm64 sk-lite",
		},
	}

	r := NewRedactor()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := r.Redact(tc.in)
			if got != tc.want {
				t.Errorf("Redact()\n got: %q\nwant: %q", got, tc.want)
			}
			if tc.leaks != "" && strings.Contains(got, tc.leaks) {
				t.Errorf("secret %q survived redaction: %q", tc.leaks, got)
			}
		})
	}
}

// TestRedactStreamingByteAtATime is the core anti-chunking guarantee: a secret
// delivered one byte per Write must still be caught. A redactor that only
// works on whole buffers silently fails here, and in production the failure
// mode is a credential written to the run journal.
func TestRedactStreamingByteAtATime(t *testing.T) {
	t.Parallel()
	secrets := []struct {
		name   string
		secret string
		marker string
	}{
		{"anthropic", fxAnthropicKey, "[REDACTED:ANTHROPIC_API_KEY]"},
		{"sonar", fxSonarUserToken, "[REDACTED:SONAR_TOKEN]"},
		{"github", fxGitHubPAT, "[REDACTED:GITHUB_TOKEN]"},
		{"github_pat", fxGitHubFineGrained, "[REDACTED:GITHUB_TOKEN]"},
		{"aws", fxAWSLongTermKey, "[REDACTED:AWS_ACCESS_KEY_ID]"},
		{"jwt", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r", "[REDACTED:JWT]"},
		{"bearer", "Bearer abcdefghijklmnopqrstuvwx", "[REDACTED:BEARER_TOKEN]"},
		{"sonar_flag", "-Dsonar.token=notaknownshape123456", "[REDACTED:SONAR_TOKEN]"},
		{"literal_env_value", "totally-opaque-value-from-env-var", "[REDACTED:CUSTOM_TOKEN]"},
	}

	r := NewRedactor(Secret{Label: "CUSTOM_TOKEN", Value: "totally-opaque-value-from-env-var"})
	for _, tc := range secrets {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := filler + " prefix " + tc.secret + " suffix " + filler
			var sb strings.Builder
			w := r.Writer(&sb)
			for i := range len(in) {
				if _, err := w.Write([]byte(in[i : i+1])); err != nil {
					t.Fatalf("write byte %d: %v", i, err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			got := sb.String()

			leak := tc.secret
			if after, ok := strings.CutPrefix(leak, "Bearer "); ok {
				leak = after
			}
			if after, ok := strings.CutPrefix(leak, "-Dsonar.token="); ok {
				leak = after
			}
			if strings.Contains(got, leak) {
				t.Fatalf("secret %q survived byte-at-a-time streaming:\n%s", leak, got)
			}
			if !strings.Contains(got, tc.marker) {
				t.Fatalf("marker %q missing from byte-at-a-time output:\n%s", tc.marker, got)
			}
			if want := r.Redact(in); got != want {
				t.Errorf("streamed output differs from one-shot:\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestRedactStreamingEveryChunkBoundary splits the same input at every
// possible offset, which catches an overlap buffer that is off by one.
func TestRedactStreamingEveryChunkBoundary(t *testing.T) {
	t.Parallel()
	r := NewRedactor(Secret{Label: "SONAR_TOKEN", Value: "opaque-secret-value-1234"})
	in := filler + " token " + fxSonarUserToken + " " +
		"and opaque-secret-value-1234 and Bearer abcdefghijklmnopqrstuvwx " + filler
	want := r.Redact(in)
	if strings.Contains(want, "squ_9f2c") || strings.Contains(want, "opaque-secret-value-1234") {
		t.Fatalf("baseline one-shot redaction leaked: %q", want)
	}
	for split := range len(in) + 1 {
		var sb strings.Builder
		w := r.Writer(&sb)
		if _, err := w.Write([]byte(in[:split])); err != nil {
			t.Fatalf("split %d: first write: %v", split, err)
		}
		if _, err := w.Write([]byte(in[split:])); err != nil {
			t.Fatalf("split %d: second write: %v", split, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("split %d: close: %v", split, err)
		}
		if got := sb.String(); got != want {
			t.Fatalf("split at %d produced different output:\n got: %q\nwant: %q", split, got, want)
		}
	}
}

// TestRedactEmptySecretDoesNotCorruptOutput covers the false-negative trap: a
// redactor seeded from an unset environment variable must behave like one that
// was not seeded at all, not replace every byte boundary with a marker.
func TestRedactEmptySecretDoesNotCorruptOutput(t *testing.T) {
	t.Parallel()
	const clean = "ok  github.com/dhaam-ai/belay/internal/journal  0.118s"
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"single space", " "},
		{"newline only", "\n"},
		{"whitespace run", "   \t\n "},
		{"one char", "x"},
		{"below minimum", "abc"},
		{"padded below minimum", "  ab  "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRedactor(Secret{Label: "SONAR_TOKEN", Value: tc.value})
			if got := r.Redact(clean); got != clean {
				t.Errorf("output corrupted by secret %q:\n got: %q\nwant: %q", tc.value, got, clean)
			}
			if got := r.Redact(""); got != "" {
				t.Errorf("empty input became %q", got)
			}
			var sb strings.Builder
			w := r.Writer(&sb)
			for i := range len(clean) {
				if _, err := w.Write([]byte(clean[i : i+1])); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if got := sb.String(); got != clean {
				t.Errorf("streamed output corrupted:\n got: %q\nwant: %q", got, clean)
			}
		})
	}
}

// TestRedactMinimumLengthBoundary pins the accept/reject boundary so a future
// change to MinSecretLen cannot silently widen or narrow it.
func TestRedactMinimumLengthBoundary(t *testing.T) {
	t.Parallel()
	justUnder := strings.Repeat("z", MinSecretLen-1)
	justOver := strings.Repeat("z", MinSecretLen)
	r := NewRedactor(Secret{Label: "T", Value: justUnder})
	if got := r.Redact("a " + justUnder + " b"); !strings.Contains(got, justUnder) {
		t.Errorf("value shorter than MinSecretLen was honoured: %q", got)
	}
	r = NewRedactor(Secret{Label: "T", Value: justOver})
	if got := r.Redact("a " + justOver + " b"); got != "a [REDACTED:T] b" {
		t.Errorf("value at MinSecretLen not redacted: %q", got)
	}
}

func TestRedactPEMBlock(t *testing.T) {
	t.Parallel()
	raw := readTestdata(t, "leaked_key.txt")
	r := NewRedactor()

	got := r.Redact(raw)
	if strings.Contains(got, "NOTAREALKEY") {
		t.Errorf("private key body survived redaction:\n%s", got)
	}
	if strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("private key header survived redaction:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED:PRIVATE_KEY]") {
		t.Errorf("no private key marker emitted:\n%s", got)
	}
	if !strings.Contains(got, "resuming migration") {
		t.Errorf("text after the key block was lost:\n%s", got)
	}
	if !strings.Contains(got, "inspecting deploy credentials") {
		t.Errorf("text before the key block was lost:\n%s", got)
	}

	var sb strings.Builder
	w := r.Writer(&sb)
	for i := range len(raw) {
		if _, err := w.Write([]byte(raw[i : i+1])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if streamed := sb.String(); streamed != got {
		t.Errorf("byte-at-a-time PEM redaction differs:\n got: %q\nwant: %q", streamed, got)
	}
}

// TestRedactUnterminatedPEM covers a key block cut short by a timeout kill or
// the output cap. Everything after the header must still be suppressed.
func TestRedactUnterminatedPEM(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	in := "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAsecretbodyAAAA\nmore body\n"
	got := r.Redact(in)
	if strings.Contains(got, "secretbody") {
		t.Errorf("unterminated key body survived: %q", got)
	}
	if !strings.HasPrefix(got, "before\n[REDACTED:PRIVATE_KEY]") {
		t.Errorf("unexpected prefix: %q", got)
	}
}

func TestRedactScannerOutputFixture(t *testing.T) {
	t.Parallel()
	raw := readTestdata(t, "scanner_output.txt")
	r := NewRedactor(Secret{Label: "SONAR_HOST", Value: "sonar.example.com"})
	got := r.Redact(raw)
	for _, leak := range []string{
		fxSonarUserToken,
		fxAnthropicKey,
		fxAWSLongTermKey,
		fxGitHubPAT,
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"sonar.example.com",
	} {
		if strings.Contains(got, leak) {
			t.Errorf("secret %q survived:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "EXECUTION SUCCESS") {
		t.Errorf("useful scanner output was lost:\n%s", got)
	}
}

func TestRedactLabelNormalisation(t *testing.T) {
	t.Parallel()
	tests := []struct{ label, want string }{
		{"SONAR_TOKEN", "[REDACTED:SONAR_TOKEN]"},
		{"my-token", "[REDACTED:MY_TOKEN]"},
		{"", "[REDACTED:SECRET]"},
		{"weird name!", "[REDACTED:WEIRD_NAME_]"},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			r := NewRedactor(Secret{Label: tc.label, Value: "opaque-secret-value"})
			if got := r.Redact("x opaque-secret-value y"); got != "x "+tc.want+" y" {
				t.Errorf("got %q want %q", got, "x "+tc.want+" y")
			}
		})
	}
}

// TestRedactLongestSecretWins keeps a secret that contains another secret from
// being reported under the shorter one's label, and proves overlapping matches
// collapse to a single marker.
func TestRedactLongestSecretWins(t *testing.T) {
	t.Parallel()
	r := NewRedactor(
		Secret{Label: "SHORT", Value: "abcd1234"},
		Secret{Label: "LONG", Value: "abcd1234efgh5678"},
	)
	if got := r.Redact("v=abcd1234efgh5678;"); got != "v=[REDACTED:LONG];" {
		t.Errorf("got %q", got)
	}
	if got := r.Redact("v=abcd1234;"); got != "v=[REDACTED:SHORT];" {
		t.Errorf("got %q", got)
	}
}

// TestRedactWriterBoundedMemory proves the hold-back buffer cannot grow
// without limit on input that never offers a safe cut point.
func TestRedactWriterBoundedMemory(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	var sb strings.Builder
	w := r.Writer(&sb)
	chunk := []byte(strings.Repeat("x", 4096))
	for range 200 { // 800 KiB with no whitespace at all
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
		if len(w.buf) > maxPending+len(chunk) {
			t.Fatalf("hold-back buffer grew to %d bytes", len(w.buf))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := sb.Len(); got != 200*4096 {
		t.Errorf("streamed %d bytes, want %d", got, 200*4096)
	}
}

func TestRedactorNilSafeOnEmptyInput(t *testing.T) {
	t.Parallel()
	if got := NewRedactor().Redact(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	//nolint:gosec // name is a literal chosen by the test.
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

// No file in this package may contain a contiguous secret-shaped literal.
//
// This package exists to scrub credentials, so its fixtures necessarily look
// like credentials -- and a public repository runs a secret scanner over every
// commit. GitHub opened alerts on two of these before they were split. None
// was a real credential, but an alert a maintainer dismisses on every push
// teaches everyone to wave through the one that is real.
//
// The fixtures are therefore assembled from parts at run time. This test walks
// the package's own source and fails if a whole one reappears, because the
// convention is otherwise only as durable as the next contributor's memory.
func TestSourceHasNoContiguousSecretLiterals(t *testing.T) {
	t.Parallel()

	patterns := map[string]*regexp.Regexp{
		"anthropic key":  regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`),
		"generic sk key": regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`),
		"sonar token":    regexp.MustCompile(`\b(?:squ_|sqp_|sqa_)[A-Za-z0-9]{20,}`),
		"github token":   regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
		"github pat":     regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		"aws key id":     regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		scanned++
		for label, re := range patterns {
			if m := re.Find(body); m != nil {
				t.Errorf("%s: contains a contiguous %s literal (%q). Split it the way the "+
					"fixtures at the top of redact_test.go are split, or a secret scanner "+
					"will open an alert on every push.", e.Name(), label, string(m))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; this guard has stopped guarding")
	}
	t.Logf("scanned %d files for contiguous secret literals", scanned)
}
