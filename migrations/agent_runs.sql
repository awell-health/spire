CREATE TABLE IF NOT EXISTS agent_runs (
    id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    epic_id VARCHAR(64),
    agent_name VARCHAR(128),
    model VARCHAR(64) NOT NULL,
    role VARCHAR(16) NOT NULL,  -- 'worker' or 'wizard'

    -- Execution metrics
    context_tokens_in INT,
    context_tokens_out INT,
    total_tokens INT,
    turns INT,
    cost_usd DOUBLE,
    duration_seconds INT,
    startup_seconds INT,     -- pod start → claude start (clone, install, claim, focus)
    working_seconds INT,     -- claude start → claude done (the actual LLM work)
    queue_seconds INT,       -- bead filed → wizard assigned (time waiting in READY)
    review_seconds INT,      -- branch pushed → review verdict (time in review)
    result VARCHAR(32) NOT NULL,  -- success, test_failure, review_rejected, timeout, error, stopped

    -- Review metrics
    review_rounds INT DEFAULT 0,
    artificer_verdict VARCHAR(32),  -- legacy column name; actual meaning is review_verdict (approve, request_changes, reject)

    -- Spec context
    spec_file VARCHAR(256),
    spec_size_tokens INT,
    focus_context_tokens INT,

    -- Code metrics
    files_changed INT,
    lines_added INT,
    lines_removed INT,
    tests_added INT,
    tests_passed BOOLEAN,

    -- Prompt capture
    system_prompt_hash VARCHAR(64),
    golden_run BOOLEAN DEFAULT FALSE,

    -- Timestamps
    started_at DATETIME NOT NULL,
    completed_at DATETIME,

    INDEX idx_bead (bead_id),
    INDEX idx_epic (epic_id),
    INDEX idx_result (result),
    INDEX idx_golden (golden_run),
    INDEX idx_model (model)
);

CREATE TABLE IF NOT EXISTS golden_prompts (
    run_id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    system_prompt TEXT,
    spec_excerpt TEXT,
    focus_context TEXT,
    tags JSON,
    context_tokens INT,
    CONSTRAINT fk_run FOREIGN KEY (run_id) REFERENCES agent_runs(id)
);
