package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func init() {
	// --- Simple commands (no flags or minimal) ---
	rootCmd.AddCommand(registerCmd)
	rootCmd.AddCommand(unregisterCmd)
	rootCmd.AddCommand(focusCmd)
	rootCmd.AddCommand(grokCmd)
	rootCmd.AddCommand(inboxCmd)
	rootCmd.AddCommand(readCmd)
	rootCmd.AddCommand(claimCmd)
	rootCmd.AddCommand(closeCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(shutdownCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(sqlCmd)

	// --- Commands with flags ---
	rootCmd.AddCommand(fileCmd)
	rootCmd.AddCommand(designCmd)
	rootCmd.AddCommand(specCmd)
	rootCmd.AddCommand(sendCmd)
	rootCmd.AddCommand(collectCmd)
	rootCmd.AddCommand(boardCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(stewardCmd)
	rootCmd.AddCommand(summonCmd)
	rootCmd.AddCommand(dismissCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(resummonCmd)
	rootCmd.AddCommand(resolveCmd)
	rootCmd.AddCommand(recoverCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(metricsCmd)
	rootCmd.AddCommand(traceCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(alertCmd)
	rootCmd.AddCommand(rosterCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(pullCmd)
	rootCmd.AddCommand(pushCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(doctorCmd)

	// --- Nested command groups ---
	rootCmd.AddCommand(towerCmd)
	rootCmd.AddCommand(repoCmd)
	rootCmd.AddCommand(workshopCmd)
	rootCmd.AddCommand(formulaCmd)

	// --- Integration commands ---
	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(disconnectCmd)
	rootCmd.AddCommand(serveCmd)

	// --- Internal / advanced ---
	rootCmd.AddCommand(executeCmd)
	rootCmd.AddCommand(wizardEpicCmd)
	rootCmd.AddCommand(wizardRunCmd)
	rootCmd.AddCommand(wizardReviewCmd)

	// --- Version ---
	rootCmd.AddCommand(versionCmd)

	// --- Completion ---
	rootCmd.AddCommand(completionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("spire", version)
		binPath := doltResolvedBinPath()
		if binPath == "" {
			fmt.Println("dolt  not installed")
		} else {
			v, err := doltInstalledVersion(binPath)
			if err != nil {
				fmt.Printf("dolt  (unknown version) (%s)\n", binPath)
			} else {
				fmt.Printf("dolt  v%s (%s)\n", v, binPath)
			}
		}
		return nil
	},
}

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion scripts",
	Long: `Generate shell completion scripts for spire.

To load completions:

Bash:
  $ source <(spire completion bash)

Zsh:
  $ spire completion zsh > "${fpath[1]}/_spire"

Fish:
  $ spire completion fish | source

PowerShell:
  PS> spire completion powershell | Out-String | Invoke-Expression`,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	DisableFlagsInUseLine: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		}
		return nil
	},
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "spire: %s\n", err)
		os.Exit(1)
	}
}
