-- 003_state_guards.sql
-- State-transition, cancellation monotonicity, immutability, and citation guards for SpecCouncil persistence.

-- 1. Immutability guards for static entities: snapshots, evidence_units, findings, finding_basis_refs.

CREATE TRIGGER snapshots_no_update
BEFORE UPDATE ON snapshots
BEGIN
  SELECT RAISE(ABORT, 'snapshots are immutable');
END;

CREATE TRIGGER snapshots_no_delete
BEFORE DELETE ON snapshots
BEGIN
  SELECT RAISE(ABORT, 'snapshots are immutable');
END;

CREATE TRIGGER evidence_units_no_update
BEFORE UPDATE ON evidence_units
BEGIN
  SELECT RAISE(ABORT, 'evidence units are immutable');
END;

CREATE TRIGGER evidence_units_no_delete
BEFORE DELETE ON evidence_units
BEGIN
  SELECT RAISE(ABORT, 'evidence units are immutable');
END;

CREATE TRIGGER findings_no_update
BEFORE UPDATE ON findings
BEGIN
  SELECT RAISE(ABORT, 'findings are immutable');
END;

CREATE TRIGGER findings_no_delete
BEFORE DELETE ON findings
BEGIN
  SELECT RAISE(ABORT, 'findings are immutable');
END;

CREATE TRIGGER finding_basis_refs_no_update
BEFORE UPDATE ON finding_basis_refs
BEGIN
  SELECT RAISE(ABORT, 'finding basis references are immutable');
END;

CREATE TRIGGER finding_basis_refs_no_delete
BEFORE DELETE ON finding_basis_refs
BEGIN
  SELECT RAISE(ABORT, 'finding basis references are immutable');
END;

-- 2. Immutability of identity, hash, foreign key, and creation fields for sessions and role_runs.

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

CREATE TRIGGER role_runs_immutable_fields
BEFORE UPDATE ON role_runs
WHEN OLD.id != NEW.id
  OR OLD.session_id != NEW.session_id
  OR OLD.role != NEW.role
  OR OLD.created_at != NEW.created_at
BEGIN
  SELECT RAISE(ABORT, 'role run identity, session, role, and creation fields are immutable');
END;

-- 3. Cancellation monotonicity guard: cancel_requested may transition only from 0 to 1.

CREATE TRIGGER sessions_cancel_monotonic
BEFORE UPDATE ON sessions
WHEN OLD.cancel_requested = 1 AND NEW.cancel_requested != 1
BEGIN
  SELECT RAISE(ABORT, 'cancel_requested is monotonic and cannot be reset to 0');
END;

-- 4. Session state transition and terminal composition guards.

-- Terminal sessions cannot change state or be modified.
CREATE TRIGGER sessions_terminal_immutable
BEFORE UPDATE ON sessions
WHEN OLD.status IN ('complete', 'partial', 'failed')
BEGIN
  SELECT RAISE(ABORT, 'terminal session cannot be modified');
END;

-- Legal session transitions: queued -> reviewing, reviewing -> complete | partial | failed.
CREATE TRIGGER sessions_status_transition
BEFORE UPDATE ON sessions
WHEN OLD.status NOT IN ('complete', 'partial', 'failed')
  AND OLD.status != NEW.status
  AND NOT (
    (OLD.status = 'queued' AND NEW.status = 'reviewing')
    OR
    (OLD.status = 'reviewing' AND NEW.status IN ('complete', 'partial', 'failed'))
  )
BEGIN
  SELECT RAISE(ABORT, 'illegal session status transition');
END;

-- Terminal composition validation on transition to terminal state.
CREATE TRIGGER sessions_terminal_composition_guard
BEFORE UPDATE ON sessions
WHEN OLD.status = 'reviewing' AND NEW.status IN ('complete', 'partial', 'failed')
  AND (
    (NEW.status = 'complete' AND (
      NEW.completed_role_count != 4
      OR NEW.incomplete_role_count != 0
      OR NEW.terminal_reason IS NOT 'all_roles_complete'
      OR NEW.terminal_at IS NULL
    ))
    OR
    (NEW.status = 'partial' AND (
      NEW.completed_role_count >= 4
      OR NEW.terminal_reason IS NULL
      OR NEW.terminal_reason NOT IN ('user_cancelled', 'process_restart', 'deadline_cutoff', 'role_failures')
      OR (NEW.completed_role_count = 0 AND NEW.terminal_reason != 'user_cancelled')
      OR NEW.terminal_at IS NULL
    ))
    OR
    (NEW.status = 'failed' AND (
      NEW.completed_role_count != 0
      OR NEW.terminal_reason IS NULL
      OR NEW.terminal_reason NOT IN ('process_restart', 'deadline_cutoff', 'role_failures')
      OR NEW.terminal_at IS NULL
    ))
  )
BEGIN
  SELECT RAISE(ABORT, 'invalid terminal session composition');
END;

-- 5. Role run state transition and terminal guards.

-- Terminal role runs cannot change state or be modified.
CREATE TRIGGER role_runs_terminal_immutable
BEFORE UPDATE ON role_runs
WHEN OLD.status IN ('complete', 'failed', 'interrupted')
BEGIN
  SELECT RAISE(ABORT, 'terminal role run cannot be modified');
END;

-- Legal role run transitions: pending -> in_flight | interrupted, in_flight -> complete | failed | interrupted.
CREATE TRIGGER role_runs_status_transition
BEFORE UPDATE ON role_runs
WHEN OLD.status NOT IN ('complete', 'failed', 'interrupted')
  AND OLD.status != NEW.status
  AND NOT (
    (OLD.status = 'pending' AND NEW.status IN ('in_flight', 'interrupted'))
    OR
    (OLD.status = 'in_flight' AND NEW.status IN ('complete', 'failed', 'interrupted'))
  )
BEGIN
  SELECT RAISE(ABORT, 'illegal role run status transition');
END;

-- Field rules corresponding to target role state during transition.
CREATE TRIGGER role_runs_transition_fields_guard
BEFORE UPDATE ON role_runs
WHEN OLD.status NOT IN ('complete', 'failed', 'interrupted')
  AND OLD.status != NEW.status
  AND (
    (NEW.status = 'in_flight' AND (
      NEW.started_at IS NULL
      OR NEW.completed_at IS NOT NULL
      OR NEW.cause IS NOT NULL
      OR NEW.error_category IS NOT NULL
      OR NEW.error_message IS NOT NULL
    ))
    OR
    (NEW.status = 'complete' AND (
      NEW.started_at IS NULL
      OR NEW.completed_at IS NULL
      OR NEW.cause IS NOT NULL
      OR NEW.error_category IS NOT NULL
      OR NEW.error_message IS NOT NULL
      OR NEW.call_count < 1
    ))
    OR
    (NEW.status = 'failed' AND (
      NEW.completed_at IS NULL
      OR NEW.cause IS NOT NULL
      OR NEW.error_category IS NULL
    ))
    OR
    (NEW.status = 'interrupted' AND (
      NEW.completed_at IS NULL
      OR NEW.cause IS NULL
      OR NEW.error_category IS NOT NULL
      OR NEW.error_message IS NOT NULL
    ))
  )
BEGIN
  SELECT RAISE(ABORT, 'invalid role run transition fields');
END;

-- 6. Citation integrity guards: basis refs must belong to the finding's session snapshot.

CREATE TRIGGER finding_basis_refs_citation_insert
BEFORE INSERT ON finding_basis_refs
WHEN (
  SELECT eu.snapshot_id
  FROM evidence_units eu
  WHERE eu.id = NEW.evidence_unit_id
) IS NOT NULL
AND (
  SELECT s.snapshot_id
  FROM findings f
  JOIN role_runs rr ON rr.id = f.role_run_id
  JOIN sessions s ON s.id = rr.session_id
  WHERE f.id = NEW.finding_id
) IS NOT NULL
AND (
  SELECT eu.snapshot_id
  FROM evidence_units eu
  WHERE eu.id = NEW.evidence_unit_id
) != (
  SELECT s.snapshot_id
  FROM findings f
  JOIN role_runs rr ON rr.id = f.role_run_id
  JOIN sessions s ON s.id = rr.session_id
  WHERE f.id = NEW.finding_id
)
BEGIN
  SELECT RAISE(ABORT, 'cross-snapshot evidence citation rejected');
END;

CREATE TRIGGER finding_basis_refs_citation_update
BEFORE UPDATE ON finding_basis_refs
WHEN (
  SELECT eu.snapshot_id
  FROM evidence_units eu
  WHERE eu.id = NEW.evidence_unit_id
) IS NOT NULL
AND (
  SELECT s.snapshot_id
  FROM findings f
  JOIN role_runs rr ON rr.id = f.role_run_id
  JOIN sessions s ON s.id = rr.session_id
  WHERE f.id = NEW.finding_id
) IS NOT NULL
AND (
  SELECT eu.snapshot_id
  FROM evidence_units eu
  WHERE eu.id = NEW.evidence_unit_id
) != (
  SELECT s.snapshot_id
  FROM findings f
  JOIN role_runs rr ON rr.id = f.role_run_id
  JOIN sessions s ON s.id = rr.session_id
  WHERE f.id = NEW.finding_id
)
BEGIN
  SELECT RAISE(ABORT, 'cross-snapshot evidence citation rejected');
END;
