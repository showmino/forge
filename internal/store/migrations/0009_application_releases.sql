-- The releases of applications (phase 16): what every part ran in
-- production after one promotion or rollback of the application. parts is
-- a JSON list of {service, version, commit, deployCommit}.
CREATE TABLE application_releases (
    application text        NOT NULL,
    number      integer     NOT NULL CHECK (number > 0),
    created_at  timestamptz NOT NULL,
    created_by  text        NOT NULL DEFAULT '',
    note        text        NOT NULL DEFAULT '',
    parts       jsonb       NOT NULL,
    PRIMARY KEY (application, number)
);
