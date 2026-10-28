-- Accounts schema. Times are Unix seconds, UTC.

CREATE TABLE users (
    id                 INTEGER PRIMARY KEY,
    github_id          INTEGER NOT NULL UNIQUE,
    login              TEXT    NOT NULL,
    created_at         INTEGER NOT NULL,
    last_login_at      INTEGER,
    last_seen_at       INTEGER,
    disabled_at        INTEGER,
    disabled_reason    TEXT,
    default_visibility INTEGER NOT NULL DEFAULT 0 CHECK (default_visibility IN (0, 1)),
    abuse_strikes      INTEGER NOT NULL DEFAULT 0
) STRICT;

-- id_hash is the SHA-256 of the session cookie, never the cookie itself
CREATE TABLE sessions (
    id_hash      BLOB    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    auth_at      INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL DEFAULT 'personal',
    name         TEXT    NOT NULL DEFAULT '',
    prefix       TEXT    NOT NULL,
    hash         BLOB    NOT NULL UNIQUE,
    scope        TEXT    NOT NULL DEFAULT 'paste',
    client_id    TEXT,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    expires_at   INTEGER
) STRICT;
CREATE INDEX api_tokens_user ON api_tokens (user_id);

-- visibility: 0 link, 1 private. seen_at is the first read by someone other than the owner.
CREATE TABLE pastes (
    key            TEXT    PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users (id),
    title          TEXT    NOT NULL DEFAULT '',
    language       TEXT    NOT NULL DEFAULT '',
    visibility     INTEGER NOT NULL DEFAULT 0 CHECK (visibility IN (0, 1)),
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER,
    size           INTEGER NOT NULL DEFAULT 0,
    views          INTEGER NOT NULL DEFAULT 0,
    last_opened_at INTEGER,
    seen_at        INTEGER,
    edited_at      INTEGER,
    edit_count     INTEGER NOT NULL DEFAULT 0,
    root_key       TEXT    NOT NULL,
    version        INTEGER NOT NULL DEFAULT 1,
    prev_key       TEXT    REFERENCES pastes (key) ON DELETE SET NULL
) STRICT;
CREATE INDEX pastes_user_created ON pastes (user_id, created_at DESC);
CREATE INDEX pastes_user_opened ON pastes (user_id, last_opened_at);
CREATE INDEX pastes_root ON pastes (root_key);
CREATE INDEX pastes_expires ON pastes (expires_at);
CREATE INDEX pastes_prev ON pastes (prev_key);

-- store_key is "<key>.r<rev>": a dotted key no route can reach
CREATE TABLE paste_revisions (
    paste_key  TEXT    NOT NULL REFERENCES pastes (key) ON DELETE CASCADE,
    rev        INTEGER NOT NULL,
    store_key  TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (paste_key, rev)
) STRICT, WITHOUT ROWID;

CREATE TABLE paste_tags (
    paste_key TEXT NOT NULL REFERENCES pastes (key) ON DELETE CASCADE,
    tag       TEXT NOT NULL CHECK (length(tag) BETWEEN 1 AND 32),
    PRIMARY KEY (paste_key, tag)
) STRICT, WITHOUT ROWID;
CREATE INDEX paste_tags_tag ON paste_tags (tag, paste_key);

-- Admin user ids below are plain numbers, not foreign keys: these rows
-- outlive the admin's account
CREATE TABLE tombstones (
    key           TEXT    PRIMARY KEY,
    reason        TEXT    NOT NULL,
    visibility    INTEGER NOT NULL,
    removed_at    INTEGER NOT NULL,
    admin_user_id INTEGER
) STRICT;

CREATE TABLE delete_queue (
    store_key   TEXT    PRIMARY KEY,
    enqueued_at INTEGER NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0
) STRICT;

-- client_key in handler.ClientKey form: an IPv4 address or an IPv6 /64
CREATE TABLE ip_bans (
    client_key    TEXT    PRIMARY KEY,
    reason        TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,
    admin_user_id INTEGER
) STRICT;

-- Append-only: rows can be added, never changed or removed
CREATE TABLE audit_log (
    id            INTEGER PRIMARY KEY,
    at            INTEGER NOT NULL,
    admin_user_id INTEGER,
    admin_login   TEXT    NOT NULL,
    action        TEXT    NOT NULL,
    target        TEXT    NOT NULL DEFAULT '',
    detail        TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX audit_log_at ON audit_log (at);

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
