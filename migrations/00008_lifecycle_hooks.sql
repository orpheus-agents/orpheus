-- +goose Up
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN ('accepted','starting','running','cancelling','finalizing','completed','failed','cancelled'));
DROP INDEX runs_one_unfinished_per_session;
CREATE UNIQUE INDEX runs_one_unfinished_per_session ON runs(session_id) WHERE status IN ('accepted','starting','running','cancelling','finalizing');

ALTER TABLE runs
  ADD COLUMN phase text CHECK (phase IN ('preparation','after_create','before_run','agent','after_run')),
  ADD COLUMN agent_status text CHECK (agent_status IN ('completed','failed','cancelled')),
  ADD COLUMN agent_error jsonb;

CREATE TABLE hook_executions (
  id uuid PRIMARY KEY,
  session_id uuid NOT NULL,
  run_id uuid NOT NULL,
  name text NOT NULL CHECK (name IN ('after_create','before_run','after_run','before_remove')),
  status text NOT NULL CHECK (status IN ('pending','running','completed','failed','cancelled','skipped')),
  started_at timestamptz,
  deadline_at timestamptz,
  cancel_attempted_at timestamptz,
  finished_at timestamptz,
  exit_code integer,
  signal integer,
  output jsonb,
  output_completeness text NOT NULL DEFAULT 'unknown' CHECK (output_completeness IN ('complete','truncated','unavailable','unknown')),
  truncation_reason text CHECK (truncation_reason IN ('orpheus_limit')),
  error jsonb,
  UNIQUE (run_id,name),
  FOREIGN KEY (session_id,run_id) REFERENCES runs(session_id,id) ON DELETE CASCADE
);
CREATE INDEX hook_executions_session_name_status ON hook_executions(session_id,name,status);

-- +goose Down
DROP TABLE hook_executions;
ALTER TABLE runs DROP COLUMN phase, DROP COLUMN agent_status, DROP COLUMN agent_error;
DROP INDEX runs_one_unfinished_per_session;
CREATE UNIQUE INDEX runs_one_unfinished_per_session ON runs(session_id) WHERE status IN ('accepted','starting','running','cancelling');
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN ('accepted','starting','running','cancelling','completed','failed','cancelled'));
