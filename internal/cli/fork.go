package cli

import "strings"

// runCommand writes out the belay command that does verb to view's run,
// naming the workspace the run lives in when it is not the one this command
// was pointed at, so the command finds it from wherever it is pasted.
func runCommand(view outcomeView, verb string) string {
	cmd := "belay "
	if view.workspace != "" {
		cmd += "--workspace " + shellQuote(view.workspace) + " "
	}
	return cmd + verb + " " + view.layout.RunID()
}

// pausedAsAskedSteps is what to do about a run that paused because this
// command asked it to stop after a number of nodes, as a fork does after the
// one it re-runs: see what that step did, then carry the run on, if it is
// worth carrying on. Nothing is waiting for a person to read, so the advice
// a gate's pause gets would send them to a plan nobody is waiting on.
func pausedAsAskedSteps(view outcomeView, resume string) []string {
	return []string{
		"it ran the step it was asked to re-run, and paused before " + view.outcome.Node + ".",
		"see what that step did with:",
		"",
		"    " + runCommand(view, "timeline"),
		"",
		"carry the run on from there with:",
		"",
		"    " + resume,
	}
}

// shellQuote returns s as a shell reads it back unchanged: as it is when it
// holds nothing a shell would act on, and in single quotes otherwise. A
// workspace on macOS is as likely as not to have a space in its path, and a
// command that breaks on paste is worse than one never written out.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
