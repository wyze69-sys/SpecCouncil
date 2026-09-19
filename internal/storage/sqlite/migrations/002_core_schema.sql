-- 002_core_schema.sql
-- Core production schema for SpecCouncil persistence:
-- snapshots, evidence_units, sessions, role_runs, findings, finding_basis_refs.

-- 1. Snapshots: immutable snapshot identity and deterministic hash.
CREATE TABLE snapshots (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  hash TEXT NOT NULL CHECK (length(hash) = 64 AND hash NOT GLOB '*[^0-9a-f]*'),
  project_id TEXT NOT NULL CHECK (length(project_id) > 0),
  title TEXT NOT NULL CHECK (length(title) > 0),
  content TEXT NOT NULL CHECK (length(content) > 0),
  normalization_version INTEGER NOT NULL CHECK (normalization_version = 1),
  created_at TEXT NOT NULL CHECK (created_at LIKE '%Z' AND length(created_at) >= 20)
);

-- 2. Evidence units: addressable design segments within a snapshot.
-- ON DELETE CASCADE: an evidence unit is owned entirely by its parent snapshot.
CREATE TABLE evidence_units (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  snapshot_id TEXT NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL CHECK (length(unit_id) > 0),
  ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
  kind TEXT NOT NULL CHECK (kind IN ('brief', 'requirement', 'component', 'flow', 'constraint', 'data_rule')),
  text TEXT NOT NULL CHECK (length(text) > 0),
  UNIQUE (snapshot_id, unit_id),
  UNIQUE (snapshot_id, ordinal)
);

-- 3. Sessions: review sessions with project, idempotency, and status tracking.
-- ON DELETE RESTRICT: a snapshot must not be deleted while referenced by a session.
CREATE TABLE sessions (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  project_id TEXT NOT NULL CHECK (length(project_id) > 0),
  idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) > 0),
  request_hash TEXT NOT NULL CHECK (length(request_hash) = 64 AND request_hash NOT GLOB '*[^0-9a-f]*'),
  snapshot_id TEXT NOT NULL REFERENCES snapshots(id) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK (status IN ('queued', 'reviewing', 'complete', 'partial', 'failed')),
  cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
  dispatch_cutoff_at TEXT CHECK (dispatch_cutoff_at IS NULL OR (dispatch_cutoff_at LIKE '%Z' AND length(dispatch_cutoff_at) >= 20)),
  hard_deadline_at TEXT CHECK (hard_deadline_at IS NULL OR (hard_deadline_at LIKE '%Z' AND length(hard_deadline_at) >= 20)),
  terminal_reason TEXT CHECK (terminal_reason IS NULL OR terminal_reason IN ('all_roles_complete', 'user_cancelled', 'process_restart', 'deadline_cutoff', 'role_failures')),
  completed_role_count INTEGER NOT NULL DEFAULT 0 CHECK (completed_role_count >= 0 AND completed_role_count <= 4),
  incomplete_role_count INTEGER NOT NULL DEFAULT 4 CHECK (incomplete_role_count >= 0 AND incomplete_role_count <= 4),
  created_at TEXT NOT NULL CHECK (created_at LIKE '%Z' AND length(created_at) >= 20),
  claimed_at TEXT CHECK (claimed_at IS NULL OR (claimed_at LIKE '%Z' AND length(claimed_at) >= 20)),
  terminal_at TEXT CHECK (terminal_at IS NULL OR (terminal_at LIKE '%Z' AND length(terminal_at) >= 20)),
  UNIQUE (project_id, idempotency_key),
  CHECK (completed_role_count + incomplete_role_count = 4),
  CHECK (dispatch_cutoff_at IS NULL OR hard_deadline_at IS NULL OR hard_deadline_at >= dispatch_cutoff_at),
  CHECK (claimed_at IS NULL OR claimed_at >= created_at),
  CHECK (terminal_at IS NULL OR claimed_at IS NULL OR terminal_at >= claimed_at),
  CHECK (
    (status = 'queued' AND
     claimed_at IS NULL AND
     dispatch_cutoff_at IS NULL AND
     hard_deadline_at IS NULL AND
     terminal_at IS NULL AND
     terminal_reason IS NULL AND
     completed_role_count = 0 AND
     incomplete_role_count = 4)
    OR
    (status = 'reviewing' AND
     claimed_at IS NOT NULL AND
     dispatch_cutoff_at IS NOT NULL AND
     hard_deadline_at IS NOT NULL AND
     terminal_at IS NULL AND
     terminal_reason IS NULL)
    OR
    (status = 'complete' AND
     claimed_at IS NOT NULL AND
     dispatch_cutoff_at IS NOT NULL AND
     hard_deadline_at IS NOT NULL AND
     terminal_at IS NOT NULL AND
     terminal_reason = 'all_roles_complete' AND
     completed_role_count = 4 AND
     incomplete_role_count = 0)
    OR
    (status = 'partial' AND
     claimed_at IS NOT NULL AND
     dispatch_cutoff_at IS NOT NULL AND
     hard_deadline_at IS NOT NULL AND
     terminal_at IS NOT NULL AND
     terminal_reason IN ('user_cancelled', 'process_restart', 'deadline_cutoff', 'role_failures') AND
     completed_role_count < 4 AND
     (completed_role_count > 0 OR terminal_reason = 'user_cancelled'))
    OR
    (status = 'failed' AND
     claimed_at IS NOT NULL AND
     dispatch_cutoff_at IS NOT NULL AND
     hard_deadline_at IS NOT NULL AND
     terminal_at IS NOT NULL AND
     terminal_reason IN ('process_restart', 'deadline_cutoff', 'role_failures') AND
     completed_role_count = 0)
  )
);

-- 4. Role runs: single execution record per canonical role for a session.
-- ON DELETE CASCADE: a role run does not survive its session.
CREATE TABLE role_runs (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK (role IN ('requirements', 'architecture', 'qa', 'security')),
  status TEXT NOT NULL CHECK (status IN ('pending', 'in_flight', 'complete', 'failed', 'interrupted')),
  cause TEXT CHECK (cause IS NULL OR cause IN ('user_cancelled', 'deadline_cutoff', 'process_restart')),
  error_category TEXT CHECK (error_category IS NULL OR error_category IN ('transport', 'timeout', 'provider_rejected', 'budget_exhausted', 'invalid_json', 'schema_invalid', 'invalid_basis_ref')),
  error_message TEXT,
  call_count INTEGER NOT NULL DEFAULT 0 CHECK (call_count >= 0 AND call_count <= 2),
  started_at TEXT CHECK (started_at IS NULL OR (started_at LIKE '%Z' AND length(started_at) >= 20)),
  completed_at TEXT CHECK (completed_at IS NULL OR (completed_at LIKE '%Z' AND length(completed_at) >= 20)),
  created_at TEXT NOT NULL CHECK (created_at LIKE '%Z' AND length(created_at) >= 20),
  UNIQUE (session_id, role),
  CHECK (completed_at IS NULL OR started_at IS NULL OR completed_at >= started_at),
  CHECK (
    (status = 'pending' AND
     started_at IS NULL AND
     completed_at IS NULL AND
     cause IS NULL AND
     error_category IS NULL AND
     error_message IS NULL AND
     call_count = 0)
    OR
    (status = 'in_flight' AND
     started_at IS NOT NULL AND
     completed_at IS NULL AND
     cause IS NULL AND
     error_category IS NULL AND
     error_message IS NULL)
    OR
    (status = 'complete' AND
     started_at IS NOT NULL AND
     completed_at IS NOT NULL AND
     cause IS NULL AND
     error_category IS NULL AND
     error_message IS NULL AND
     call_count >= 1)
    OR
    (status = 'failed' AND
     completed_at IS NOT NULL AND
     cause IS NULL AND
     error_category IS NOT NULL)
    OR
    (status = 'interrupted' AND
     completed_at IS NOT NULL AND
     cause IS NOT NULL AND
     error_category IS NULL AND
     error_message IS NULL)
  )
);

-- 5. Findings: validated reviewer findings produced by a role run.
-- ON DELETE CASCADE: a finding does not survive its role run.
CREATE TABLE findings (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  role_run_id TEXT NOT NULL REFERENCES role_runs(id) ON DELETE CASCADE,
  finding_id TEXT NOT NULL CHECK (length(finding_id) > 0),
  severity TEXT NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low')),
  category TEXT NOT NULL CHECK (length(category) > 0),
  issue TEXT NOT NULL CHECK (length(issue) > 0 AND length(issue) <= 1000),
  recommendation TEXT NOT NULL CHECK (length(recommendation) > 0 AND length(recommendation) <= 1000),
  created_at TEXT NOT NULL CHECK (created_at LIKE '%Z' AND length(created_at) >= 20),
  UNIQUE (role_run_id, finding_id)
);

-- 6. Finding basis references: citations from a finding to snapshot evidence units.
-- ON DELETE CASCADE: a basis ref does not survive its finding.
-- ON DELETE RESTRICT: an evidence unit cannot be deleted while cited by a finding.
CREATE TABLE finding_basis_refs (
  id TEXT PRIMARY KEY CHECK (length(id) > 0),
  finding_id TEXT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
  evidence_unit_id TEXT NOT NULL REFERENCES evidence_units(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL CHECK (ordinal >= 1 AND ordinal <= 5),
  UNIQUE (finding_id, evidence_unit_id),
  UNIQUE (finding_id, ordinal)
);
