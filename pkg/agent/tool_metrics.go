package agent

// Tool metrics are now collected via the OTel pipeline (pkg/otel). The daemon
// runs a lightweight OTLP gRPC receiver that captures tool invocation spans
// directly from Claude Code and Codex, writing them to the tool_events table
// in DuckDB. The PostToolUse hooks approach is removed.
//
// These stub functions exist for backward compatibility with callers that
// haven't been migrated yet. They are intentional no-ops.

// SetupToolMetrics is a no-op. Tool metrics are now collected via OTel.
// Kept for backward compatibility with callers that haven't been migrated.
func SetupToolMetrics(providerName, worktreePath string) error {
	return nil
}

// CollectToolMetrics is a no-op. Tool metrics are now in DuckDB via OTel.
// Kept for backward compatibility with callers that haven't been migrated.
func CollectToolMetrics(providerName, worktreePath string) (map[string]int, error) {
	return nil, nil
}
