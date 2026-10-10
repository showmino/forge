-- The audit log: one row per write call, outcome of long-running work, or
-- forge-api action. Append-only: the triggers below reject every change.
CREATE TABLE audit_events (
    id               bigserial PRIMARY KEY,
    time             timestamptz NOT NULL,
    kind             text NOT NULL,
    action           text NOT NULL,
    actor_kind       text NOT NULL,
    actor_subject    text NOT NULL DEFAULT '',
    actor_email      text NOT NULL DEFAULT '',
    actor_repository text NOT NULL DEFAULT '',
    service          text NOT NULL DEFAULT '',
    target           text NOT NULL DEFAULT '',
    result_code      text NOT NULL,
    result_message   text NOT NULL DEFAULT '',
    operation        text NOT NULL DEFAULT '',
    details          jsonb NOT NULL DEFAULT '{}',
    client_version   text NOT NULL DEFAULT '',
    request_id       text NOT NULL DEFAULT ''
);
CREATE INDEX audit_events_service ON audit_events (service, id);
CREATE INDEX audit_events_actor_email ON audit_events (actor_email, id);
CREATE INDEX audit_events_actor_repository ON audit_events (actor_repository, id);
CREATE INDEX audit_events_time ON audit_events (time);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END
$$;
CREATE TRIGGER audit_events_no_change BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
