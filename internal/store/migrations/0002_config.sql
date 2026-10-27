-- The user's Entra groups go into the session, so roles are recomputed on
-- every request: removing a group from the admin map takes effect at once,
-- not only when the session expires.
ALTER TABLE sessions ADD COLUMN groups text[] NOT NULL DEFAULT '{}';

-- Changes made in the dashboard's Config tab, on top of the config file.
-- Append-only: every save is a new revision of one kind (providers, models,
-- pricing, groups) holding the FULL set of that kind. The newest revision of
-- each kind is in effect; a rollback is a new revision copying an old one.
CREATE TABLE config_revisions (
    id          bigserial   PRIMARY KEY,
    kind        text        NOT NULL,
    payload     jsonb       NOT NULL,
    actor       text        NOT NULL,
    note        text        NOT NULL DEFAULT '',
    rollback_of bigint,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX config_revisions_kind ON config_revisions (kind, id DESC);
