-- migrations/005_init_auth.up.sql
-- 规格书 7.5 — Auth

CREATE TABLE IF NOT EXISTS gateway_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL UNIQUE,
    key_hash        TEXT NOT NULL UNIQUE,
    allowed_models  TEXT NOT NULL DEFAULT '["*"]',
    rpm             INTEGER NOT NULL DEFAULT 100,
    tpm             INTEGER NOT NULL DEFAULT 500000,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
