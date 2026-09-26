package policy

import (
	"slices"

	"github.com/dhaam-ai/belay/internal/config"
)

// From returns the rule set to pin in a run's manifest for the policy section
// of cfg.
//
// The two types are deliberately separate. config.Policy is the YAML a person
// writes and may change at any time; Policy is what one run was started under
// and is written into manifest.json, where it is read back by every resume
// and by every later belay. Keeping them apart is the same choice
// state.OutputMode makes against internal/config's own output mode: the file
// is a question, the manifest is the answer, and an answer that changed shape
// because somebody reorganised the configuration schema would make old runs
// unreadable.
//
// Every list is copied. A pin that aliased the caller's configuration would
// be a run's rules that something else could still edit.
func From(cfg config.Policy) Policy {
	p := Policy{
		ForbiddenPaths:    slices.Clone(cfg.ForbiddenPaths),
		MaxChangedFiles:   cfg.MaxChangedFiles,
		MaxChangedLines:   cfg.MaxChangedLines,
		NoNewDependencies: cfg.NoNewDependencies,
	}
	for _, rule := range cfg.RequiredTests {
		p.RequiredTests = append(p.RequiredTests, TestRule{When: rule.When, Require: rule.Require})
	}
	for _, rule := range cfg.ForbiddenContent {
		p.ForbiddenContent = append(p.ForbiddenContent, ContentRule{
			Pattern: rule.Pattern,
			Paths:   slices.Clone(rule.Paths),
			Reason:  rule.Reason,
		})
	}
	return p
}
