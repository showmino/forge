-- Resources a service declares in forge.yaml: what exists, the plans that
-- change it, and OpenTofu's state for each service and environment.
CREATE TABLE resources (
    name    text PRIMARY KEY,
    service text NOT NULL,
    env     text NOT NULL,
    data    jsonb NOT NULL
);
CREATE INDEX resources_service ON resources (service, env);

CREATE TABLE resource_plans (
    seq             bigserial PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    service         text NOT NULL,
    env             text NOT NULL,
    state           text NOT NULL,
    creator_subject text NOT NULL DEFAULT '',
    data            jsonb NOT NULL,
    digest          text NOT NULL DEFAULT '',
    state_version   bigint NOT NULL DEFAULT 0,
    -- The SHA-256 of the active run's token; never the token.
    token_hash      text NOT NULL DEFAULT '',
    deadline        timestamptz,
    plan_file       bytea,
    plan_json       bytea,
    audit           bytea
);
CREATE INDEX resource_plans_target ON resource_plans (service, env, seq);
CREATE UNIQUE INDEX resource_plans_token ON resource_plans (token_hash) WHERE token_hash <> '';
-- The sweep and the "one run per target" check read only open plans.
CREATE INDEX resource_plans_open ON resource_plans (seq)
    WHERE state IN ('STATE_PLANNING', 'STATE_APPLYING', 'STATE_PLANNED');

CREATE TABLE resource_states (
    key       text PRIMARY KEY,
    data      bytea,
    version   bigint NOT NULL DEFAULT 0,
    lock_id   text NOT NULL DEFAULT '',
    lock_info bytea
);
