CREATE TABLE IF NOT EXISTS coverage_plans (
    id BIGSERIAL PRIMARY KEY,
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    request_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    scope_revision TEXT NOT NULL,
    scope_snapshot JSONB NOT NULL,
    actor TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(task_id,revision),
    UNIQUE(task_id,request_key)
);
CREATE TABLE IF NOT EXISTS coverage_checks (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL REFERENCES coverage_plans(id) ON DELETE CASCADE,
    target_ref TEXT NOT NULL,
    principal_ref TEXT NOT NULL,
    category TEXT NOT NULL,
    test_id TEXT NOT NULL,
    precondition_version INTEGER NOT NULL,
    prerequisites TEXT NOT NULL,
    UNIQUE(plan_id,target_ref,principal_ref,category,test_id,precondition_version)
);
CREATE TABLE IF NOT EXISTS coverage_attempts (
    id BIGSERIAL PRIMARY KEY,
    check_id BIGINT NOT NULL REFERENCES coverage_checks(id) ON DELETE CASCADE,
    request_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    actor TEXT NOT NULL,
    authority TEXT NOT NULL CHECK(authority IN ('agent_reported','human_reported','machine_observed')),
    state TEXT NOT NULL CHECK(state IN ('not_run','running','observed_no_issue','finding','blocked','not_applicable','needs_review')),
    reported_outcome TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence_node_id BIGINT,
    evidence_revision BIGINT,
    evidence_snapshot JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(check_id,request_key)
);
CREATE INDEX IF NOT EXISTS idx_coverage_attempts_history ON coverage_attempts(check_id,id DESC);
