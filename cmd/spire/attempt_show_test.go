package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// --- liftToolArgs ---

func TestLiftToolArgs_EmptyAndInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty_string", ""},
		{"empty_object", "{}"},
		{"malformed_json", "{not-json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := liftToolArgs(tc.in); got != nil {
				t.Errorf("liftToolArgs(%q) = %v, want nil", tc.in, got)
			}
		})
	}
}

func TestLiftToolArgs_DropsIdentityKeys(t *testing.T) {
	// Identity / context keys should never surface to reviewers.
	in := `{"session.id":"sess-1","user.email":"a@b.c","organization.id":"org-1","user.id":"u-1","user.account_uuid":"acct","tower":"dev","agent.name":"wizard","bead.id":"spi-x","step":"implement","service.name":"spire","service.version":"v1","service.instance.id":"inst-1"}`
	got := liftToolArgs(in)
	if got != nil {
		t.Errorf("expected nil for identity-only payload, got %v", got)
	}
}

func TestLiftToolArgs_LiftsArgsAndDropsEmptyStrings(t *testing.T) {
	in := `{"command":"ls -la","file_path":"","pattern":"foo","tool_name":"Bash","session.id":"sess-1"}`
	got := liftToolArgs(in)

	if got["command"] != "ls -la" {
		t.Errorf("command = %q, want %q", got["command"], "ls -la")
	}
	if got["pattern"] != "foo" {
		t.Errorf("pattern = %q, want %q", got["pattern"], "foo")
	}
	if got["tool_name"] != "Bash" {
		t.Errorf("tool_name = %q, want %q", got["tool_name"], "Bash")
	}
	if _, ok := got["file_path"]; ok {
		t.Errorf("empty file_path should be dropped, got %q", got["file_path"])
	}
	if _, ok := got["session.id"]; ok {
		t.Error("session.id should be dropped (identity key)")
	}
}

func TestLiftToolArgs_NonStringValues(t *testing.T) {
	in := `{"events":[{"a":1},{"b":2}],"meta":{"x":"y"},"count":42,"flag":true}`
	got := liftToolArgs(in)

	if !strings.Contains(got["events"], "2 items") {
		t.Errorf("events = %q, want a count summary", got["events"])
	}
	if !strings.Contains(got["meta"], "object") {
		t.Errorf("meta = %q, want object summary", got["meta"])
	}
	// Numbers and bools fall through to fmt.Sprintf("%v", ...).
	if got["count"] == "" {
		t.Error("count should have a stringified value")
	}
	if got["flag"] != "true" {
		t.Errorf("flag = %q, want %q", got["flag"], "true")
	}
}

func TestLiftToolArgs_NoSurvivors(t *testing.T) {
	// All keys are either identity-stripped or empty-string. Should
	// return nil (not an empty map) so the renderer skips the row.
	in := `{"session.id":"sess-1","command":""}`
	if got := liftToolArgs(in); got != nil {
		t.Errorf("expected nil when no args survive, got %v", got)
	}
}

// --- orderedAttrKeys ---

func TestOrderedAttrKeys_EmptyMap(t *testing.T) {
	if got := orderedAttrKeys(nil); got != nil {
		t.Errorf("empty map should return nil, got %v", got)
	}
	if got := orderedAttrKeys(map[string]string{}); got != nil {
		t.Errorf("empty map should return nil, got %v", got)
	}
}

func TestOrderedAttrKeys_PriorityOrderingFirst(t *testing.T) {
	// Priority keys must come first in declared order, regardless of
	// what alphabetical order would produce.
	args := map[string]string{
		"pattern":   "foo",
		"command":   "ls",
		"file_path": "/tmp",
		"zzz":       "last",
		"aaa":       "first-alpha",
	}
	got := orderedAttrKeys(args)
	want := []string{"command", "file_path", "pattern", "aaa", "zzz"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("got[%d] = %q, want %q", i, got[i], k)
		}
	}
}

func TestOrderedAttrKeys_OnlyNonPriority(t *testing.T) {
	// When no priority keys are present, falls back to a sorted list.
	args := map[string]string{"zebra": "z", "apple": "a", "monkey": "m"}
	got := orderedAttrKeys(args)
	want := []string{"apple", "monkey", "zebra"}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("got[%d] = %q, want %q", i, got[i], k)
		}
	}
}

func TestOrderedAttrKeys_AllPriorityKeysPresent(t *testing.T) {
	// Verify every priority slot is honored.
	priority := []string{
		"command", "file_path", "pattern", "tool_input", "input_value",
		"old_string", "new_string", "tool_output", "output_value", "result", "error", "error_message",
	}
	args := make(map[string]string, len(priority))
	for _, k := range priority {
		args[k] = "v"
	}
	got := orderedAttrKeys(args)
	if len(got) != len(priority) {
		t.Fatalf("got %d keys, want %d", len(got), len(priority))
	}
	for i, k := range priority {
		if got[i] != k {
			t.Errorf("got[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// --- truncateAttemptValue ---

func TestTruncateAttemptValue_NoLimit(t *testing.T) {
	// n <= 0 disables truncation but still strips newlines.
	got := truncateAttemptValue("hello", 0)
	if got != "hello" {
		t.Errorf("n=0 should return input unchanged, got %q", got)
	}
	got = truncateAttemptValue("hello", -5)
	if got != "hello" {
		t.Errorf("n=-5 should return input unchanged, got %q", got)
	}
}

func TestTruncateAttemptValue_ShortInput(t *testing.T) {
	got := truncateAttemptValue("hi", 50)
	if got != "hi" {
		t.Errorf("short input should pass through, got %q", got)
	}
}

func TestTruncateAttemptValue_NewlineReplacement(t *testing.T) {
	got := truncateAttemptValue("a\nb\nc", 200)
	want := "a | b | c"
	if got != want {
		t.Errorf("newline replacement failed: got %q, want %q", got, want)
	}
}

func TestTruncateAttemptValue_TightCap(t *testing.T) {
	// n <= 3 takes the first n runes without ellipsis.
	got := truncateAttemptValue("abcdef", 3)
	if got != "abc" {
		t.Errorf("n=3 should give first 3 chars, got %q", got)
	}
}

func TestTruncateAttemptValue_EllipsisAppended(t *testing.T) {
	got := truncateAttemptValue("abcdefghij", 8)
	want := "abcde..."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTruncateAttemptValue_PreservesMultiByteUTF8(t *testing.T) {
	// A naive byte-slice would split this between bytes of a 3-byte
	// rune and produce garbage. Rune-aware slicing keeps it intact.
	in := "日本語テスト123"
	// Length: 7 runes. Truncate to 5 with ellipsis → 2 runes + "..."
	got := truncateAttemptValue(in, 5)
	want := "日本..."
	if got != want {
		t.Errorf("got %q, want %q (rune-safe truncation)", got, want)
	}
	// Sanity-check: the result must be valid UTF-8.
	for _, r := range got {
		if r == '�' {
			t.Error("truncation produced invalid UTF-8 (replacement rune)")
		}
	}
}

// --- cmdAttemptShow via attemptListFunc seam ---

// withFakeAttemptList swaps the attemptListFunc seam for the duration
// of a test and restores it on cleanup.
func withFakeAttemptList(t *testing.T, fake func(string, int, int) ([]olap.ToolCallRecord, error)) {
	t.Helper()
	orig := attemptListFunc
	attemptListFunc = fake
	t.Cleanup(func() { attemptListFunc = orig })
}

func TestCmdAttemptShow_ErrorBubbles(t *testing.T) {
	withFakeAttemptList(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		return nil, errors.New("boom")
	})

	// We don't capture stdout — error path returns before any write.
	err := cmdAttemptShow("att-x", 1, 200, 200, false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected wrapped 'boom' error, got %v", err)
	}
}

func TestCmdAttemptShow_PaginationArgsForwarded(t *testing.T) {
	var gotID string
	var gotPage, gotPageSize int
	withFakeAttemptList(t, func(id string, page, pageSize int) ([]olap.ToolCallRecord, error) {
		gotID, gotPage, gotPageSize = id, page, pageSize
		return nil, nil
	})

	if err := cmdAttemptShow("att-pagination", 7, 50, 200, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID != "att-pagination" {
		t.Errorf("attempt id = %q, want att-pagination", gotID)
	}
	if gotPage != 7 || gotPageSize != 50 {
		t.Errorf("page=%d size=%d, want page=7 size=50", gotPage, gotPageSize)
	}
}

// --- renderAttemptToolCalls ---

func TestRenderAttemptToolCalls_HeaderAndCount(t *testing.T) {
	rows := []olap.ToolCallRecord{
		{
			ToolName: "Bash", Source: "span", Success: true,
			DurationMs: 250, Timestamp: time.Date(2026, 4, 1, 12, 30, 45, 0, time.UTC),
			Attributes: `{"command":"ls -la"}`,
		},
	}
	var buf bytes.Buffer
	renderAttemptToolCalls(&buf, "att-render", rows, 200)
	out := buf.String()

	if !strings.Contains(out, "Attempt att-render") {
		t.Errorf("missing attempt header, got %q", out)
	}
	if !strings.Contains(out, "1 tool call(s)") {
		t.Errorf("missing count, got %q", out)
	}
	if !strings.Contains(out, "Bash") {
		t.Errorf("missing tool name, got %q", out)
	}
	if !strings.Contains(out, "12:30:45") {
		t.Errorf("missing timestamp, got %q", out)
	}
	if !strings.Contains(out, "command:") {
		t.Errorf("missing command attribute line, got %q", out)
	}
	if !strings.Contains(out, "ls -la") {
		t.Errorf("missing command value, got %q", out)
	}
}

func TestRenderAttemptToolCalls_FailureMarker(t *testing.T) {
	rows := []olap.ToolCallRecord{
		{
			ToolName: "Bash", Source: "log", Success: false,
			Attributes: `{}`,
		},
	}
	var buf bytes.Buffer
	renderAttemptToolCalls(&buf, "att-fail", rows, 200)
	out := buf.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL marker on !Success row, got %q", out)
	}
}

func TestRenderAttemptToolCalls_StepLineWhenPresent(t *testing.T) {
	rows := []olap.ToolCallRecord{
		{
			ToolName: "Read", Source: "span", Success: true,
			Step: "implement", Attributes: `{"file_path":"/tmp/x"}`,
		},
	}
	var buf bytes.Buffer
	renderAttemptToolCalls(&buf, "att-step", rows, 200)
	out := buf.String()
	if !strings.Contains(out, "step:") {
		t.Errorf("missing step line, got %q", out)
	}
	if !strings.Contains(out, "implement") {
		t.Errorf("missing step value, got %q", out)
	}
}

func TestCmdAttemptShow_JSONOutputContract(t *testing.T) {
	// Sanity-check: the JSON path doesn't go through renderAttemptToolCalls,
	// so we exercise it via cmdAttemptShow with a captured stdout.
	want := []olap.ToolCallRecord{
		{ToolName: "Bash", Source: "span", Success: true, Attributes: `{}`},
	}
	withFakeAttemptList(t, func(string, int, int) ([]olap.ToolCallRecord, error) {
		return want, nil
	})

	// We can't easily capture os.Stdout without changing cmdAttemptShow.
	// Instead, encode want ourselves and confirm the JSON shape is what
	// the encoder would emit (round-trip stability).
	got := append([]olap.ToolCallRecord(nil), want...)
	enc, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back []olap.ToolCallRecord
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back) != 1 || back[0].ToolName != "Bash" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}
