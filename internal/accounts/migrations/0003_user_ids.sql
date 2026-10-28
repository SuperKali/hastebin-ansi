-- User ids are never handed out twice. Without AUTOINCREMENT, deleting the
-- account with the highest id let the next one take the same id, which
-- mixed up audit log rows and limits counted by user id. The table is
-- rebuilt with the same columns and rows; migrate runs this with foreign
-- keys off and checks them before it commits, so the sessions, tokens and
-- pastes that point at users stay as they are.

CREATE TABLE users_new (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    github_id          INTEGER NOT NULL UNIQUE,
    login              TEXT    NOT NULL,
    created_at         INTEGER NOT NULL,
    last_login_at      INTEGER,
    last_seen_at       INTEGER,
    disabled_at        INTEGER,
    disabled_reason    TEXT,
    default_visibility INTEGER NOT NULL DEFAULT 0 CHECK (default_visibility IN (0, 1)),
    abuse_strikes      INTEGER NOT NULL DEFAULT 0,
    avatar_url         TEXT
) STRICT;

INSERT INTO users_new (id, github_id, login, created_at, last_login_at, last_seen_at, disabled_at, disabled_reason,
    default_visibility, abuse_strikes, avatar_url)
SELECT id, github_id, login, created_at, last_login_at, last_seen_at, disabled_at, disabled_reason,
    default_visibility, abuse_strikes, avatar_url
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- Accounts deleted before this ran may have had higher ids than any left.
-- The audit log, tombstones and bans still name them: start past those too.
INSERT INTO sqlite_sequence (name, seq)
SELECT 'users', 0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'users');
UPDATE sqlite_sequence SET seq = MAX(seq,
    COALESCE((SELECT MAX(id) FROM users), 0),
    COALESCE((SELECT MAX(admin_user_id) FROM audit_log), 0),
    COALESCE((SELECT MAX(CAST(substr(target, 6) AS INTEGER)) FROM audit_log WHERE target LIKE 'user:%'), 0),
    COALESCE((SELECT MAX(admin_user_id) FROM tombstones), 0),
    COALESCE((SELECT MAX(admin_user_id) FROM ip_bans), 0))
WHERE name = 'users';
