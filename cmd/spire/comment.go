package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var commentCmd = &cobra.Command{
	Use:   "comment <bead-id> [text...]",
	Short: "Add a comment authored by the archmage identity (Name <email>)",
	Long: `Add a comment to a bead with the author resolved from the active
tower's archmage identity (formatted as "Name <email>").

Text sources are mutually exclusive: positional text, --file, or --stdin.
Provide exactly one.

Examples:
  spire comment spi-xxx "This is a comment"
  spire comment spi-xxx --file comment.md
  echo "long body" | spire comment spi-xxx --stdin`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("file")
		useStdin, _ := cmd.Flags().GetBool("stdin")
		return cmdComment(args, file, useStdin)
	},
}

func init() {
	commentCmd.Flags().String("file", "", "Read comment body from file")
	commentCmd.Flags().Bool("stdin", false, "Read comment body from stdin")
}

// commentStdinReader is replaceable for tests.
var commentStdinReader io.Reader = os.Stdin

func cmdComment(args []string, file string, useStdin bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire comment <bead-id> [text...] | --file <path> | --stdin")
	}
	beadID := args[0]
	positional := strings.Join(args[1:], " ")

	text, err := resolveCommentText(positional, file, useStdin)
	if err != nil {
		return err
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	author, err := resolveArchmageAuthor()
	if err != nil {
		return err
	}

	if err := storeAddCommentAs(beadID, author, text); err != nil {
		return fmt.Errorf("comment %s: %w", beadID, err)
	}

	fmt.Printf("commented on %s as %s\n", beadID, author)
	return nil
}

// resolveCommentText picks exactly one text source among positional args,
// --file, and --stdin. Returns an error if none or more than one are set.
func resolveCommentText(positional, file string, useStdin bool) (string, error) {
	sources := 0
	if strings.TrimSpace(positional) != "" {
		sources++
	}
	if file != "" {
		sources++
	}
	if useStdin {
		sources++
	}
	if sources == 0 {
		return "", fmt.Errorf("comment body required: provide positional text, --file, or --stdin")
	}
	if sources > 1 {
		return "", fmt.Errorf("comment body must come from exactly one source (positional, --file, or --stdin)")
	}

	switch {
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		return string(data), nil
	case useStdin:
		data, err := io.ReadAll(commentStdinReader)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	default:
		return positional, nil
	}
}

// resolveArchmageAuthor builds a "Name <email>" author string from the active
// tower's archmage identity. Returns an error with a hint when either field is
// missing or when the email is malformed.
func resolveArchmageAuthor() (string, error) {
	tower, err := activeTowerConfig()
	if err != nil {
		return "", fmt.Errorf("resolve active tower: %w", err)
	}
	name := strings.TrimSpace(tower.Archmage.Name)
	email := strings.TrimSpace(tower.Archmage.Email)
	if name == "" || email == "" {
		return "", fmt.Errorf(
			"archmage identity not set on tower %q.\nRun: spire tower set --archmage-name \"Your Name\" --archmage-email you@example.com",
			tower.Name,
		)
	}
	if !strings.Contains(email, "@") {
		return "", fmt.Errorf(
			"archmage email %q is malformed (missing '@').\nRun: spire tower set --archmage-email you@example.com",
			email,
		)
	}
	return fmt.Sprintf("%s <%s>", name, email), nil
}
