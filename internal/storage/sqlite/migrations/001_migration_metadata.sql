CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);
