-- Schema migrations tracking table.
-- Tracks applied migrations with numeric version, exact migration name, SHA-256 checksum,
-- and UTC applied timestamp formatted as ISO-8601 / RFC3339Nano.
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
