-- 004_findings_insert_guard.sql
-- Findings insertion guard: findings may only be inserted while role is in_flight.

CREATE TRIGGER findings_insert_in_flight_guard
BEFORE INSERT ON findings
WHEN (SELECT status FROM role_runs WHERE id = NEW.role_run_id) IS NOT 'in_flight'
BEGIN
  SELECT RAISE(ABORT, 'findings may only be inserted while role is in_flight');
END;
