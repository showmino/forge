-- What Forge keeps about a team of the organization (phase 15): its on-call
-- rotation. The team itself is GitHub's.
CREATE TABLE team_settings (
    team       text PRIMARY KEY,
    oncall     text NOT NULL,
    updated_by text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL
);
