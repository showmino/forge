-- Personal tokens (phase 15) are sessions of kind 'token' with the scopes
-- they may use. Logins have none: they may do all their person may.
ALTER TABLE sessions ADD COLUMN scopes text[] NOT NULL DEFAULT '{}';
