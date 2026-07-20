-- migrations/001_init_providers.up.sql
-- 规格书 7.1 — Providers
-- 当前实现由 GORM AutoMigrate 维护(对应 internal/database/models.go),
-- 此文件保留作为 schema 规格引用;后续可改用 golang-migrate 执行。

CREATE TABLE IF NOT EXISTS providers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL UNIQUE,
    protocol        TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS provider_models (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_name       TEXT NOT NULL REFERENCES providers(name),
    model_id            TEXT NOT NULL,
    cost_per_1k_input   REAL NOT NULL DEFAULT 0,
    cost_per_1k_output  REAL NOT NULL DEFAULT 0,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider_name, model_id)
);

CREATE TABLE IF NOT EXISTS model_aliases (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    alias           TEXT NOT NULL,
    provider_name   TEXT NOT NULL REFERENCES providers(name),
    model_id        TEXT NOT NULL REFERENCES provider_models(model_id),
    priority        INTEGER NOT NULL DEFAULT 0,
    weight          INTEGER NOT NULL DEFAULT 0,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(alias, provider_name, model_id)
);
