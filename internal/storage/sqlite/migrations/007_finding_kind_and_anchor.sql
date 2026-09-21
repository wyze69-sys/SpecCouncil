-- 007_finding_kind_and_anchor.sql
-- Persist the v2 finding contract: the kind of concern a finding raises and, for
-- omissions, the anchor unit the omission is about.

ALTER TABLE findings ADD COLUMN kind TEXT NOT NULL DEFAULT 'existing'
  CHECK (kind IN ('existing', 'conflicting', 'missing'));

ALTER TABLE findings ADD COLUMN anchor_unit_id TEXT REFERENCES evidence_units(id) ON DELETE RESTRICT;

CREATE TRIGGER findings_missing_requires_anchor
BEFORE INSERT ON findings
WHEN NEW.kind = 'missing' AND NEW.anchor_unit_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'missing finding requires an anchor');
END;

CREATE TRIGGER findings_anchor_only_for_missing
BEFORE INSERT ON findings
WHEN NEW.kind != 'missing' AND NEW.anchor_unit_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'anchor is only allowed for a missing finding');
END;

CREATE TRIGGER findings_anchor_snapshot_guard
BEFORE INSERT ON findings
WHEN NEW.anchor_unit_id IS NOT NULL
AND (
  SELECT eu.snapshot_id FROM evidence_units eu WHERE eu.id = NEW.anchor_unit_id
) IS NOT NULL
AND (
  SELECT s.snapshot_id FROM role_runs rr JOIN sessions s ON s.id = rr.session_id
  WHERE rr.id = NEW.role_run_id
) IS NOT NULL
AND (
  SELECT eu.snapshot_id FROM evidence_units eu WHERE eu.id = NEW.anchor_unit_id
) != (
  SELECT s.snapshot_id FROM role_runs rr JOIN sessions s ON s.id = rr.session_id
  WHERE rr.id = NEW.role_run_id
)
BEGIN
  SELECT RAISE(ABORT, 'cross-snapshot anchor rejected');
END;
