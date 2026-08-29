// Package cli assembles belay's cobra command tree. Subcommands register
// themselves from their own packages via init(); only the root and version
// commands are defined here.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version information, injected via ldflags during build.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "belay",
	Short: "Durable, resumable orchestrator for autonomous coding agents",
	Long: `belay is a durable, resumable orchestrator for autonomous coding agents.

Instead of one unreliable AI call, it runs a state-machine graph where a dispatcher
persists state after every node, so a crashed run resumes from the last completed
node instead of restarting.`,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// NOTE: Other subcommands (run, resume, runs, timeline) are registered by
	// their respective packages via init(). Do not add them here.
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("belay version %s (commit: %s, built: %s)\n", Version, Commit, BuildDate)
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
