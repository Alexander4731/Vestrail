CREATE TABLE IF NOT EXISTS goal_assessments (
    id BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    goal_id BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    goal_revision BIGINT NOT NULL,
    goal_snapshot JSONB NOT NULL,
    evidence_id BIGINT NOT NULL,
    evidence_exploration_id BIGINT NOT NULL,
    evidence_revision BIGINT NOT NULL,
    evidence_snapshot JSONB NOT NULL,
    evidence_sha256 TEXT NOT NULL,
    method TEXT NOT NULL CHECK(method IN ('model_assessed','human_accepted','machine_checked')),
    actor TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_goal_assessments_latest ON goal_assessments(exploration_id,goal_id,id DESC);
