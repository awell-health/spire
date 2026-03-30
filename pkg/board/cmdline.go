package board

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// CmdlineState holds command-line input state.
type CmdlineState struct {
	Active      bool     // true when command mode is active
	Input       string   // current input text (without leading ':')
	CursorPos   int      // cursor position within Input
	Completions []string // current completion suggestions
	CompIdx     int      // selected completion index (-1 = none)
	CompPrefix  string   // the input prefix that generated current completions
	History     []string // previous commands (most recent last)
	HistIdx     int      // -1 = editing current input, 0+ = browsing history
	SavedInput  string   // saved current input when browsing history
	Output      string   // last command output (shown briefly in status)
	OutputErr   bool     // true if last command was an error
}

// HandleCmdlineKey processes a keypress in command mode.
// Returns updated state. done=true means command mode should close.
// execCmd is non-empty when Enter was pressed with input to execute.
// rootCmd is needed for tab completion; pass nil if unavailable.
func HandleCmdlineKey(state CmdlineState, msg tea.KeyMsg, rootCmd *cobra.Command) (newState CmdlineState, done bool, execCmd string) {
	newState = state

	switch msg.Type {
	case tea.KeyEsc:
		return CmdlineState{
			History: state.History,
			CompIdx: -1,
			HistIdx: -1,
		}, true, ""

	case tea.KeyEnter:
		input := strings.TrimSpace(newState.Input)
		return CmdlineState{
			History: state.History,
			CompIdx: -1,
			HistIdx: -1,
		}, true, input

	case tea.KeyBackspace:
		if newState.CursorPos == 0 && newState.Input == "" {
			return CmdlineState{
				History: state.History,
				CompIdx: -1,
				HistIdx: -1,
			}, true, ""
		}
		if newState.CursorPos > 0 {
			newState.Input = newState.Input[:newState.CursorPos-1] + newState.Input[newState.CursorPos:]
			newState.CursorPos--
			newState.clearCompletions()
		}
		return newState, false, ""

	case tea.KeyLeft:
		if newState.CursorPos > 0 {
			newState.CursorPos--
		}
		return newState, false, ""

	case tea.KeyRight:
		if newState.CursorPos < len(newState.Input) {
			newState.CursorPos++
		}
		return newState, false, ""

	case tea.KeyUp:
		if len(newState.History) == 0 {
			return newState, false, ""
		}
		if newState.HistIdx == -1 {
			newState.SavedInput = newState.Input
			newState.HistIdx = len(newState.History) - 1
		} else if newState.HistIdx > 0 {
			newState.HistIdx--
		}
		newState.Input = newState.History[newState.HistIdx]
		newState.CursorPos = len(newState.Input)
		newState.clearCompletions()
		return newState, false, ""

	case tea.KeyDown:
		if newState.HistIdx == -1 {
			return newState, false, ""
		}
		if newState.HistIdx < len(newState.History)-1 {
			newState.HistIdx++
			newState.Input = newState.History[newState.HistIdx]
		} else {
			newState.HistIdx = -1
			newState.Input = newState.SavedInput
		}
		newState.CursorPos = len(newState.Input)
		newState.clearCompletions()
		return newState, false, ""

	case tea.KeyTab:
		newState = handleTabCompletion(newState, rootCmd, false)
		return newState, false, ""

	case tea.KeyShiftTab:
		newState = handleTabCompletion(newState, rootCmd, true)
		return newState, false, ""

	case tea.KeyCtrlA:
		newState.CursorPos = 0
		return newState, false, ""

	case tea.KeyCtrlE:
		newState.CursorPos = len(newState.Input)
		return newState, false, ""

	case tea.KeyCtrlU:
		newState.Input = ""
		newState.CursorPos = 0
		newState.clearCompletions()
		return newState, false, ""

	case tea.KeyCtrlW:
		if newState.CursorPos == 0 {
			return newState, false, ""
		}
		before := newState.Input[:newState.CursorPos]
		after := newState.Input[newState.CursorPos:]
		trimmed := strings.TrimRight(before, " ")
		lastSpace := strings.LastIndex(trimmed, " ")
		if lastSpace == -1 {
			before = ""
		} else {
			before = trimmed[:lastSpace+1]
		}
		newState.Input = before + after
		newState.CursorPos = len(before)
		newState.clearCompletions()
		return newState, false, ""

	case tea.KeySpace:
		newState.Input = newState.Input[:newState.CursorPos] + " " + newState.Input[newState.CursorPos:]
		newState.CursorPos++
		newState.clearCompletions()
		return newState, false, ""

	case tea.KeyRunes:
		ch := msg.String()
		newState.Input = newState.Input[:newState.CursorPos] + ch + newState.Input[newState.CursorPos:]
		newState.CursorPos += len(ch)
		newState.clearCompletions()
		return newState, false, ""
	}

	return newState, false, ""
}

func (s *CmdlineState) clearCompletions() {
	s.Completions = nil
	s.CompIdx = -1
	s.CompPrefix = ""
}

// handleTabCompletion manages the tab/shift-tab completion cycle.
func handleTabCompletion(state CmdlineState, rootCmd *cobra.Command, reverse bool) CmdlineState {
	if len(state.Completions) == 0 {
		comps := GetCompletions(rootCmd, state.Input)
		if len(comps) == 0 {
			return state
		}
		state.Completions = comps
		state.CompPrefix = state.Input
		state.CompIdx = 0
	} else {
		if reverse {
			state.CompIdx--
			if state.CompIdx < 0 {
				state.CompIdx = len(state.Completions) - 1
			}
		} else {
			state.CompIdx++
			if state.CompIdx >= len(state.Completions) {
				state.CompIdx = 0
			}
		}
	}

	if state.CompIdx >= 0 && state.CompIdx < len(state.Completions) {
		state.Input = applyCompletion(state.CompPrefix, state.Completions[state.CompIdx])
		state.CursorPos = len(state.Input)
	}
	return state
}

// applyCompletion replaces the last word in the input with the completion.
func applyCompletion(input, completion string) string {
	trimmed := strings.TrimRight(input, " ")
	lastSpace := strings.LastIndex(trimmed, " ")
	if lastSpace == -1 {
		return completion
	}
	return trimmed[:lastSpace+1] + completion
}

// GetCompletions returns completion suggestions for the current input.
// Uses cobra's command tree for traversal.
func GetCompletions(rootCmd *cobra.Command, input string) []string {
	if rootCmd == nil {
		return nil
	}

	args := splitArgs(input)
	trailingSpace := strings.HasSuffix(input, " ") || input == ""

	if len(args) == 0 || (len(args) == 1 && !trailingSpace) {
		prefix := ""
		if len(args) == 1 {
			prefix = strings.ToLower(args[0])
		}
		return matchCmdNames(rootCmd.Commands(), prefix)
	}

	// Walk the command tree.
	cmd := rootCmd
	consumed := 0
	for consumed < len(args) {
		sub, _, err := cmd.Find([]string{args[consumed]})
		if err != nil || sub == cmd {
			break
		}
		cmd = sub
		consumed++
	}

	remaining := args[consumed:]
	if trailingSpace {
		return matchCmdNamesAndFlags(cmd, "")
	}
	if len(remaining) > 0 {
		partial := remaining[len(remaining)-1]
		if strings.HasPrefix(partial, "-") {
			return matchCmdFlags(cmd, partial)
		}
		return matchCmdNames(cmd.Commands(), strings.ToLower(partial))
	}
	return matchCmdNamesAndFlags(cmd, "")
}

// matchCmdNames returns command names matching the prefix, sorted alphabetically.
func matchCmdNames(cmds []*cobra.Command, prefix string) []string {
	var matches []string
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		if strings.HasPrefix(strings.ToLower(c.Name()), prefix) {
			matches = append(matches, c.Name())
		}
		for _, alias := range c.Aliases {
			if strings.HasPrefix(strings.ToLower(alias), prefix) && !strSliceContains(matches, alias) {
				matches = append(matches, alias)
			}
		}
	}
	sort.Strings(matches)
	return matches
}

// matchCmdFlags returns flag names for the command matching the prefix.
func matchCmdFlags(cmd *cobra.Command, prefix string) []string {
	var matches []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		full := "--" + f.Name
		if strings.HasPrefix(full, prefix) {
			matches = append(matches, full)
		}
	})
	sort.Strings(matches)
	return matches
}

// matchCmdNamesAndFlags returns both subcommand names and flags.
func matchCmdNamesAndFlags(cmd *cobra.Command, prefix string) []string {
	result := matchCmdNames(cmd.Commands(), prefix)
	if strings.HasPrefix(prefix, "-") {
		result = append(result, matchCmdFlags(cmd, prefix)...)
	}
	return result
}

func strSliceContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// splitArgs splits input into arguments, respecting double-quoted strings.
func splitArgs(input string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	for _, r := range input {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// RenderCmdline renders the command line bar for the bottom of the screen.
// Includes any completion popup above the bar.
func RenderCmdline(state CmdlineState, width int) string {
	var result strings.Builder

	if len(state.Completions) > 0 && state.CompIdx >= 0 {
		result.WriteString(renderCompletionPopup(state.Completions, state.CompIdx))
		result.WriteString("\n")
	}

	barStyle := lipgloss.NewStyle().Background(lipgloss.Color("236")).Width(width)
	prefixStyle := lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236"))

	input := state.Input
	var display string
	if state.CursorPos >= len(input) {
		display = input + "█"
	} else {
		display = input[:state.CursorPos] + "█" + input[state.CursorPos+1:]
	}

	maxInputWidth := width - 2
	if maxInputWidth < 10 {
		maxInputWidth = 10
	}
	if len(display) > maxInputWidth {
		start := state.CursorPos - maxInputWidth/2
		if start < 0 {
			start = 0
		}
		end := start + maxInputWidth
		if end > len(display) {
			end = len(display)
			start = end - maxInputWidth
			if start < 0 {
				start = 0
			}
		}
		display = display[start:end]
	}

	line := prefixStyle.Render(":") + display
	result.WriteString(barStyle.Render(line))

	return result.String()
}

// renderCompletionPopup renders a bordered popup of completion suggestions.
func renderCompletionPopup(completions []string, selected int) string {
	maxVisible := 10
	if len(completions) < maxVisible {
		maxVisible = len(completions)
	}

	scrollStart := 0
	if selected >= maxVisible {
		scrollStart = selected - maxVisible + 1
	}
	scrollEnd := scrollStart + maxVisible
	if scrollEnd > len(completions) {
		scrollEnd = len(completions)
		scrollStart = scrollEnd - maxVisible
		if scrollStart < 0 {
			scrollStart = 0
		}
	}

	maxW := 0
	for _, c := range completions[scrollStart:scrollEnd] {
		if len(c) > maxW {
			maxW = len(c)
		}
	}
	if maxW < 12 {
		maxW = 12
	}

	normalStyle := lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1).Width(maxW + 2)
	selectedStyle := normalStyle.Background(lipgloss.Color("4")).Foreground(lipgloss.Color("15"))

	var lines []string
	for i := scrollStart; i < scrollEnd; i++ {
		if i == selected {
			lines = append(lines, selectedStyle.Render(completions[i]))
		} else {
			lines = append(lines, normalStyle.Render(completions[i]))
		}
	}

	popup := strings.Join(lines, "\n")
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8"))

	return borderStyle.Render(popup)
}

// ExecuteCmd runs a command string using the cobra command tree.
// Returns output string and error.
// Redirects os.Stdout/Stderr to prevent garbling the TUI alt-screen,
// since many commands use fmt.Printf instead of cmd.OutOrStdout().
func ExecuteCmd(rootCmd *cobra.Command, input string) (string, error) {
	if rootCmd == nil {
		return "", fmt.Errorf("no command root available")
	}

	args := splitArgs(input)
	if len(args) == 0 {
		return "", nil
	}

	// Capture os.Stdout/Stderr — commands use fmt.Printf, not cobra's output.
	oldStdout, oldStderr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w
	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	var outBuf, errBuf bytes.Buffer
	rootCmd.SetOut(&outBuf)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(args)

	err := rootCmd.Execute()

	// Close the write end and read captured output.
	w.Close()
	var captured bytes.Buffer
	captured.ReadFrom(r)
	r.Close()

	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)

	output := captured.String() + outBuf.String()
	if errOutput := errBuf.String(); errOutput != "" {
		if output != "" {
			output += "\n"
		}
		output += errOutput
	}

	return strings.TrimSpace(output), err
}
