ALTER TABLE agent_runs ADD COLUMN phase_bucket VARCHAR(20) NULL;

UPDATE agent_runs SET phase_bucket = CASE
    WHEN phase IN ('implement', 'build-fix')                                              THEN 'implement'
    WHEN phase IN ('review', 'review-fix')                                                THEN 'review'
    WHEN phase IN ('validate-design', 'enrich-subtasks', 'auto-approve', 'skip', 'waitForHuman') THEN 'design'
    ELSE NULL
END
WHERE phase IS NOT NULL;
