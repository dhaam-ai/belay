package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
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

// documentedTypes maps the words docs/config.md uses in its "**Type**:" lines
// onto the Go kinds that satisfy them.
var documentedTypes = map[string][]reflect.Kind{
	"Boolean":           {reflect.Bool},
	"Integer":           {reflect.Int, reflect.Int64},
	"Float":             {reflect.Float64},
	"Number":            {reflect.Float64},
	"String":            {reflect.String},
	"String (duration)": {reflect.Struct}, // config.Duration
}

// yamlFields walks a struct and returns every leaf field by its dotted yaml
// path, so a doc heading like `graph.give_up` can be resolved to a real type.
func yamlFields(t reflect.Type, prefix string, out map[string]reflect.Type) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		// Duration is a leaf even though it is a struct: it is documented as
		// the string a user writes, not as its internals.
		if f.Type.Kind() == reflect.Struct && f.Type.Name() != "Duration" {
			yamlFields(f.Type, path, out)
			continue
		}
		out[path] = f.Type
	}
}

// The type a field is documented as must be the type it actually is.
//
// TestDocumentedExamplesActuallyLoad only validates whole YAML examples, so a
// field the examples happen not to exercise can be documented with the wrong
// type indefinitely. That is not hypothetical: graph.give_up was documented
// as a Boolean and graph.approval as a String, when give_up is an int cap and
// approval is a bool -- the two had been swapped, most likely by confusing
// config.Graph.GiveUp (how many fix attempts) with state.Fix.GiveUp (whether
// the loop gave up). A reader following either would have written a config
// the loader rejects.
func TestDocumentedFieldTypesMatchTheStruct(t *testing.T) {
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Skipf("docs/config.md unavailable: %v", err)
	}

	actual := map[string]reflect.Type{}
	yamlFields(reflect.TypeOf(config.Config{}), "", actual)

	var heading string
	checked := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if after, ok := strings.CutPrefix(line, "### `"); ok {
			// A heading may carry a trailing qualifier after the closing
			// backtick; the field path is what precedes it.
			heading, _, _ = strings.Cut(after, "`")
			continue
		}
		after, ok := strings.CutPrefix(line, "**Type**: ")
		if !ok || heading == "" {
			continue
		}
		documented := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(after), "  "))
		field, known := actual[heading]
		name := heading
		heading = ""
		if !known {
			// Section headings that are not leaf fields (a whole block, or a
			// conditional) are not type-checked here.
			continue
		}
		checked++
		want, recognised := documentedTypes[documented]
		if !recognised {
			t.Errorf("%s: documented type %q is not one this test knows; add it to documentedTypes", name, documented)
			continue
		}
		if !slices.Contains(want, field.Kind()) {
			t.Errorf("%s is documented as %q but is a %s in config.Config; a reader following the docs writes a config the loader rejects",
				name, documented, field.Kind())
		}
	}

	if checked < 15 {
		t.Fatalf("only type-checked %d fields; the heading or **Type** format changed and this guard has stopped guarding", checked)
	}
	t.Logf("type-checked %d documented fields against config.Config", checked)
}
