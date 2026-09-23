package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// fakeEnv answers like an environment holding exactly these variables.
func fakeEnv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// utf8Env is an environment whose locale is UTF-8 and whose terminal can
// draw.
func utf8Env(extra map[string]string) map[string]string {
	vars := map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// alwaysTerminal and neverTerminal stand in for the ioctl check.
func alwaysTerminal(*os.File) bool { return true }
func neverTerminal(*os.File) bool  { return false }

// TestDetectViewerTerminalOpensTheViewerOnlyWhereSomebodyCanAnswerIt is the
// most important test in this package: everywhere the viewer must not open,
// belay behaves as it always has. A viewer opened where nobody is watching
// hangs a scripted run for ever.
func TestDetectViewerTerminalOpensTheViewerOnlyWhereSomebodyCanAnswerIt(t *testing.T) {
	stdin, stdout := os.Stdin, os.Stdout
	tests := []struct {
		name    string
		in      io.Reader
		out     io.Writer
		noInput bool
		vars    map[string]string
		isTTY   func(*os.File) bool
		want    bool
		reason  string
	}{
		{name: "a terminal at both ends", in: stdin, out: stdout, vars: utf8Env(nil), isTTY: alwaysTerminal, want: true},
		{
			name: "--no-input", in: stdin, out: stdout, noInput: true, vars: utf8Env(nil),
			isTTY: alwaysTerminal, reason: "--no-input",
		},
		{
			name: "CI is set", in: stdin, out: stdout, vars: utf8Env(map[string]string{"CI": "true"}),
			isTTY: alwaysTerminal, reason: "CI",
		},
		{
			name: "CI=false", in: stdin, out: stdout, vars: utf8Env(map[string]string{"CI": "false"}),
			isTTY: alwaysTerminal, want: true,
		},
		{
			name: "CI=0", in: stdin, out: stdout, vars: utf8Env(map[string]string{"CI": "0"}),
			isTTY: alwaysTerminal, want: true,
		},
		{
			name: "CI is set but empty", in: stdin, out: stdout, vars: utf8Env(map[string]string{"CI": ""}),
			isTTY: alwaysTerminal, want: true,
		},
		{
			name: "TERM is dumb", in: stdin, out: stdout, vars: utf8Env(map[string]string{"TERM": "dumb"}),
			isTTY: alwaysTerminal, reason: "TERM",
		},
		{
			name: "TERM is unset", in: stdin, out: stdout, vars: map[string]string{"LANG": "en_US.UTF-8"},
			isTTY: alwaysTerminal, reason: "TERM",
		},
		{
			name: "neither end is a terminal", in: stdin, out: stdout, vars: utf8Env(nil),
			isTTY: neverTerminal, reason: "terminal",
		},
		{
			name: "the output is a buffer", in: stdin, out: &bytes.Buffer{}, vars: utf8Env(nil),
			isTTY: alwaysTerminal, reason: "terminal",
		},
		{
			name: "the input is a buffer", in: &bytes.Buffer{}, out: stdout, vars: utf8Env(nil),
			isTTY: alwaysTerminal, reason: "terminal",
		},
		{name: "no streams at all", vars: utf8Env(nil), isTTY: alwaysTerminal, reason: "terminal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectViewerTerminal(tt.in, tt.out, tt.noInput, fakeEnv(tt.vars), tt.isTTY)
			if got.ok != tt.want {
				t.Fatalf("ok = %v, want %v (reason %q)", got.ok, tt.want, got.reason)
			}
			if !tt.want {
				if !strings.Contains(got.reason, tt.reason) {
					t.Errorf("reason = %q, want it to name %q", got.reason, tt.reason)
				}
				if got.in != nil || got.out != nil {
					t.Error("a viewer that may not open was given streams to draw on")
				}
				return
			}
			if got.in == nil || got.out == nil {
				t.Error("a viewer that may open was given no streams")
			}
			if got.reason != "" {
				t.Errorf("reason = %q, want none", got.reason)
			}
		})
	}
}

// TestDetectViewerTerminalAsksTheTerminalDriver: /dev/null is a character
// device, and belay's colour decision takes a character device for a
// terminal. The viewer must not open on one: `belay run </dev/null` from
// cron would hang waiting for a keypress.
func TestDetectViewerTerminalAsksTheTerminalDriver(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	got := detectViewerTerminal(devNull, devNull, false, fakeEnv(utf8Env(nil)), realTerminal)
	if got.ok {
		t.Error("the viewer would open on /dev/null")
	}
	if colorEnabled(devNull, false) != true {
		t.Skip("colour is off here, so this test cannot show the difference between the two checks")
	}
}

// TestUTF8Locale covers what decides whether the viewer draws in ASCII:
// the first of LC_ALL, LC_CTYPE and LANG that is set, and nothing else.
func TestUTF8Locale(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		want    bool
		setting string
	}{
		{name: "nothing set"},
		{name: "LANG is C", vars: map[string]string{"LANG": "C"}, setting: "LANG=C"},
		{name: "LANG is POSIX", vars: map[string]string{"LANG": "POSIX"}, setting: "LANG=POSIX"},
		{name: "LANG is empty", vars: map[string]string{"LANG": ""}},
		{
			name: "LANG is Latin-1", vars: map[string]string{"LANG": "en_US.ISO8859-1"},
			setting: "LANG=en_US.ISO8859-1",
		},
		{name: "LANG is UTF-8", vars: map[string]string{"LANG": "en_US.UTF-8"}, want: true},
		{name: "LANG is utf8", vars: map[string]string{"LANG": "en_US.utf8"}, want: true},
		{name: "C.UTF-8", vars: map[string]string{"LANG": "C.UTF-8"}, want: true},
		{
			name: "LC_ALL wins over LANG", vars: map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"},
			setting: "LC_ALL=C",
		},
		{
			name: "LC_CTYPE wins over LANG", vars: map[string]string{"LC_CTYPE": "C", "LANG": "en_US.UTF-8"},
			setting: "LC_CTYPE=C",
		},
		{name: "LC_ALL wins over LC_CTYPE", vars: map[string]string{"LC_ALL": "en_US.UTF-8", "LC_CTYPE": "C"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, setting := utf8Locale(fakeEnv(tt.vars))
			if got != tt.want {
				t.Errorf("utf8Locale(%v) = %v, want %v", tt.vars, got, tt.want)
			}
			if !tt.want && setting != tt.setting {
				t.Errorf("setting = %q, want %q", setting, tt.setting)
			}
		})
	}
}
