-- migrations/002_init_keys.up.sql
-- 规格书 7.2 — Keys

CREATE TABLE IF NOT EXISTS api_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_name   TEXT NOT NULL REFERENCES providers(name),
    name            TEXT NOT NULL,
    key_encrypted   TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'ACTIVE',
    cooling_until   DATETIME,
    cooling_count   INTEGER NOT NULL DEFAULT 0,
    total_requests  INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    error_count     INTEGER NOT NULL DEFAULT 0,
    last_used_at    DATETIME,
    last_error_at   DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider_name, name)
);
