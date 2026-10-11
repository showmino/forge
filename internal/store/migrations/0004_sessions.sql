-- Sessions: logins forge-api keeps for the CLI and the web dashboard. The
-- token itself is never stored, only its SHA-256.
CREATE TABLE sessions (
    id           text PRIMARY KEY,
    token_hash   text NOT NULL UNIQUE,
    kind         text NOT NULL,
    label        text NOT NULL DEFAULT '',
    subject      text NOT NULL,
    email        text NOT NULL DEFAULT '',
    login        text NOT NULL DEFAULT '',
    user_groups  text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    checked_at   timestamptz NOT NULL
);
CREATE INDEX sessions_subject ON sessions (subject, created_at);
CREATE INDEX sessions_expires_at ON sessions (expires_at);
