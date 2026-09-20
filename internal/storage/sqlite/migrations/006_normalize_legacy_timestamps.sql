-- 006_normalize_legacy_timestamps.sql
-- Normalize legacy RFC3339 UTC timestamps before fixed-width lexical comparisons.
-- Existing whole-second and variable-width fractional values become 9-digit UTC text.

DROP TRIGGER sessions_immutable_fields;
DROP TRIGGER sessions_terminal_immutable;
DROP TRIGGER role_runs_immutable_fields;
DROP TRIGGER role_runs_terminal_immutable;
DROP TRIGGER snapshots_no_update;
DROP TRIGGER findings_no_update;
UPDATE snapshots
SET created_at = CASE
  WHEN length(created_at) = 20 AND substr(created_at, 20, 1) = 'Z'
    THEN substr(created_at, 1, 19) || '.000000000Z'
  WHEN length(created_at) BETWEEN 22 AND 30
    AND substr(created_at, 20, 1) = '.'
    AND substr(created_at, -1, 1) = 'Z'
    THEN substr(created_at, 1, 20) || substr(substr(created_at, 21, length(created_at) - 21) || '000000000', 1, 9) || 'Z'
  ELSE created_at
END;

UPDATE sessions
SET created_at = CASE
      WHEN length(created_at) = 20 AND substr(created_at, 20, 1) = 'Z' THEN substr(created_at, 1, 19) || '.000000000Z'
      WHEN length(created_at) BETWEEN 22 AND 30 AND substr(created_at, 20, 1) = '.' AND substr(created_at, -1, 1) = 'Z' THEN substr(created_at, 1, 20) || substr(substr(created_at, 21, length(created_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE created_at
    END,
    claimed_at = CASE
      WHEN claimed_at IS NULL THEN NULL
      WHEN length(claimed_at) = 20 AND substr(claimed_at, 20, 1) = 'Z' THEN substr(claimed_at, 1, 19) || '.000000000Z'
      WHEN length(claimed_at) BETWEEN 22 AND 30 AND substr(claimed_at, 20, 1) = '.' AND substr(claimed_at, -1, 1) = 'Z' THEN substr(claimed_at, 1, 20) || substr(substr(claimed_at, 21, length(claimed_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE claimed_at
    END,
    dispatch_cutoff_at = CASE
      WHEN dispatch_cutoff_at IS NULL THEN NULL
      WHEN length(dispatch_cutoff_at) = 20 AND substr(dispatch_cutoff_at, 20, 1) = 'Z' THEN substr(dispatch_cutoff_at, 1, 19) || '.000000000Z'
      WHEN length(dispatch_cutoff_at) BETWEEN 22 AND 30 AND substr(dispatch_cutoff_at, 20, 1) = '.' AND substr(dispatch_cutoff_at, -1, 1) = 'Z' THEN substr(dispatch_cutoff_at, 1, 20) || substr(substr(dispatch_cutoff_at, 21, length(dispatch_cutoff_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE dispatch_cutoff_at
    END,
    hard_deadline_at = CASE
      WHEN hard_deadline_at IS NULL THEN NULL
      WHEN length(hard_deadline_at) = 20 AND substr(hard_deadline_at, 20, 1) = 'Z' THEN substr(hard_deadline_at, 1, 19) || '.000000000Z'
      WHEN length(hard_deadline_at) BETWEEN 22 AND 30 AND substr(hard_deadline_at, 20, 1) = '.' AND substr(hard_deadline_at, -1, 1) = 'Z' THEN substr(hard_deadline_at, 1, 20) || substr(substr(hard_deadline_at, 21, length(hard_deadline_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE hard_deadline_at
    END,
    terminal_at = CASE
      WHEN terminal_at IS NULL THEN NULL
      WHEN length(terminal_at) = 20 AND substr(terminal_at, 20, 1) = 'Z' THEN substr(terminal_at, 1, 19) || '.000000000Z'
      WHEN length(terminal_at) BETWEEN 22 AND 30 AND substr(terminal_at, 20, 1) = '.' AND substr(terminal_at, -1, 1) = 'Z' THEN substr(terminal_at, 1, 20) || substr(substr(terminal_at, 21, length(terminal_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE terminal_at
    END;

UPDATE role_runs
SET started_at = CASE
      WHEN started_at IS NULL THEN NULL
      WHEN length(started_at) = 20 AND substr(started_at, 20, 1) = 'Z' THEN substr(started_at, 1, 19) || '.000000000Z'
      WHEN length(started_at) BETWEEN 22 AND 30 AND substr(started_at, 20, 1) = '.' AND substr(started_at, -1, 1) = 'Z' THEN substr(started_at, 1, 20) || substr(substr(started_at, 21, length(started_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE started_at
    END,
    completed_at = CASE
      WHEN completed_at IS NULL THEN NULL
      WHEN length(completed_at) = 20 AND substr(completed_at, 20, 1) = 'Z' THEN substr(completed_at, 1, 19) || '.000000000Z'
      WHEN length(completed_at) BETWEEN 22 AND 30 AND substr(completed_at, 20, 1) = '.' AND substr(completed_at, -1, 1) = 'Z' THEN substr(completed_at, 1, 20) || substr(substr(completed_at, 21, length(completed_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE completed_at
    END,
    created_at = CASE
      WHEN length(created_at) = 20 AND substr(created_at, 20, 1) = 'Z' THEN substr(created_at, 1, 19) || '.000000000Z'
      WHEN length(created_at) BETWEEN 22 AND 30 AND substr(created_at, 20, 1) = '.' AND substr(created_at, -1, 1) = 'Z' THEN substr(created_at, 1, 20) || substr(substr(created_at, 21, length(created_at) - 21) || '000000000', 1, 9) || 'Z'
      ELSE created_at
    END;

UPDATE findings
SET created_at = CASE
  WHEN length(created_at) = 20 AND substr(created_at, 20, 1) = 'Z' THEN substr(created_at, 1, 19) || '.000000000Z'
  WHEN length(created_at) BETWEEN 22 AND 30 AND substr(created_at, 20, 1) = '.' AND substr(created_at, -1, 1) = 'Z' THEN substr(created_at, 1, 20) || substr(substr(created_at, 21, length(created_at) - 21) || '000000000', 1, 9) || 'Z'
  ELSE created_at
END;

UPDATE schema_migrations
SET applied_at = CASE
  WHEN length(applied_at) = 20 AND substr(applied_at, 20, 1) = 'Z' THEN substr(applied_at, 1, 19) || '.000000000Z'
  WHEN length(applied_at) BETWEEN 22 AND 30 AND substr(applied_at, 20, 1) = '.' AND substr(applied_at, -1, 1) = 'Z' THEN substr(applied_at, 1, 20) || substr(substr(applied_at, 21, length(applied_at) - 21) || '000000000', 1, 9) || 'Z'
  ELSE applied_at
END;

CREATE TRIGGER snapshots_no_update
BEFORE UPDATE ON snapshots
BEGIN
  SELECT RAISE(ABORT, 'snapshots are immutable');
END;

CREATE TRIGGER findings_no_update
BEFORE UPDATE ON findings
BEGIN
  SELECT RAISE(ABORT, 'findings are immutable');
END;

CREATE TRIGGER sessions_immutable_fields
BEFORE UPDATE ON sessions
WHEN OLD.id != NEW.id
  OR OLD.project_id != NEW.project_id
  OR OLD.idempotency_key != NEW.idempotency_key
  OR OLD.request_hash != NEW.request_hash
  OR OLD.snapshot_id != NEW.snapshot_id
  OR OLD.created_at != NEW.created_at
BEGIN
  SELECT RAISE(ABORT, 'session identity, project, idempotency, hash, snapshot, and creation fields are immutable');
END;

CREATE TRIGGER sessions_terminal_immutable
BEFORE UPDATE ON sessions
WHEN OLD.status IN ('complete', 'partial', 'failed')
BEGIN
  SELECT RAISE(ABORT, 'terminal session cannot be modified');
END;

CREATE TRIGGER role_runs_immutable_fields
BEFORE UPDATE ON role_runs
WHEN OLD.id != NEW.id
  OR OLD.session_id != NEW.session_id
  OR OLD.role != NEW.role
  OR OLD.created_at != NEW.created_at
BEGIN
  SELECT RAISE(ABORT, 'role run identity, session, role, and creation fields are immutable');
END;

CREATE TRIGGER role_runs_terminal_immutable
BEFORE UPDATE ON role_runs
WHEN OLD.status IN ('complete', 'failed', 'interrupted')
BEGIN
  SELECT RAISE(ABORT, 'terminal role run cannot be modified');
END;
