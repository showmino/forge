-- The modes admins set for the switchable policy rules (phase 15): enforce,
-- warn or off. A rule with no row has its default.
CREATE TABLE rule_modes (
    rule       text PRIMARY KEY,
    mode       text NOT NULL CHECK (mode IN ('enforce', 'warn', 'off')),
    updated_by text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL
);
