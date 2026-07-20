-- migrations/003_init_usage.up.sql
-- 规格书 7.3 — Usage

CREATE TABLE IF NOT EXISTS usage_records (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL,
    gateway_key_id  TEXT,
    provider_name   TEXT NOT NULL,
    model_id        TEXT NOT NULL,
    protocol        TEXT NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    cost            REAL NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    is_stream       BOOLEAN NOT NULL DEFAULT FALSE,
    status_code     INTEGER,
    error_type      TEXT,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_usage_created_at ON usage_records(created_at);
CREATE INDEX idx_usage_provider ON usage_records(provider_name);
CREATE INDEX idx_usage_model ON usage_records(model_id);
CREATE INDEX idx_usage_trace ON usage_records(trace_id);
