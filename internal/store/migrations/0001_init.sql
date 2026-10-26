-- Application tokens created from the dashboard. Only the SHA-256 of the
-- token is stored; the clear text is shown once, at creation.
CREATE TABLE api_tokens (
    id                bigserial PRIMARY KEY,
    agent             text        NOT NULL,
    token_hash        text        NOT NULL UNIQUE,
    token_prefix      text        NOT NULL,
    team              text        NOT NULL DEFAULT '',
    cost_center       text        NOT NULL DEFAULT '',
    models            text[]      NOT NULL,
    tokens_per_minute bigint      NOT NULL DEFAULT 0,
    tokens_per_day    bigint      NOT NULL DEFAULT 0,
    note              text        NOT NULL DEFAULT '',
    created_by        text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    revoked_at        timestamptz,
    revoked_by        text,
    last_used_at      timestamptz
);
CREATE INDEX api_tokens_agent ON api_tokens (agent);

-- Dashboard sessions (Entra ID sign-in). The cookie value is hashed too.
CREATE TABLE sessions (
    id_hash    text        PRIMARY KEY,
    user_id    text        NOT NULL,
    user_name  text        NOT NULL,
    user_email text        NOT NULL,
    roles      text[]      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

-- Who did what (token creation, revocation, sign-in, denied sign-in).
CREATE TABLE audit_log (
    id      bigserial   PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text        NOT NULL,
    action  text        NOT NULL,
    target  text        NOT NULL,
    details jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
