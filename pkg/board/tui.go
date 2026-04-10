package board

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// PendingAction identifies an action to run after the TUI exits.
type PendingAction int

const (
	ActionNone     PendingAction = iota
	ActionFocus                  // print cmdFocus output, then relaunch
	ActionLogs                   // tail wizard logs, then relaunch
	ActionSummon                 // summon a wizard for the bead, then relaunch
	ActionClaim                  // claim the bead, then relaunch
	ActionResummon               // resummon a stuck bead (needs-human), then relaunch
	ActionClose                  // close/dismiss the bead (inline via tea.Cmd)
	ActionUnsummon               // dismiss active wizard (inline via tea.Cmd)
	ActionResetSoft              // soft reset (inline via tea.Cmd)
	ActionResetHard              // reset --hard (inline via tea.Cmd)
	ActionGrok                   // deep focus grok (inline via tea.Cmd)
	ActionTrace                  // DAG timeline trace (inline via tea.Cmd)
	ActionApprove                // approve a needs-human bead (remove label, inline via tea.Cmd)
	ActionApproveDesign          // approve a needs-human design bead (close it, inline via tea.Cmd)
	ActionRejectDesign           // reject a design bead with feedback comment (inline via tea.Cmd)
	ActionDefer                  // toggle deferred status (inline via tea.Cmd)
	ActionResolve                // resolve a needs-human bead with recovery learning (inline via tea.Cmd)
	ActionApproveGate            // approve a human.approve gate (remove awaiting-approval + needs-human, inline via tea.Cmd)
)

// Section identifies which vertical zone of the board the cursor is in.
type Section int

const (
	SectionAlerts  Section = iota // alert beads above the columns
	SectionColumns                // the main phase columns
	SectionLower                  // blocked + interrupted side-by-side below the columns
)

// Model is the Bubble Tea model for the board TUI.
type Model struct {
	Opts          Opts
	Cols          Columns
	Agents        []LocalAgent // alive local wizards from registry
	Identity      string       // user identity for snapshot fetching
	TypeScope     TypeScope
	ShowAllCols   bool // when true, show all phase columns including empty ones
	Width         int
	Height        int
	LastTick      time.Time
	Quitting      bool
	SelSection    Section   // which vertical zone the cursor is in
	ViewMode      ViewMode  // active tabbed view (board, alerts, lower)
	SelCol        int       // selected column index into DisplayColumns()
	SelCard       int     // selected card index within selCol
	SelLowerCol   int     // 0 = BLOCKED, 1 = INTERRUPTED (within SectionLower)
	ColScroll     int     // scroll offset for the selected column (beads above viewport)
	PendingAction PendingAction
	PendingBeadID string

	// Snapshot holds the pre-fetched board state assembled in the background.
	// View() reads from this struct — no I/O during render.
	Snapshot *BoardSnapshot

	// Inspector state.
	Inspecting      bool            // true when the inspector pane is visible
	InspectorData   *InspectorData  // fetched detail data (nil when loading)
	InspectorLoading bool           // true while async fetch is in progress
	InspectorScroll int             // scroll offset within the inspector

	// FetchAgentsFn is called to refresh local agents. Injected by the caller.
	FetchAgentsFn func() []LocalAgent

	// Action menu overlay state.
	ActionMenuOpen   bool
	ActionMenuItems  []MenuAction
	ActionMenuCursor int
	ActionMenuBeadID    string
	ActionMenuBeadTitle string

	// Search/filter state.
	SearchActive bool   // true when user is typing a search query
	SearchQuery  string // current search filter text

	// Inspector tab (0=details, 1=logs).
	InspectorTab int

	// Vim gg key sequence: true after first g press, waiting for second key.
	PendingG bool

	// Inline action execution state.
	ActionRunning    bool
	ActionStatus     string    // transient status message shown in footer
	ActionStatusTime time.Time // when status was set (for auto-clear)

	// InlineActionFn executes an action within the TUI via tea.Cmd.
	// Returns nil on success, error on failure.
	InlineActionFn func(PendingAction, string) error

	// RejectDesignFn adds a rejection comment to a design bead. Injected by the caller.
	RejectDesignFn func(beadID, feedback string) error

	// Feedback input state for design bead rejection.
	FeedbackActive  bool   // true when feedback text input is shown
	FeedbackInput   string // current text
	FeedbackBeadID  string // bead to add the comment to

	// Resolve input state for needs-human bead resolution.
	ResolveFn      func(beadID, comment string) error
	ResolveActive  bool
	ResolveInput   string
	ResolveBeadID  string

	// Confirmation dialog state.
	ConfirmOpen   bool
	ConfirmAction PendingAction
	ConfirmBeadID string
	ConfirmPrompt string
	ConfirmDanger DangerLevel

	// Command mode state.
	Cmdline     CmdlineState   // vim-style command line state
	CmdlineRoot *cobra.Command // root cobra command for parsing/completion

	// Tower switcher overlay state.
	TowerSwitcherOpen   bool
	TowerSwitcherItems  []TowerItem
	TowerSwitcherCursor int

	// Terminal pane overlay state (generic scrollable content viewer).
	TermOpen    bool     // true when the terminal pane is visible
	TermTitle   string   // title bar text
	TermLines   []string // pre-split content lines for scrolling
	TermScroll  int      // scroll offset (first visible line index)
	TermLoading bool     // true while async content fetch is in progress
	TermBeadID  string   // bead ID for refresh

	// TermContentFn fetches content for the terminal pane. Injected by the caller.
	// Takes (beadID, contentWidth) and returns rendered content string.
	TermContentFn func(string, int) (string, error)
}

// VisibleCols returns the columns filtered by the current type scope.
func (m Model) VisibleCols() Columns {
	return FilterTypeScope(m.Cols, m.TypeScope)
}

// DisplayColumns returns the columns to display, respecting ShowAllCols toggle
// and search filter. This is the single filtering point for search — both
// View() and navigation use these results.
func (m Model) DisplayColumns() []ColDef {
	vis := m.VisibleCols()
	if m.SearchQuery != "" {
		vis = FilterColumns(vis, m.SearchQuery)
	}
	if m.ShowAllCols {
		return AllColumns(vis)
	}
	return ActiveColumns(vis)
}

// ensureCardVisible adjusts ColScroll so SelCard is within the visible window.
func (m *Model) ensureCardVisible(maxCards int) {
	if maxCards <= 0 {
		return
	}
	if m.SelCard < m.ColScroll {
		m.ColScroll = m.SelCard
	}
	if m.SelCard >= m.ColScroll+maxCards {
		m.ColScroll = m.SelCard - maxCards + 1
	}
	if m.ColScroll < 0 {
		m.ColScroll = 0
	}
}

// colMaxCards computes MaxCards from the current board state.
func (m *Model) colMaxCards() int {
	displayCols := m.DisplayColumns()
	warningCount := 0
	if m.Snapshot != nil {
		warningCount = len(m.Snapshot.Warnings)
	}
	// In board view, columns get all vertical space (no alerts/lower deductions).
	budget := CalcHeightBudget(m.Height, warningCount, 0, 0, 0, len(displayCols), len(m.Agents))
	return budget.MaxCards
}

// ClampSelection keeps SelSection, SelCol, and SelCard within valid bounds.
func (m *Model) ClampSelection() {
	vis := m.VisibleCols()

	// Force SelSection to match the active ViewMode.
	switch m.ViewMode {
	case ViewAlerts:
		m.SelSection = SectionAlerts
	case ViewBoard:
		m.SelSection = SectionColumns
	case ViewLower:
		m.SelSection = SectionLower
	}

	switch m.SelSection {
	case SectionAlerts:
		n := len(vis.Alerts)
		if m.SelCard < 0 {
			m.SelCard = 0
		}
		if m.SelCard >= n {
			m.SelCard = n - 1
		}
		return
	case SectionLower:
		// Clamp SelLowerCol to a sub-column that has items.
		hasBlocked := len(vis.Blocked) > 0
		hasInterrupted := len(vis.Interrupted) > 0
		if m.SelLowerCol == 0 && !hasBlocked && hasInterrupted {
			m.SelLowerCol = 1
		}
		if m.SelLowerCol == 1 && !hasInterrupted && hasBlocked {
			m.SelLowerCol = 0
		}
		var items []BoardBead
		if m.SelLowerCol == 0 {
			items = vis.Blocked
		} else {
			items = vis.Interrupted
		}
		n := len(items)
		if m.SelCard < 0 {
			m.SelCard = 0
		}
		if n > 0 && m.SelCard >= n {
			m.SelCard = n - 1
		}
		return
	}

	// SectionColumns
	active := m.DisplayColumns()
	if len(active) == 0 {
		m.SelCol = 0
		m.SelCard = 0
		return
	}
	if m.SelCol < 0 {
		m.SelCol = 0
	}
	if m.SelCol >= len(active) {
		m.SelCol = len(active) - 1
	}
	n := len(active[m.SelCol].Beads)
	if n == 0 {
		m.SelCard = 0
		m.ColScroll = 0
		return
	}
	m.SelCard = ((m.SelCard % n) + n) % n

	// Clamp ColScroll to valid range.
	if m.ColScroll > m.SelCard {
		m.ColScroll = m.SelCard
	}
	if m.ColScroll > n-1 {
		m.ColScroll = n - 1
	}
	if m.ColScroll < 0 {
		m.ColScroll = 0
	}
}

// SelectedBead returns a pointer to the currently selected bead, or nil.
func (m Model) SelectedBead() *BoardBead {
	vis := m.VisibleCols()
	switch m.SelSection {
	case SectionAlerts:
		if m.SelCard >= 0 && m.SelCard < len(vis.Alerts) {
			return &vis.Alerts[m.SelCard]
		}
		return nil
	case SectionLower:
		var items []BoardBead
		if m.SelLowerCol == 0 {
			items = vis.Blocked
		} else {
			items = vis.Interrupted
		}
		if m.SelCard >= 0 && m.SelCard < len(items) {
			return &items[m.SelCard]
		}
		return nil
	default: // SectionColumns
		active := m.DisplayColumns()
		if m.SelCol < 0 || m.SelCol >= len(active) {
			return nil
		}
		beads := active[m.SelCol].Beads
		if m.SelCard < 0 || m.SelCard >= len(beads) {
			return nil
		}
		return &beads[m.SelCard]
	}
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// RunBoardTUI runs the board TUI in a loop, executing pending actions between launches.
// actionFn is called when the TUI exits with a pending action; it returns true to relaunch.
// inlineActionFn is used for actions that execute within the TUI via tea.Cmd (no exit-relaunch).
func RunBoardTUI(opts Opts, identity string, fetchAgents func() []LocalAgent, actionFn func(PendingAction, string) bool, inlineActionFn func(PendingAction, string) error, rejectDesignFn ...func(string, string) error) error {
	var rejectFn func(string, string) error
	if len(rejectDesignFn) > 0 {
		rejectFn = rejectDesignFn[0]
	}
	for {
		m := Model{
			Opts:           opts,
			Identity:       identity,
			LastTick:       time.Now(),
			SelSection:     SectionColumns,
			FetchAgentsFn:  fetchAgents,
			InlineActionFn: inlineActionFn,
			RejectDesignFn: rejectFn,
			ResolveFn:      opts.ResolveFn,
			CmdlineRoot:    opts.RootCmd,
			TermContentFn:  opts.TermContentFn,
		}
		p := tea.NewProgram(m, tea.WithAltScreen())
		result, err := p.Run()
		if err != nil {
			return err
		}

		final, ok := result.(Model)
		if !ok || final.PendingAction == ActionNone {
			break
		}

		if !actionFn(final.PendingAction, final.PendingBeadID) {
			break
		}
	}
	return nil
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
}

// actionResultMsg carries the result of an inline action executed via tea.Cmd.
type actionResultMsg struct {
	Action PendingAction
	BeadID string
	Err    error
}

// termContentMsg carries async-fetched content for the terminal pane overlay.
type termContentMsg struct {
	Title   string
	Content string
	BeadID  string
	Err     error
}

// fetchTermContentCmd returns a tea.Cmd that fetches content for the terminal pane.
func fetchTermContentCmd(fn func(string, int) (string, error), beadID, title string, width int) tea.Cmd {
	return func() tea.Msg {
		content, err := fn(beadID, width)
		return termContentMsg{Title: title, Content: content, BeadID: beadID, Err: err}
	}
}

// runInlineActionCmd returns a tea.Cmd that executes an action in a goroutine.
func runInlineActionCmd(fn func(PendingAction, string) error, action PendingAction, beadID string) tea.Cmd {
	return func() tea.Msg {
		err := fn(action, beadID)
		return actionResultMsg{Action: action, BeadID: beadID, Err: err}
	}
}

// actionLabel returns a human-readable label for an action.
func actionLabel(a PendingAction) string {
	switch a {
	case ActionSummon:
		return "Summon"
	case ActionResummon:
		return "Resummon"
	case ActionUnsummon:
		return "Unsummon"
	case ActionResetSoft:
		return "Reset"
	case ActionResetHard:
		return "Reset --hard"
	case ActionGrok:
		return "Grok"
	case ActionTrace:
		return "Trace"
	case ActionClose:
		return "Close"
	case ActionApprove:
		return "Approve"
	case ActionApproveDesign:
		return "Approve design"
	case ActionRejectDesign:
		return "Reject design"
	case ActionDefer:
		return "Defer"
	case ActionResolve:
		return "Resolve"
	case ActionApproveGate:
		return "Approve gate"
	default:
		return "Action"
	}
}

// cmdlineDoneMsg carries the result of a command-line execution.
type cmdlineDoneMsg struct {
	output string
	err    error
}

// updateCmdline handles keypresses while command mode is active.
func (m Model) updateCmdline(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	newState, done, execCmd := HandleCmdlineKey(m.Cmdline, msg, m.CmdlineRoot)
	m.Cmdline = newState
	if done {
		m.Cmdline.Active = false
		if execCmd != "" {
			m.Cmdline.History = append(m.Cmdline.History, execCmd)
			m.ActionRunning = true
			m.ActionStatus = "Running: " + execCmd
			m.ActionStatusTime = time.Now()
			rootCmd := m.CmdlineRoot
			return m, func() tea.Msg {
				output, err := ExecuteCmd(rootCmd, execCmd)
				return cmdlineDoneMsg{output: output, err: err}
			}
		}
	}
	return m, nil
}

// isInlineAction returns true if the action should execute within the TUI.
func isInlineAction(a PendingAction) bool {
	switch a {
	case ActionSummon, ActionResummon, ActionUnsummon, ActionResetSoft, ActionResetHard, ActionGrok, ActionClose, ActionApprove, ActionApproveDesign, ActionApproveGate, ActionDefer:
		return true
	}
	return false
}

// truncateTitle truncates a title to maxLen runes, appending "…" if truncated.
func truncateTitle(title string, maxLen int) string {
	runes := []rune(title)
	if len(runes) <= maxLen {
		return title
	}
	if maxLen <= 1 {
		return "…"
	}
	return string(runes[:maxLen-1]) + "…"
}

// confirmPromptForAction returns the confirmation prompt text for an action.
// If title is non-empty, it is appended after the bead ID for context.
func confirmPromptForAction(action PendingAction, beadID, title string) string {
	label := beadID
	if title != "" {
		label = fmt.Sprintf("%s: %s", beadID, truncateTitle(title, 50))
	}
	switch action {
	case ActionClose:
		return fmt.Sprintf("Close %s?", label)
	case ActionApprove:
		return fmt.Sprintf("Approve design %s?", label)
	case ActionApproveDesign:
		return fmt.Sprintf("Approve design %s?", label)
	case ActionApproveGate:
		return fmt.Sprintf("Approve and advance %s?", label)
	case ActionUnsummon:
		return fmt.Sprintf("Dismiss wizard for %s?", label)
	case ActionResetSoft:
		return fmt.Sprintf("Reset %s?", label)
	case ActionResetHard:
		return fmt.Sprintf("Hard reset %s? This is destructive.", label)
	default:
		return fmt.Sprintf("%s %s?", actionLabel(action), label)
	}
}

// dangerForAction returns the danger level for an action.
func dangerForAction(action PendingAction) DangerLevel {
	switch action {
	case ActionResetHard:
		return DangerDestructive
	case ActionClose, ActionUnsummon, ActionResetSoft, ActionApprove, ActionApproveDesign, ActionApproveGate:
		return DangerConfirm
	default:
		return DangerNone
	}
}

// updateConfirm handles key input in the confirmation dialog.
func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		action := m.ConfirmAction
		beadID := m.ConfirmBeadID
		m.ConfirmOpen = false
		if m.ActionRunning {
			return m, nil
		}
		m.ActionRunning = true
		m.ActionStatus = actionLabel(action) + "..."
		m.ActionStatusTime = time.Now()
		return m, runInlineActionCmd(m.InlineActionFn, action, beadID)
	case "n", "N", "esc":
		m.ConfirmOpen = false
		return m, nil
	}
	return m, nil
}

// dispatchInlineAction dispatches an inline action via tea.Cmd if the Model has an InlineActionFn.
func (m *Model) dispatchInlineAction(action PendingAction, beadID string) (Model, tea.Cmd) {
	if m.ActionRunning {
		return *m, nil
	}
	if m.InlineActionFn == nil {
		// Fallback to exit-relaunch pattern if no inline fn provided.
		m.PendingAction = action
		m.PendingBeadID = beadID
		m.Quitting = true
		return *m, tea.Quit
	}
	m.ActionRunning = true
	m.ActionStatus = actionLabel(action) + "..."
	m.ActionStatusTime = time.Now()
	return *m, runInlineActionCmd(m.InlineActionFn, action, beadID)
}

// copyToClipboard pipes text to the system clipboard command.
func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		cmd = exec.Command("xclip", "-selection", "clipboard")
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}


// dispatchMenuAction handles an action selected from the action menu.
// ActionRejectDesign and ActionResolve get special treatment: they open the inspector + text input.
func (m *Model) dispatchMenuAction(item MenuAction) (Model, tea.Cmd) {
	if item.ActionType == ActionResolve {
		// Open inspector and activate resolve input.
		m.Inspecting = true
		m.InspectorScroll = 0
		m.InspectorTab = 0
		m.ResolveActive = true
		m.ResolveInput = ""
		m.ResolveBeadID = m.ActionMenuBeadID
		m.InspectorLoading = true
		m.InspectorData = nil
		if bead := m.SelectedBead(); bead != nil {
			return *m, fetchInspectorCmd(*bead)
		}
		return *m, nil
	}
	if item.ActionType == ActionRejectDesign {
		// Open inspector and activate feedback input.
		m.Inspecting = true
		m.InspectorScroll = 0
		m.InspectorTab = 0
		m.FeedbackActive = true
		m.FeedbackInput = ""
		m.FeedbackBeadID = m.ActionMenuBeadID
		m.InspectorLoading = true
		m.InspectorData = nil
		if bead := m.SelectedBead(); bead != nil {
			return *m, fetchInspectorCmd(*bead)
		}
		return *m, nil
	}
	if item.ActionType == ActionTrace {
		// Open terminal pane with trace content.
		beadID := m.ActionMenuBeadID
		m.TermOpen = true
		m.TermLoading = true
		m.TermTitle = "Trace: " + beadID
		m.TermBeadID = beadID
		m.TermLines = nil
		m.TermScroll = 0
		if m.TermContentFn != nil {
			contentWidth := m.Width * 9 / 10
			if contentWidth < 80 {
				contentWidth = 80
			}
			return *m, fetchTermContentCmd(m.TermContentFn, beadID, m.TermTitle, contentWidth)
		}
		return *m, nil
	}
	if isInlineAction(item.ActionType) {
		if item.Danger != DangerNone {
			m.ConfirmOpen = true
			m.ConfirmAction = item.ActionType
			m.ConfirmBeadID = m.ActionMenuBeadID
			m.ConfirmPrompt = confirmPromptForAction(item.ActionType, m.ActionMenuBeadID, m.ActionMenuBeadTitle)
			m.ConfirmDanger = item.Danger
			return *m, nil
		}
		return m.dispatchInlineAction(item.ActionType, m.ActionMenuBeadID)
	}
	m.PendingAction = item.ActionType
	m.PendingBeadID = m.ActionMenuBeadID
	m.Quitting = true
	return *m, tea.Quit
}

// rejectDesignResultMsg carries the result of a design rejection action.
type rejectDesignResultMsg struct {
	BeadID string
	Err    error
}

// resolveResultMsg carries the result of a resolve action.
type resolveResultMsg struct {
	BeadID string
	Err    error
}

// updateFeedbackInput handles keypresses in the feedback text input mode.
func (m Model) updateFeedbackInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.FeedbackActive = false
		m.FeedbackInput = ""
		m.FeedbackBeadID = ""
		return m, nil
	case "enter":
		feedback := strings.TrimSpace(m.FeedbackInput)
		if feedback == "" {
			m.ActionStatus = "Feedback required"
			m.ActionStatusTime = time.Now()
			return m, nil
		}
		beadID := m.FeedbackBeadID
		m.FeedbackActive = false
		m.FeedbackInput = ""
		m.FeedbackBeadID = ""
		m.ActionRunning = true
		m.ActionStatus = "Rejecting design..."
		m.ActionStatusTime = time.Now()
		rejectFn := m.RejectDesignFn
		return m, func() tea.Msg {
			var err error
			if rejectFn != nil {
				err = rejectFn(beadID, feedback)
			} else {
				err = fmt.Errorf("reject design not available")
			}
			return rejectDesignResultMsg{BeadID: beadID, Err: err}
		}
	case "backspace":
		if len(m.FeedbackInput) > 0 {
			m.FeedbackInput = m.FeedbackInput[:len(m.FeedbackInput)-1]
		}
		return m, nil
	case "ctrl+u":
		m.FeedbackInput = ""
		return m, nil
	default:
		if len(msg.String()) == 1 && msg.String()[0] >= 32 {
			m.FeedbackInput += msg.String()
		}
		return m, nil
	}
}

// updateResolveInput handles keypresses in the resolve text input mode.
func (m Model) updateResolveInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.ResolveActive = false
		m.ResolveInput = ""
		m.ResolveBeadID = ""
		return m, nil
	case "enter":
		comment := strings.TrimSpace(m.ResolveInput)
		if comment == "" {
			m.ActionStatus = "Resolution comment required"
			m.ActionStatusTime = time.Now()
			return m, nil
		}
		beadID := m.ResolveBeadID
		m.ResolveActive = false
		m.ResolveInput = ""
		m.ResolveBeadID = ""
		m.ActionRunning = true
		m.ActionStatus = "Resolving..."
		m.ActionStatusTime = time.Now()
		resolveFn := m.ResolveFn
		return m, func() tea.Msg {
			var err error
			if resolveFn != nil {
				err = resolveFn(beadID, comment)
			} else {
				err = fmt.Errorf("resolve not available")
			}
			return resolveResultMsg{BeadID: beadID, Err: err}
		}
	case "backspace":
		if len(m.ResolveInput) > 0 {
			m.ResolveInput = m.ResolveInput[:len(m.ResolveInput)-1]
		}
		return m, nil
	case "ctrl+u":
		m.ResolveInput = ""
		return m, nil
	default:
		if len(msg.String()) == 1 && msg.String()[0] >= 32 {
			m.ResolveInput += msg.String()
		}
		return m, nil
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Confirmation dialog: absorb all keys.
		if m.ConfirmOpen {
			return m.updateConfirm(msg)
		}

		// Terminal pane mode: absorb all keys.
		if m.TermOpen {
			switch msg.String() {
			case "esc", "q":
				m.TermOpen = false
				m.TermLoading = false
				m.TermLines = nil
				m.TermScroll = 0
				return m, nil
			case "ctrl+c":
				m.Quitting = true
				return m, tea.Quit
			case "j", "down":
				m.TermScroll++
				viewportH := m.Height*85/100 - 4
				if viewportH < 10 {
					viewportH = 10
				}
				maxScroll := len(m.TermLines) - viewportH
				if maxScroll < 0 {
					maxScroll = 0
				}
				if m.TermScroll > maxScroll {
					m.TermScroll = maxScroll
				}
			case "k", "up":
				m.TermScroll--
				if m.TermScroll < 0 {
					m.TermScroll = 0
				}
			case "d":
				viewportH := m.Height*85/100 - 4
				if viewportH < 10 {
					viewportH = 10
				}
				m.TermScroll += viewportH / 2
				maxScroll := len(m.TermLines) - viewportH
				if maxScroll < 0 {
					maxScroll = 0
				}
				if m.TermScroll > maxScroll {
					m.TermScroll = maxScroll
				}
			case "u":
				viewportH := m.Height*85/100 - 4
				if viewportH < 10 {
					viewportH = 10
				}
				m.TermScroll -= viewportH / 2
				if m.TermScroll < 0 {
					m.TermScroll = 0
				}
			case "g":
				if m.PendingG {
					m.PendingG = false
					m.TermScroll = 0
				} else {
					m.PendingG = true
				}
				return m, nil
			case "G":
				m.PendingG = false
				viewportH := m.Height*85/100 - 4
				if viewportH < 10 {
					viewportH = 10
				}
				maxScroll := len(m.TermLines) - viewportH
				if maxScroll < 0 {
					maxScroll = 0
				}
				m.TermScroll = maxScroll
			case "r":
				if m.TermContentFn != nil && m.TermBeadID != "" {
					m.TermLoading = true
					contentWidth := m.Width * 9 / 10
					if contentWidth < 80 {
						contentWidth = 80
					}
					return m, fetchTermContentCmd(m.TermContentFn, m.TermBeadID, m.TermTitle, contentWidth)
				}
			}
			return m, nil
		}

		// Action menu mode: absorb all keys.
		if m.ActionMenuOpen {
			switch msg.String() {
			case "esc", "q":
				m.ActionMenuOpen = false
				return m, nil
			case "j", "down":
				if m.ActionMenuCursor < len(m.ActionMenuItems)-1 {
					m.ActionMenuCursor++
				}
				return m, nil
			case "k", "up":
				if m.ActionMenuCursor > 0 {
					m.ActionMenuCursor--
				}
				return m, nil
			case "enter":
				if m.ActionMenuCursor >= 0 && m.ActionMenuCursor < len(m.ActionMenuItems) {
					item := m.ActionMenuItems[m.ActionMenuCursor]
					m.ActionMenuOpen = false
					return m.dispatchMenuAction(item)
				}
				return m, nil
			default:
				// Check shortcut key match.
				for _, item := range m.ActionMenuItems {
					if msg.String() == string(item.Key) {
						m.ActionMenuOpen = false
						return m.dispatchMenuAction(item)
					}
				}
				return m, nil
			}
		}

		// Tower switcher overlay: absorb all keys.
		if m.TowerSwitcherOpen {
			switch msg.String() {
			case "esc", "q":
				m.TowerSwitcherOpen = false
				return m, nil
			case "j", "down":
				if m.TowerSwitcherCursor < len(m.TowerSwitcherItems)-1 {
					m.TowerSwitcherCursor++
				}
				return m, nil
			case "k", "up":
				if m.TowerSwitcherCursor > 0 {
					m.TowerSwitcherCursor--
				}
				return m, nil
			case "enter":
				if m.TowerSwitcherCursor >= 0 && m.TowerSwitcherCursor < len(m.TowerSwitcherItems) {
					selected := m.TowerSwitcherItems[m.TowerSwitcherCursor]
					m.TowerSwitcherOpen = false
					if selected.Name == m.Opts.TowerName {
						return m, nil // already on this tower
					}
					if m.Opts.SwitchTowerFn != nil {
						if name, err := m.Opts.SwitchTowerFn(selected.Name); err == nil {
							m.Opts.TowerName = name
							m.Snapshot = nil
							m.SelCol = 0
							m.SelCard = 0
							m.ColScroll = 0
							m.SelSection = SectionColumns
							return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
						}
					}
				}
				return m, nil
			}
			return m, nil
		}

		// Command mode: absorb all keys.
		if m.Cmdline.Active {
			return m.updateCmdline(msg)
		}

		// Search mode: absorb all keys.
		if m.SearchActive {
			switch msg.String() {
			case "esc":
				m.SearchActive = false
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			case "enter":
				m.SearchActive = false
				return m, nil
			case "backspace":
				if len(m.SearchQuery) > 0 {
					m.SearchQuery = m.SearchQuery[:len(m.SearchQuery)-1]
				}
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			case "ctrl+u":
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			default:
				// Append printable runes.
				if len(msg.String()) == 1 && msg.String()[0] >= 32 {
					m.SearchQuery += msg.String()
					m.SelCard = 0
					m.ColScroll = 0
					m.ClampSelection()
				}
				return m, nil
			}
		}

		// Inspector mode: handle keys differently.
		if m.Inspecting {
			// Feedback input mode: absorb all keys.
			if m.FeedbackActive {
				return m.updateFeedbackInput(msg)
			}

			// Resolve input mode: absorb all keys.
			if m.ResolveActive {
				return m.updateResolveInput(msg)
			}

			// Check if inspected bead is a design bead with needs-human.
			isReviewableDesign := false
			if m.InspectorData != nil {
				ib := m.InspectorData.Bead
				isReviewableDesign = ib.Type == "design" && ib.HasLabel("needs-human")
			}

			switch msg.String() {
			case "y":
				if isReviewableDesign {
					beadID := m.InspectorData.Bead.ID
					title := m.InspectorData.Bead.Title
					m.ConfirmOpen = true
					m.ConfirmAction = ActionApproveDesign
					m.ConfirmBeadID = beadID
					m.ConfirmPrompt = confirmPromptForAction(ActionApproveDesign, beadID, title)
					m.ConfirmDanger = DangerConfirm
					return m, nil
				}
				// Fallback: copy bead ID to clipboard.
				if m.InspectorData != nil {
					if err := copyToClipboard(m.InspectorData.Bead.ID); err != nil {
						m.ActionStatus = fmt.Sprintf("clipboard error: %v", err)
					} else {
						m.ActionStatus = fmt.Sprintf("copied: %s", m.InspectorData.Bead.ID)
					}
					m.ActionStatusTime = time.Now()
				}
			case "n":
				if isReviewableDesign {
					m.FeedbackActive = true
					m.FeedbackInput = ""
					m.FeedbackBeadID = m.InspectorData.Bead.ID
					return m, nil
				}
			case "esc", "q", "enter":
				m.Inspecting = false
				m.InspectorScroll = 0
				m.InspectorTab = 0
				m.InspectorData = nil
				m.InspectorLoading = false
			case "ctrl+c":
				m.Quitting = true
				return m, tea.Quit
			case "j", "down":
				m.InspectorScroll++
			case "k", "up":
				m.InspectorScroll--
				if m.InspectorScroll < 0 {
					m.InspectorScroll = 0
				}
			case "g":
				m.InspectorScroll = 0
			case "G":
				if m.InspectorData != nil {
					var dag *DAGProgress
					if m.Snapshot != nil {
						dag = m.Snapshot.DAGProgress[m.InspectorData.Bead.ID]
					}
					total := inspectorLineCountSnap(m.InspectorData, dag, m.Width, m.InspectorTab)
					maxVisible := m.Height - 2
					if maxVisible < 5 {
						maxVisible = 5
					}
					m.InspectorScroll = total - maxVisible
					if m.InspectorScroll < 0 {
						m.InspectorScroll = 0
					}
				}
			case "tab":
				m.InspectorTab++
				if m.InspectorTab > 1 {
					m.InspectorTab = 0
				}
				m.InspectorScroll = 0
			case "shift+tab":
				m.InspectorTab--
				if m.InspectorTab < 0 {
					m.InspectorTab = 1
				}
				m.InspectorScroll = 0
			}
			return m, nil
		}

		// Board mode: handle pending G for gg sequence.
		if m.PendingG {
			m.PendingG = false
			if msg.String() == "g" {
				m.SelCard = 0
				m.ColScroll = 0
				return m, nil
			}
			// Not gg — fall through to handle the key normally.
		}

		switch msg.String() {
		case "q", "ctrl+c", "esc":
			if m.SearchQuery != "" {
				m.SearchQuery = ""
				m.SelCard = 0
				m.ColScroll = 0
				m.ClampSelection()
				return m, nil
			}
			m.Quitting = true
			return m, tea.Quit

		// Open inspector on Enter or i.
		case "enter", "i":
			if bead := m.SelectedBead(); bead != nil {
				m.Inspecting = true
				m.InspectorScroll = 0
				m.InspectorTab = 0
				m.InspectorLoading = true
				m.InspectorData = nil
				return m, fetchInspectorCmd(*bead)
			}

		// Column navigation.
		case "h", "left":
			switch m.SelSection {
			case SectionLower:
				vis := m.VisibleCols()
				if m.SelLowerCol == 1 && len(vis.Blocked) > 0 {
					m.SelLowerCol = 0
					m.SelCard = 0
					m.ClampSelection()
				}
			case SectionColumns:
				m.SelCol--
				m.ColScroll = 0
				m.ClampSelection()
				m.ensureCardVisible(m.colMaxCards())
			}
		case "l", "right":
			switch m.SelSection {
			case SectionLower:
				vis := m.VisibleCols()
				if m.SelLowerCol == 0 && len(vis.Interrupted) > 0 {
					m.SelLowerCol = 1
					m.SelCard = 0
					m.ClampSelection()
				}
			case SectionColumns:
				m.SelCol++
				m.ColScroll = 0
				m.ClampSelection()
				m.ensureCardVisible(m.colMaxCards())
			}

		// Card navigation (within active view mode — no cross-section flow).
		case "j", "down":
			vis := m.VisibleCols()
			if m.SearchQuery != "" {
				vis = FilterColumns(vis, m.SearchQuery)
			}
			switch m.SelSection {
			case SectionAlerts:
				if m.SelCard+1 < len(vis.Alerts) {
					m.SelCard++
				}
			case SectionColumns:
				active := m.DisplayColumns()
				maxCard := 0
				if m.SelCol >= 0 && m.SelCol < len(active) {
					maxCard = len(active[m.SelCol].Beads)
				}
				if m.SelCard+1 < maxCard {
					m.SelCard++
					m.ClampSelection()
					m.ensureCardVisible(m.colMaxCards())
				}
			case SectionLower:
				var items []BoardBead
				if m.SelLowerCol == 0 {
					items = vis.Blocked
				} else {
					items = vis.Interrupted
				}
				if m.SelCard+1 < len(items) {
					m.SelCard++
				}
			}
		case "k", "up":
			switch m.SelSection {
			case SectionAlerts:
				if m.SelCard > 0 {
					m.SelCard--
				}
			case SectionColumns:
				if m.SelCard > 0 {
					m.SelCard--
					m.ClampSelection()
					m.ensureCardVisible(m.colMaxCards())
				}
			case SectionLower:
				if m.SelCard > 0 {
					m.SelCard--
				}
			}

		// Vim gg: first g sets PendingG.
		case "g":
			m.PendingG = true
			return m, nil

		// Vim G: go to bottom of current column.
		case "G":
			active := m.DisplayColumns()
			if m.SelSection == SectionColumns && m.SelCol >= 0 && m.SelCol < len(active) {
				lastCard := len(active[m.SelCol].Beads) - 1
				if lastCard < 0 {
					lastCard = 0
				}
				m.SelCard = lastCard
				m.ensureCardVisible(m.colMaxCards())
			}

		// Epic scoping toggle.
		case "e":
			if m.Opts.Epic != "" {
				m.Opts.Epic = ""
			} else if bead := m.SelectedBead(); bead != nil {
				if bead.Type == "epic" {
					m.Opts.Epic = bead.ID
				} else if bead.Parent != "" {
					m.Opts.Epic = bead.Parent
				}
			}
			m.ClampSelection()
			return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
		case "t":
			m.TypeScope = m.TypeScope.Next()
			m.ClampSelection()

		// Tower switcher.
		case "T":
			if m.Opts.ListTowersFn != nil {
				items := m.Opts.ListTowersFn()
				if len(items) > 1 {
					m.TowerSwitcherOpen = true
					m.TowerSwitcherCursor = 0
					m.TowerSwitcherItems = items
				}
			}
			return m, nil

		// Toggle showing all phase columns (including empty).
		case "H":
			m.ShowAllCols = !m.ShowAllCols
			m.ClampSelection()

		// Summon wizard — inline.
		case "s":
			if bead := m.SelectedBead(); bead != nil {
				mm, cmd := m.dispatchInlineAction(ActionSummon, bead.ID)
				return mm, cmd
			}

		// Unsummon wizard — confirm, then inline (only if bead has a wizard).
		case "u":
			if bead := m.SelectedBead(); bead != nil {
				for _, a := range m.Agents {
					if a.BeadID == bead.ID {
						m.ConfirmOpen = true
						m.ConfirmAction = ActionUnsummon
						m.ConfirmBeadID = bead.ID
						m.ConfirmPrompt = confirmPromptForAction(ActionUnsummon, bead.ID, bead.Title)
						m.ConfirmDanger = DangerConfirm
						return m, nil
					}
				}
			}

		// Resummon — inline.
		case "S":
			if bead := m.SelectedBead(); bead != nil && bead.HasLabel("needs-human") {
				mm, cmd := m.dispatchInlineAction(ActionResummon, bead.ID)
				return mm, cmd
			}

		// Reset — confirm, then inline.
		case "r":
			if bead := m.SelectedBead(); bead != nil && bead.Status == "in_progress" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionResetSoft
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionResetSoft, bead.ID, bead.Title)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Reset --hard — confirm, then inline.
		case "R":
			if bead := m.SelectedBead(); bead != nil && bead.Status == "in_progress" {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionResetHard
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionResetHard, bead.ID, bead.Title)
				m.ConfirmDanger = DangerDestructive
				return m, nil
			}

		// Close — confirm, then inline.
		case "x":
			if bead := m.SelectedBead(); bead != nil {
				m.ConfirmOpen = true
				m.ConfirmAction = ActionClose
				m.ConfirmBeadID = bead.ID
				m.ConfirmPrompt = confirmPromptForAction(ActionClose, bead.ID, bead.Title)
				m.ConfirmDanger = DangerConfirm
				return m, nil
			}

		// Defer/undefer toggle.
		case "d":
			if bead := m.SelectedBead(); bead != nil {
				// Only toggle on ready (open) or deferred beads.
				if bead.Status == "open" || bead.Status == "deferred" {
					mm, cmd := m.dispatchInlineAction(ActionDefer, bead.ID)
					return mm, cmd
				}
			}

		// Action menu.
		case "a":
			if bead := m.SelectedBead(); bead != nil {
				m.ActionMenuBeadID = bead.ID
				m.ActionMenuBeadTitle = bead.Title
				m.ActionMenuItems = BuildActionMenu(bead, m.Agents)
				m.ActionMenuCursor = 0
				m.ActionMenuOpen = true
				return m, nil
			}

		// Command mode.
		case ":":
			m.Cmdline = CmdlineState{Active: true, History: m.Cmdline.History, HistIdx: -1, CompIdx: -1}
			return m, nil

		// Copy bead ID to clipboard.
		case "y":
			if bead := m.SelectedBead(); bead != nil {
				if err := copyToClipboard(bead.ID); err != nil {
					m.ActionStatus = fmt.Sprintf("clipboard error: %v", err)
				} else {
					m.ActionStatus = fmt.Sprintf("copied: %s", bead.ID)
				}
				m.ActionStatusTime = time.Now()
			}
			return m, nil

		// Cycle view mode.
		case "tab":
			m.ViewMode++
			if m.ViewMode > ViewLower {
				m.ViewMode = ViewBoard
			}
			m.SelCard = 0
			m.ColScroll = 0
			m.ClampSelection()
			return m, nil
		case "shift+tab":
			if m.ViewMode == ViewBoard {
				m.ViewMode = ViewLower
			} else {
				m.ViewMode--
			}
			m.SelCard = 0
			m.ColScroll = 0
			m.ClampSelection()
			return m, nil

		// Search.
		case "/":
			m.SearchActive = true
			m.SearchQuery = ""
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		if m.SelSection == SectionColumns {
			m.ensureCardVisible(m.colMaxCards())
		}
	case tickMsg:
		m.LastTick = time.Now()
		// Auto-clear action status after 5 seconds.
		if m.ActionStatus != "" && time.Since(m.ActionStatusTime) > 5*time.Second {
			m.ActionStatus = ""
		}
		if !m.Inspecting {
			return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
		}
		return m, tickCmd(m.Opts.Interval)
	case snapshotMsg:
		if msg.Err == nil && msg.Snap != nil {
			m.Snapshot = msg.Snap
			m.Cols = msg.Snap.Columns
			m.Agents = msg.Snap.Agents
		}
		if !m.Inspecting {
			m.ClampSelection()
		}
		return m, tickCmd(m.Opts.Interval)
	case termContentMsg:
		if msg.Err != nil {
			m.TermLoading = false
			m.TermLines = []string{"Error: " + msg.Err.Error()}
			return m, nil
		}
		m.TermLoading = false
		m.TermTitle = msg.Title
		m.TermBeadID = msg.BeadID
		m.TermLines = strings.Split(msg.Content, "\n")
		m.TermScroll = 0
		return m, nil
	case inspectorDataMsg:
		if msg.Err == nil && msg.Data != nil {
			m.InspectorData = msg.Data
		}
		m.InspectorLoading = false
		return m, nil
	case actionResultMsg:
		m.ActionRunning = false
		if msg.Err != nil {
			m.ActionStatus = fmt.Sprintf("%s failed: %v", actionLabel(msg.Action), msg.Err)
		} else {
			m.ActionStatus = fmt.Sprintf("%s: done", actionLabel(msg.Action))
		}
		m.ActionStatusTime = time.Now()
		// Refresh board data after action completes.
		return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
	case rejectDesignResultMsg:
		m.ActionRunning = false
		if msg.Err != nil {
			m.ActionStatus = fmt.Sprintf("Reject design failed: %v", msg.Err)
		} else {
			m.ActionStatus = "Design rejected"
		}
		m.ActionStatusTime = time.Now()
		return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
	case resolveResultMsg:
		m.ActionRunning = false
		if msg.Err != nil {
			m.ActionStatus = fmt.Sprintf("Resolve failed: %v", msg.Err)
		} else {
			m.ActionStatus = "Resolved"
		}
		m.ActionStatusTime = time.Now()
		return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
	case cmdlineDoneMsg:
		m.ActionRunning = false
		if msg.err != nil {
			m.ActionStatus = fmt.Sprintf("Error: %v", msg.err)
		} else if msg.output != "" {
			// Truncate to first line for status display.
			line := msg.output
			if idx := strings.Index(line, "\n"); idx >= 0 {
				line = line[:idx]
			}
			if len(line) > 80 {
				line = line[:77] + "..."
			}
			m.ActionStatus = line
		} else {
			m.ActionStatus = "Done"
		}
		m.ActionStatusTime = time.Now()
		return m, fetchSnapshotCmd(m.Opts, m.Identity, m.FetchAgentsFn)
	}
	return m, nil
}
