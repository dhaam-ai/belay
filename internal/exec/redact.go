//go:build unix

package exec

import (
	"bytes"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// MinSecretLen is the shortest literal secret value a Redactor honours.
//
// Seeding a redactor with a short value is worse than not seeding it at all.
// The degenerate case is an unset environment variable: os.Getenv returns "",
// and a naive redactor built from that value would match at every byte offset
// and replace the entire captured stream with markers. belay feeds captured
// output back into model prompts and writes it to the run journal, so a
// redactor that destroys output is a correctness bug, not a safe default.
// Values shorter than MinSecretLen after whitespace trimming are dropped.
//
// Pattern-based redaction is unaffected: a real credential that happens to be
// passed through a short variable is still caught by the built-in patterns.
const MinSecretLen = 4

// minOverlap is the number of trailing bytes a RedactWriter always withholds.
//
// It must exceed the longest fixed marker that redaction keys off, which is a
// PEM header such as "-----BEGIN ENCRYPTED PRIVATE KEY-----" (37 bytes). 128
// leaves headroom for longer key types without meaningfully delaying output.
const minOverlap = 128

// maxPending bounds the RedactWriter hold-back buffer.
//
// The writer normally withholds any trailing bytes that could still turn out
// to be part of a secret. Input that never presents a safe cut point (a
// multi-megabyte run with no whitespace) would otherwise grow that buffer
// without bound, so past this size the writer flushes on the fixed overlap
// alone and accepts the small chance of splitting an implausibly long token.
const maxPending = 256 << 10

// pemTailKeep is how many bytes of a suppressed PEM body are retained while
// scanning for the END marker, so a marker split across writes is still found.
const pemTailKeep = 64

// holdTokens is the number of trailing whitespace-delimited runs a
// RedactWriter withholds.
//
// Credentials never contain whitespace, so withholding the final incomplete
// run guarantees a token is evaluated whole no matter how the stream was
// chunked. Two runs are withheld rather than one because the highest-value
// patterns are two tokens wide ("Bearer <token>").
const holdTokens = 2

// Secret is a literal value that must never survive into captured output.
//
// Label names the value in the replacement marker, so an operator reading a
// redacted log learns which credential leaked without learning the credential.
// Label is normalised to [A-Z0-9_].
type Secret struct {
	// Label identifies the secret in the "[REDACTED:LABEL]" marker.
	Label string
	// Value is the literal secret. Values shorter than MinSecretLen are ignored.
	Value string
}

// pattern is one credential shape.
//
// group names the submatch replaced by the marker, so a pattern can keep the
// context it matched ("Bearer ", "-Dsonar.token=") and redact only the
// credential.
//
// gates are lowercase literals, at least one of which every possible match
// must contain. They are checked with a case-insensitive substring scan before
// the regexp runs. This is not a micro-optimisation: the bounded repetitions
// these patterns need ({20,} and up) compile to hundreds of NFA states, and an
// unanchored search restarts at every byte, so running all of them over every
// captured byte costs roughly 0.3 MB/s. Captured output is measured in
// megabytes per node, and belay must not spend minutes of CPU redacting a
// verbose test run. A gate is a necessary condition for a match, never a
// sufficient one, so gating changes throughput and not results.
type pattern struct {
	label string
	re    *regexp.Regexp
	group int
	gates []string
}

// patterns are high-confidence credential shapes. Order matters only for the
// label chosen when two patterns overlap: the more specific one comes first.
var patterns = []pattern{
	{"ANTHROPIC_API_KEY", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`), 0,
		[]string{"sk-ant-"}},
	{"API_KEY", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`), 0,
		[]string{"sk-"}},
	{"SONAR_TOKEN", regexp.MustCompile(`\b(?:squ_|sqp_|sqa_)[A-Za-z0-9]{20,}`), 0,
		[]string{"squ_", "sqp_", "sqa_"}},
	{"GITHUB_TOKEN", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), 0,
		[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}},
	{"GITHUB_TOKEN", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), 0,
		[]string{"github_pat_"}},
	{"AWS_ACCESS_KEY_ID", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0,
		[]string{"akia", "asia"}},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), 0,
		[]string{"eyj"}},
	{"BEARER_TOKEN", regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/-]{8,}={0,2})`), 2,
		[]string{"bearer"}},
	{"SONAR_TOKEN", regexp.MustCompile(`(?i)((?:-D)?sonar\.(?:token|login|password)\s*=\s*)([^\s"']+)`), 2,
		[]string{"sonar."}},
	{"TOKEN", regexp.MustCompile(`(?i)(--?(?:token|api[-_]?key|apikey|password|passwd|secret|auth[-_]?token)\s*=\s*)([^\s"']+)`), 2,
		[]string{"token", "key", "password", "passwd", "secret", "auth"}},
}

// open reports whether b could contain a match for p.
func (p pattern) open(b []byte) bool {
	for _, g := range p.gates {
		if containsFold(b, g) {
			return true
		}
	}
	return false
}

// containsFold reports whether b contains lit, ignoring ASCII case. lit must
// already be lowercase.
func containsFold(b []byte, lit string) bool {
	n := len(lit)
	if n == 0 {
		return true
	}
	if len(b) < n {
		return false
	}
	lower := lit[0]
	upper := lower
	if lower >= 'a' && lower <= 'z' {
		upper = lower - ('a' - 'A')
	}
	for _, first := range [2]byte{lower, upper} {
		for i := 0; i+n <= len(b); {
			j := bytes.IndexByte(b[i:len(b)-n+1], first)
			if j < 0 {
				break
			}
			i += j
			if equalFoldASCII(b[i:i+n], lit) {
				return true
			}
			i++
		}
		if upper == lower {
			break
		}
	}
	return false
}

func equalFoldASCII(b []byte, lit string) bool {
	for i := 0; i < len(lit); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lit[i] {
			return false
		}
	}
	return true
}

var (
	pemBeginRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEndRe   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// Redactor removes credentials from bytes that belay captured from a child
// process before those bytes reach the journal, a log line, or a model prompt.
//
// A Redactor is immutable after construction and safe for concurrent use.
type Redactor struct {
	secrets []Secret
	overlap int
}

// NewRedactor builds a Redactor seeded with literal secret values in addition
// to the always-on credential patterns.
//
// Values are whitespace-trimmed, de-duplicated, and dropped when shorter than
// MinSecretLen. Passing no secrets is valid and yields a pattern-only
// redactor.
func NewRedactor(secrets ...Secret) *Redactor {
	r := &Redactor{overlap: minOverlap}
	seen := make(map[string]bool, len(secrets))
	for _, s := range secrets {
		v := strings.TrimSpace(s.Value)
		if len(v) < MinSecretLen || seen[v] {
			continue
		}
		seen[v] = true
		r.secrets = append(r.secrets, Secret{Label: sanitizeLabel(s.Label), Value: v})
	}
	// Longest first so that a secret containing another secret as a prefix
	// wins the overlap merge and produces the more specific label.
	slices.SortFunc(r.secrets, func(a, b Secret) int { return len(b.Value) - len(a.Value) })
	if len(r.secrets) > 0 {
		if n := len(r.secrets[0].Value) + 1; n > r.overlap {
			r.overlap = n
		}
	}
	return r
}

// Redact returns s with every known secret and credential pattern replaced by
// a marker.
//
// Redact runs the same state machine as Writer, so a string and a stream of
// the same bytes always redact identically.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return ""
	}
	var sb strings.Builder
	w := r.Writer(&sb)
	if _, err := io.WriteString(w, s); err != nil {
		return ""
	}
	if err := w.Close(); err != nil {
		return ""
	}
	return sb.String()
}

// RedactWriter is a streaming redactor.
//
// It withholds the trailing bytes of the stream that could still turn out to
// be part of a credential, so a secret split across any number of Write calls
// (down to one byte per call) is still caught. Close flushes the remainder and
// must be called once the child process has exited.
//
// RedactWriter is safe for concurrent use; writes are serialised.
type RedactWriter struct {
	r   *Redactor
	dst io.Writer

	mu    sync.Mutex
	buf   []byte
	inPEM bool
}

// Writer returns a RedactWriter that forwards redacted bytes to dst.
func (r *Redactor) Writer(dst io.Writer) *RedactWriter {
	return &RedactWriter{r: r, dst: dst}
}

// Write consumes p, forwarding to the destination every byte that can no
// longer be part of an unfinished credential. It always reports len(p)
// consumed unless the destination fails.
func (w *RedactWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if err := w.process(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes the withheld tail. It is safe to call more than once.
func (w *RedactWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.process(true)
}

func (w *RedactWriter) process(final bool) error {
	if !final && len(w.buf) < minOverlap {
		return nil
	}
	for {
		if w.inPEM {
			if loc := pemEndRe.FindIndex(w.buf); loc != nil {
				w.dropFront(loc[1])
				w.inPEM = false
				continue
			}
			// Body bytes are unconditionally discarded; keep only enough
			// tail to recognise an END marker split across writes.
			if len(w.buf) > pemTailKeep {
				w.dropFront(len(w.buf) - pemTailKeep)
			}
			if final {
				w.buf = w.buf[:0]
			}
			return nil
		}
		loc := pemBeginRe.FindIndex(w.buf)
		if loc == nil {
			break
		}
		if err := w.emit(w.r.redactBytes(w.buf[:loc[0]])); err != nil {
			return err
		}
		if err := w.emit([]byte(marker("PRIVATE_KEY"))); err != nil {
			return err
		}
		w.dropFront(loc[1])
		w.inPEM = true
	}

	var (
		found    []span
		computed bool
	)
	cut := len(w.buf)
	if !final {
		cut -= w.r.overlap
		// Skipped when nothing can be flushed anyway, which keeps a
		// whitespace-free stream (a base64 blob, a minified bundle) from
		// costing a full rescan of the hold-back buffer per write.
		if cut > 0 && len(w.buf) < maxPending {
			if h := tailHoldStart(w.buf, holdTokens); h < cut {
				cut = h
			}
			if cut > 0 {
				found, computed = w.r.spans(w.buf), true
				cut = retreatPastSpans(cut, found)
			}
		}
	}
	if cut <= 0 {
		return nil
	}
	out := w.buf[:cut]
	if computed {
		out = applySpans(out, found)
	} else {
		out = w.r.redactBytes(out)
	}
	if err := w.emit(out); err != nil {
		return err
	}
	w.dropFront(cut)
	return nil
}

// retreatPastSpans lowers cut until no credential straddles it.
//
// A credential that is already complete in the buffer must be replaced whole.
// Flushing its first half and withholding the second would put cleartext on
// the far side of the writer, and the withheld half would no longer match
// anything on the next pass. Iterating to a fixed point matters because a
// pattern's match extends past the range it redacts, so lowering the cut for
// one span can expose another.
func retreatPastSpans(cut int, found []span) int {
	for cut > 0 {
		lowered := cut
		for _, s := range found {
			if s.gstart < lowered && s.gend > lowered {
				lowered = s.gstart
			}
		}
		if lowered == cut {
			return cut
		}
		cut = lowered
	}
	return cut
}

func (w *RedactWriter) emit(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := w.dst.Write(b)
	return err
}

func (w *RedactWriter) dropFront(n int) {
	if n >= len(w.buf) {
		w.buf = w.buf[:0]
		return
	}
	w.buf = w.buf[:copy(w.buf, w.buf[n:])]
}

// span is one credential found in a buffer.
//
// start:end is the range replaced by a marker. gstart:gend is the full match
// that produced it, which is wider whenever a pattern keeps context it does not
// redact ("Bearer " before the token, "-Dsonar.token=" before the value). The
// wider range is what a streaming flush must not cut through: emitting the
// context and withholding the credential would leave the credential
// unrecognisable on the next pass, and it would be forwarded in clear.
type span struct {
	start, end   int
	gstart, gend int
	label        string
}

func (r *Redactor) spans(b []byte) []span {
	var out []span
	for _, s := range r.secrets {
		v := []byte(s.Value)
		for off := 0; off <= len(b)-len(v); {
			i := bytes.Index(b[off:], v)
			if i < 0 {
				break
			}
			st, en := off+i, off+i+len(v)
			out = append(out, span{st, en, st, en, s.Label})
			off = en
		}
	}
	for _, p := range patterns {
		if !p.open(b) {
			continue
		}
		for _, m := range p.re.FindAllSubmatchIndex(b, -1) {
			g := 2 * p.group
			if g+1 >= len(m) || m[g] < 0 {
				continue
			}
			out = append(out, span{m[g], m[g+1], m[0], m[1], p.label})
		}
	}
	if len(out) < 2 {
		return out
	}
	slices.SortFunc(out, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	merged := out[:1]
	for _, s := range out[1:] {
		last := &merged[len(merged)-1]
		if s.start < last.end {
			last.end = max(last.end, s.end)
			last.gstart = min(last.gstart, s.gstart)
			last.gend = max(last.gend, s.gend)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

func (r *Redactor) redactBytes(b []byte) []byte {
	return applySpans(b, r.spans(b))
}

// applySpans replaces each span of b with its marker. Spans are sorted and
// non-overlapping. A span that runs past the end of b can only happen if a
// caller cut the buffer without retreating first; it is redacted to the end
// rather than half-emitted, so the failure mode is lost output and never a
// leaked credential.
func applySpans(b []byte, found []span) []byte {
	if len(found) == 0 {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b))
	prev := 0
	for _, s := range found {
		if s.start >= len(b) {
			break
		}
		out.Write(b[prev:s.start])
		out.WriteString(marker(s.label))
		if s.end > len(b) {
			return out.Bytes()
		}
		prev = s.end
	}
	out.Write(b[prev:])
	return out.Bytes()
}

// tailHoldStart returns the offset of the first of the final runs
// whitespace-delimited runs of b, or 0 if b has fewer runs than that.
//
// Everything from that offset onward may still grow into a credential once
// more bytes arrive, so it must not be forwarded yet.
func tailHoldStart(b []byte, runs int) int {
	i := len(b)
	for range runs {
		for i > 0 && !isSpaceByte(b[i-1]) {
			i--
		}
		if i == 0 {
			return 0
		}
		for i > 0 && isSpaceByte(b[i-1]) {
			i--
		}
		if i == 0 {
			return 0
		}
	}
	return i
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

func marker(label string) string {
	return "[REDACTED:" + label + "]"
}

func sanitizeLabel(s string) string {
	if s == "" {
		return "SECRET"
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c-('a'-'A'))
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
