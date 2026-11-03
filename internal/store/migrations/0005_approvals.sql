-- Approvals: the production runs of infrastructure repos (phase 12), waiting
-- for or given an approval by the owning team.
CREATE TABLE approvals (
    id             text PRIMARY KEY,
    service        text NOT NULL,
    environment    text NOT NULL,
    commit_sha     text NOT NULL,
    run_id         bigint NOT NULL DEFAULT 0,
    run_url        text NOT NULL DEFAULT '',
    requested_by   text NOT NULL,
    triggered_by   text NOT NULL DEFAULT '',
    state          text NOT NULL,
    decided_by     text NOT NULL DEFAULT '',
    reason         text NOT NULL DEFAULT '',
    dispatch_error text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    decided_at     timestamptz,
    claimed_at     timestamptz
);
CREATE INDEX approvals_service ON approvals (service, created_at);
CREATE INDEX approvals_state ON approvals (state, created_at);
