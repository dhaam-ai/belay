//go:build unix

package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	bexec "github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// CustomName is the [belay.TestRunner.Name] of the custom runner.
const CustomName = "custom"

// customFailureName is the single failure a non-zero custom command produces.
// It is not [BuildFailureName]: nothing here can tell a compile error from an
// assertion, and claiming otherwise would send the fix node down the wrong
// branch.
const customFailureName = "custom-command"

// Custom runs an operator-supplied command and reports pass or fail from its
// exit status.
//
// # What it can and cannot tell you
//
// Exit 0 is a pass; anything else is a failure. That is the whole protocol,
// and it is the entire reason to reach for this runner: it works with make
// targets, shell-free wrapper binaries, cargo, gradle, ctest, anything.
//
// The cost is that the report carries no per-test detail. A failing run
// produces Total 1, Failed 1 and exactly one [belay.TestFailure] whose Message
// is the tail of the command's output, with File empty and Line zero. There is
// no way to recover more without knowing the tool, which is precisely what the
// operator opted out of telling belay. A passing run reports Total 1, Passed 1
// for the same reason — [belay.TestReport.OK] requires Total > 0, so a report
// claiming zero tests would mark a green custom command as needing a human.
// The counts describe the command, not the tests inside it.
//
// If the fix loop needs real failure detail, configure a language runner
// instead.
//
// # Environment
//
// The custom command gets exec's base environment (HOME, LANG, PATH, TERM,
// TMPDIR) and nothing else, because belay cannot know which variables an
// arbitrary command needs and guessing would erode the deny-by-default control
// that exists to keep credentials away from the processes a run starts. A
// command that needs more should read it from a file, or be a script that sets
// what it needs.
//
// # No shell
//
// The command string is turned into argv by [exec.ParseCommandLine], which
// removes quoting and nothing else. `make test && echo done` runs `make` with
// the arguments "test", "&&", "echo", "done" — it does not run two processes.
// Pipelines, redirection, globbing and substitution are not available; put
// them in a script and run the script.
type Custom struct {
	base
	cmdline string
}

// NewCustom returns a Custom runner that runs cmdline, which is the
// test.custom_cmd configuration value.
//
// A nil Execer uses a real *exec.Runner; a nil logger uses slog.Default.
func NewCustom(cmdline string, x Execer, logger *slog.Logger) *Custom {
	return &Custom{base: newBase(x, logger), cmdline: cmdline}
}

// Name implements [belay.TestRunner].
func (c *Custom) Name() string { return CustomName }

// Detect implements [belay.TestRunner]. It reports whether a command was
// configured, since a custom command is by definition not inferable from the
// contents of dir. dir is ignored.
func (c *Custom) Detect(_ string) bool {
	return strings.TrimSpace(c.cmdline) != ""
}

// Test runs the configured command in dir and reports pass or fail from its
// exit status.
//
// A missing program surfaces as an error satisfying
// errors.Is(err, [belay.ErrToolchainMissing]) naming the program from the
// command string. An unparseable command string, or an empty one, is a
// [*RunError] wrapping [ErrNoCommand]. A command that runs and exits non-zero
// is not an error at all — that is the failure report described on [Custom].
func (c *Custom) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	if strings.TrimSpace(c.cmdline) == "" {
		return belay.TestReport{}, &RunError{Runner: CustomName, ExitCode: -1, Err: ErrNoCommand}
	}
	cmd, err := bexec.ParseCommandLine(c.cmdline)
	if err != nil {
		return belay.TestReport{}, &RunError{
			Runner:   CustomName,
			ExitCode: -1,
			Err:      fmt.Errorf("parse test.custom_cmd %q: %w", c.cmdline, err),
		}
	}
	cmd.Dir = dir

	res, err := c.run(ctx, CustomName, cmd.Path, cmd)
	if err != nil {
		return belay.TestReport{}, err
	}

	report := exitStatusReport(strconv.Quote(c.cmdline), customFailureName, res)
	report.Raw = rawString(res.Stdout)
	return report, nil
}
