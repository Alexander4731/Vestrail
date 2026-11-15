-- Existing findings retain an explicit legacy label. Only subsequent inserts
-- receive candidate; repeating startup must never relabel historical rows.
ALTER TABLE findings ADD COLUMN IF NOT EXISTS verification_state TEXT NOT NULL DEFAULT 'legacy_unverified';
ALTER TABLE findings ALTER COLUMN verification_state SET DEFAULT 'candidate';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS verification_assurance TEXT NOT NULL DEFAULT 'none';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS verification_evidence_version BIGINT NOT NULL DEFAULT -1;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS verification_attempt_id BIGINT;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS status_revision BIGINT NOT NULL DEFAULT 0;

CREATE OR REPLACE FUNCTION bump_finding_status_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN NEW.status_revision := OLD.status_revision + 1; END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER trg_finding_status_revision BEFORE UPDATE OF status ON findings
FOR EACH ROW EXECUTE FUNCTION bump_finding_status_revision();

-- A changed claim invalidates its prior proof just as a changed traffic binding
-- does. Report prose and manual triage do not change the evidence being tested.
CREATE OR REPLACE FUNCTION bump_finding_claim_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.vulnclass,NEW.summary,NEW.evidence,NEW.asset_ids) IS DISTINCT FROM
       ROW(OLD.vulnclass,OLD.summary,OLD.evidence,OLD.asset_ids) THEN
        NEW.evidence_version := OLD.evidence_version + 1;
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER trg_finding_claim_version BEFORE UPDATE OF vulnclass,summary,evidence,asset_ids ON findings
FOR EACH ROW EXECUTE FUNCTION bump_finding_claim_version();

CREATE TABLE IF NOT EXISTS finding_verification_attempts (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    request_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    evidence_version BIGINT NOT NULL,
    verifier TEXT NOT NULL,
    verifier_version INTEGER NOT NULL DEFAULT 1,
    actor TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'running' CHECK(state IN ('running','completed','interrupted')),
    outcome TEXT NOT NULL DEFAULT 'inconclusive' CHECK(outcome IN ('supported','not_supported','inconclusive')),
    assurance TEXT NOT NULL DEFAULT 'none' CHECK(assurance IN ('none','evidence_bound','semantic_verified')),
    reason TEXT NOT NULL DEFAULT '',
    evidence_refs JSONB NOT NULL DEFAULT '[]',
    limitations JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    UNIQUE(finding_id,request_key)
);
CREATE INDEX IF NOT EXISTS idx_finding_verification_history ON finding_verification_attempts(finding_id,id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_verification_running ON finding_verification_attempts(finding_id) WHERE state='running';

ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS assessment_method TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE finding_retests ALTER COLUMN assessment_method SET DEFAULT 'model_assessed';
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS baseline_version BIGINT NOT NULL DEFAULT -1;
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS status_revision BIGINT NOT NULL DEFAULT -1;
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS application_state TEXT NOT NULL DEFAULT 'not_applied';
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS applied_by TEXT NOT NULL DEFAULT '';
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS application_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE finding_retests ADD COLUMN IF NOT EXISTS applied_at TIMESTAMPTZ;
