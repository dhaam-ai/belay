//go:build unix

package lintgate

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Concurrent quality gates: what happens when several candidates of one
// fanout reach the review node at the same time.
//
// With fanout.concurrency above 1, belay runs N candidate runs at once, each
// with its own workspace and its own review node, so N golangci-lint
// processes can be in flight together. That is the default, not an edge case:
// fanout.concurrency defaults to 3.
//
// # What the pinned tool does, established rather than assumed
//
// golangci-lint takes one file lock for the whole machine when it starts.
// `golangci-lint run --help` (v2.12.2, the pinned version) says so:
//
//	--allow-parallel-runners   Allow multiple parallel golangci-lint instances running.
//	                           If false (default) - golangci-lint acquires file lock on start.
//	--allow-serial-runners     Allow multiple golangci-lint instances running, but serialize them around a lock.
//	                           If false (default) - golangci-lint exits with an error if it fails to acquire file lock on start.
//
// Measured on that version, the default is not "error immediately": a loser
// waits for the lock for about five seconds and only then gives up with exit
// 3 and "The command is terminated due to an error: parallel golangci-lint is
// running". With this file's own lock held for 2s, a gate got through after
// 2.6s; held for 5s, 10s and 20s, it failed after 5.2s every time. Three
// gates at once over tiny modules all passed, finishing about a second apart
// -- they queued inside the window. Five at once did not, three runs out of
// three: the last in the queue outwaited it. So the default is not safe, and
// it is not reliably broken either, which is worse: whether a fanout survives
// depends on how long a lint takes against how many are queued behind it.
//
// The lock is global. Five instances in five separate module directories
// collided, and giving each its own GOLANGCI_LINT_CACHE did not help, so it
// is not in the cache directory: it is $TMPDIR/golangci-lint.lock.
//
// # Why --allow-serial-runners and not --allow-parallel-runners
//
// Serialising costs wall-clock time inside one candidate's own envelope and
// nothing else. --allow-parallel-runners would let the instances run at once
// over one cache that golangci-lint keys by file content while storing
// absolute paths (golangci/golangci-lint#3502) -- so a candidate could be
// handed a sibling's findings, with paths into a directory it never touched.
// internal/linter's own doc already records that contamination as a limit of
// scoping; inviting it on purpose to save a few seconds would be a bad trade
// in a gate that decides whether a run's change is acceptable.
//
// Neither test here can use a stub runner: what is under test is a real
// process taking a real lock.

// lintLockName is where golangci-lint v2.12.2 puts the lock, relative to the
// temporary directory the process is given. TestAGateWhoseLockIsHeldStillRuns
// asserts it is really there before relying on it, so that a future version
// moving the lock fails that test loudly instead of turning it into a check
// of nothing.
const lintLockName = "golangci-lint.lock"

// lintLockHold is how long TestAGateWhoseLockIsHeldStillRuns keeps the lock.
// It has to be comfortably longer than the window the default gives a loser
// -- about five seconds on the pinned version -- so that a gate without
// --allow-serial-runners fails rather than getting lucky. Eight is that
// window with half again to spare, and no more, because every second of it
// is a second this suite takes.
const lintLockHold = 8 * time.Second

// concurrentGates is how many gates TestConcurrentGolangCIGatesAllSucceed
// runs at once.
//
// Five rather than fanout.concurrency's default of three, because three
// passes on the pinned version without the flag: the losers wait inside the
// five-second window and win the lock. A test that passes with the bug
// present guards nothing, so this is a count measured to break it -- five
// failed three runs out of three, and four was not tried. fanout.candidates
// has no upper bound, so five is a configuration a person can ask for.
const concurrentGates = 5

// TestAGateWhoseLockIsHeldStillRuns is the deterministic half: it holds
// golangci-lint's lock itself, then requires the gate to produce a report
// anyway.
//
// It does not race anything. The lock is held for a fixed time by this
// process, which is the one condition a candidate's review node has to
// survive -- some other golangci-lint is running, and this one must wait
// rather than fail -- with none of the timing luck a real race brings.
//
// TMPDIR is a directory of this test's own, so the lock it takes is its own
// file: it can neither be disturbed by a golangci-lint running elsewhere on
// the machine nor disturb one.
func TestAGateWhoseLockIsHeldStillRuns(t *testing.T) {
	goEnv := requireTools(t)
	for name, value := range goEnv {
		t.Setenv(name, value)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("GOLANGCI_LINT_CACHE", t.TempDir())
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	writeModule(t, dir, map[string]string{"debt.go": scopeDebt})
	commitAll(t, dir)
	writeFiles(t, dir, map[string]string{"added.go": scopeAdded})

	// One plain run first, watched, which proves the lock is where this
	// test thinks it is. golangci-lint removes the file when it finishes,
	// so the only way to see it is to look while a run is in flight. A
	// version that moved its lock never shows up here, and the failure
	// names the assumption rather than leaving the rest of this test
	// quietly proving nothing.
	lock := filepath.Join(tmp, lintLockName)
	warm := make(chan belay.QualityReport, 1)
	go func() { warm <- lintIn(t, dir) }()
	seen := false
	for deadline := time.Now().Add(lintLockHold); !seen && time.Now().Before(deadline); {
		if _, err := os.Stat(lock); err == nil {
			seen = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if report := <-warm; report.Gate != belay.GateFail {
		t.Fatalf("warm-up run: Gate = %s, want %s (%s)", report.Gate, belay.GateFail, report.Summary)
	}
	if !seen {
		t.Fatalf("golangci-lint never created %s while it was running; it has moved its lock, "+
			"and this test no longer proves that a gate waits for one", lock)
	}

	// Take the lock and hold it. gofrs/flock, which golangci-lint uses, is
	// flock(2) underneath, so this is the same lock the tool competes for.
	// The file is gone again by now -- the tool removes it -- so this
	// recreates it at the path the tool will reopen.
	f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- a path under this test's own TMPDIR.
	if err != nil {
		t.Fatalf("open %s: %v", lock, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close %s: %v", lock, err)
		}
	}()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock %s: %v", lock, err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(lintLockHold)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Errorf("unlock %s: %v", lock, err)
		}
		close(released)
	}()

	var logs bytes.Buffer
	gate := linter.NewGolangCI(linter.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	began := time.Now()
	report, err := gate.Lint(t.Context(), dir)
	waited := time.Since(began)
	<-released
	if err != nil {
		t.Fatalf("a gate whose lock was held could not run after %v: %v\nlogs:\n%s", waited, err, logs.String())
	}
	// A gate that came back well before the lock was released never
	// competed for it, so whatever it proved, it was not this. The second
	// of slack is the time between taking the lock and starting the gate.
	if waited < lintLockHold-time.Second {
		t.Fatalf("the gate returned after %v, but the lock was held for %v: it never waited "+
			"for the lock, so this test no longer proves anything about one", waited, lintLockHold)
	}
	if report.Gate != belay.GateFail {
		t.Errorf("Gate = %s, want %s (%s)", report.Gate, belay.GateFail, report.Summary)
	}
	if want := []string{"added.go:7 errcheck major"}; !equalStrings(findings(report.Issues), want) {
		t.Errorf("findings = %v, want %v", findings(report.Issues), want)
	}
}

// TestConcurrentGolangCIGatesAllSucceed is the realistic half: concurrentGates
// real gates at once over separate modules sharing one cache, every one of
// which must come back with a report.
//
// An error from any instance is what this exists to catch, and it is worse
// than it looks. internal/linter turns a golangci-lint that could not run into
// an error rather than a failed gate, so the candidate's review node errors,
// the candidate ends with Outcome "failed", and join reports that N attempts
// at the goal failed -- for a reason that has nothing to do with any of their
// code.
func TestConcurrentGolangCIGatesAllSucceed(t *testing.T) {
	goEnv := requireTools(t)
	for name, value := range goEnv {
		t.Setenv(name, value)
	}
	// TMPDIR of its own, so these instances compete only with each other
	// and not with a golangci-lint somebody is running on this machine.
	t.Setenv("TMPDIR", t.TempDir())
	// One cache for all of them, deliberately: this is the shared-cache
	// case, and it is what a real machine has.
	t.Setenv("GOLANGCI_LINT_CACHE", t.TempDir())
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("HOME", t.TempDir())

	// Separate modules, as separate as two fanout candidates are: each is
	// its own directory, its own repository and its own commit.
	dirs := make([]string, concurrentGates)
	for i := range dirs {
		dirs[i] = t.TempDir()
		writeModule(t, dirs[i], map[string]string{"debt.go": scopeDebt})
		commitAll(t, dirs[i])
		writeFiles(t, dirs[i], map[string]string{"added.go": scopeAdded})
	}

	type result struct {
		report belay.QualityReport
		logs   string
		err    error
	}
	results := make([]result, len(dirs))

	var wg sync.WaitGroup
	// A barrier, so the instances start together rather than merely soon
	// after one another: the failure is a race for one lock, and staggered
	// starts can miss it.
	start := make(chan struct{})
	for i, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var logs bytes.Buffer
			gate := linter.NewGolangCI(linter.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
			<-start
			report, err := gate.Lint(t.Context(), dir)
			results[i] = result{report: report, logs: logs.String(), err: err}
		}()
	}
	close(start)
	wg.Wait()

	for i, got := range results {
		if got.err != nil {
			t.Errorf("gate %d of %d could not run: %v\nlogs:\n%s",
				i+1, len(dirs), got.err, got.logs)
			continue
		}
		// Every module has one new finding, so every gate must fail on the
		// code rather than on the tool. A gate that passed would mean the
		// instance produced a report without seeing the files.
		if got.report.Gate != belay.GateFail {
			t.Errorf("gate %d of %d: Gate = %s, want %s (%s)",
				i+1, len(dirs), got.report.Gate, belay.GateFail, got.report.Summary)
		}
		if want := []string{"added.go:7 errcheck major"}; !equalStrings(findings(got.report.Issues), want) {
			t.Errorf("gate %d of %d: findings = %v, want %v",
				i+1, len(dirs), findings(got.report.Issues), want)
		}
	}
}

// lintIn runs one gate over dir with whatever environment the caller has
// already set, and fails the test if it could not run at all.
func lintIn(t *testing.T, dir string) belay.QualityReport {
	t.Helper()
	var logs bytes.Buffer
	gate := linter.NewGolangCI(linter.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	report, err := gate.Lint(t.Context(), dir)
	if err != nil {
		t.Fatalf("Lint(%s): %v\nlogs:\n%s", dir, err, logs.String())
	}
	return report
}

// equalStrings reports whether a and b hold the same strings in order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
