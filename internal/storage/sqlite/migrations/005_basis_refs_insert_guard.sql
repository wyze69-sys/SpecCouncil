-- 005_basis_refs_insert_guard.sql
-- Basis references may only be inserted while their finding's role is in-flight.

CREATE TRIGGER finding_basis_refs_insert_in_flight_guard
BEFORE INSERT ON finding_basis_refs
WHEN EXISTS (
  SELECT 1 FROM findings f WHERE f.id = NEW.finding_id
)
AND (
  SELECT rr.status
  FROM findings f
  JOIN role_runs rr ON rr.id = f.role_run_id
  WHERE f.id = NEW.finding_id
) IS NOT 'in_flight'
BEGIN
  SELECT RAISE(ABORT, 'finding basis references may only be inserted while role is in_flight');
END;
