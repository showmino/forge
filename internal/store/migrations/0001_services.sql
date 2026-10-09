-- The service catalog. data is the forge.v1.Service message as protojson;
-- the other columns are what queries filter on.
CREATE TABLE services (
    name        text PRIMARY KEY,
    revision    bigint NOT NULL,
    repo        text,
    owner_team  text NOT NULL,
    data        jsonb NOT NULL,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);
CREATE INDEX services_repo ON services (repo);
CREATE INDEX services_owner_team ON services (owner_team);

-- RegisterService results by request_id, kept for 24 hours for retries.
CREATE TABLE service_requests (
    request_id text PRIMARY KEY,
    service    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
