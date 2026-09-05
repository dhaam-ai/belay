package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belay-dev/belay/internal/config"
)

// docsPath is docs/config.md relative to this package.
const docsPath = "../../docs/config.md"

// yamlBlocks returns every fenced ```yaml block in src, with the 1-based line
// number its fence opened on, so a failure names a place a human can go.
func yamlBlocks(src string) []struct {
	Line int
	Body string
} {
	var out []struct {
		Line int
		Body string
	}
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```yaml" {
			continue
		}
		start := i + 1
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			body = append(body, lines[i])
		}
		out = append(out, struct {
			Line int
			Body string
		}{start, strings.Join(body, "\n")})
	}
	return out
}

// Documentation that does not load is worse than no documentation: a reader
// copies it, belay rejects it, and the reader concludes the tool is broken.
// This walks every YAML example in docs/config.md through the real loader and
// validator, so the docs cannot drift from the schema without failing here.
func TestDocumentedExamplesActuallyLoad(t *testing.T) {
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Skipf("docs/config.md unavailable: %v", err)
	}
	blocks := yamlBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatal("no ```yaml blocks found in docs/config.md; this guard would silently pass forever")
	}
	for _, b := range blocks {
		if !strings.Contains(b.Body, "version:") {
			continue // a fragment illustrating one section, not a whole file
		}
		t.Run("line_"+itoa(b.Line), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "belay.yaml")
			if err := os.WriteFile(path, []byte(b.Body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := config.Load(config.Options{Path: path})
			if err != nil {
				t.Fatalf("docs/config.md:%d does not load:\n%v\n\n--- block ---\n%s", b.Line, err, b.Body)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("docs/config.md:%d loads but fails validation:\n%v", b.Line, err)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
