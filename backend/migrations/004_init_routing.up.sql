-- migrations/004_init_routing.up.sql
-- 规格书 7.4 — Routing

CREATE TABLE IF NOT EXISTS routing_configs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    alias           TEXT NOT NULL UNIQUE,
    strategy        TEXT NOT NULL DEFAULT 'priority',
    config_json     TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
