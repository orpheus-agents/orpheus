-- +goose Up
CREATE TABLE sessions (
 id uuid PRIMARY KEY,
 created_at timestamptz NOT NULL DEFAULT now(),
 configuration jsonb NOT NULL,
 env_ciphertext text,
 sandbox_state text NOT NULL DEFAULT 'not_created' CHECK (sandbox_state IN ('not_created','provisioning','ready','pausing','paused','resuming','unavailable')),
 sandbox_last_known_state text,
 sandbox_error jsonb,
 sandbox_id text,
 process_id integer,
 launch_id uuid,
 thread_id text,
 history_path text,
 history_offset bigint NOT NULL DEFAULT 0,
 workspace text,
 harness_home text,
 slot_reserved boolean NOT NULL DEFAULT true,
 next_run_number integer NOT NULL DEFAULT 1,
 next_event_sequence bigint NOT NULL DEFAULT 1
);
CREATE INDEX sessions_reserved ON sessions(id) WHERE slot_reserved;
CREATE TABLE runs (
 id uuid PRIMARY KEY,
 session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 number integer NOT NULL,
 status text NOT NULL DEFAULT 'accepted' CHECK (status IN ('accepted','starting','running','cancelling','completed','failed','cancelled')),
 observation text CHECK (observation IN ('attached','reconnecting','uncertain')),
 created_at timestamptz NOT NULL DEFAULT now(),
 execution_started_at timestamptz,
 deadline_at timestamptz,
 finished_at timestamptz,
 cancel_requested_at timestamptz,
 cancel_attempted_at timestamptz,
 stop_reason text CHECK (stop_reason IN ('user_request','run_timeout')),
 stop_method text CHECK (stop_method IN ('graceful','forced')),
 error jsonb,
 native_turn_id text,
 next_delivery_number integer NOT NULL DEFAULT 1,
 final_message_id uuid,
 UNIQUE(session_id,number),
 UNIQUE(session_id,id)
);
CREATE UNIQUE INDEX runs_one_unfinished_per_session ON runs(session_id) WHERE status IN ('accepted','starting','running','cancelling');
CREATE TABLE messages (
 id uuid PRIMARY KEY,
 session_id uuid NOT NULL,
 run_id uuid NOT NULL,
 role text NOT NULL,
 kind text,
 text text NOT NULL,
 delivery_status text,
 delivery_number integer,
 error jsonb,
 native_key text,
 registered_sequence bigint NOT NULL,
 position jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(session_id,run_id) REFERENCES runs(session_id,id) ON DELETE CASCADE,
 UNIQUE(session_id,native_key),
 UNIQUE(run_id,delivery_number),
 UNIQUE(run_id,id),
 CONSTRAINT message_role_delivery CHECK ((role='assistant' AND kind IS NOT NULL AND kind IN ('progress','answer') AND delivery_status IS NULL) OR (role='user' AND kind IS NULL AND delivery_status IS NOT NULL AND delivery_status IN ('pending','sending','delivered','uncertain','rejected')))
);
ALTER TABLE runs ADD CONSTRAINT run_final_message FOREIGN KEY(id,final_message_id) REFERENCES messages(run_id,id);
CREATE TABLE tool_calls (
 id uuid PRIMARY KEY,
 session_id uuid NOT NULL,
 run_id uuid NOT NULL,
 name text NOT NULL,
 input jsonb NOT NULL,
 status text NOT NULL CHECK (status IN ('running','completed','failed','cancelled','unknown')),
 result jsonb,
 result_digest text NOT NULL,
 output_completeness text NOT NULL CHECK (output_completeness IN ('complete','truncated','unavailable','unknown')),
 truncation_reason text CHECK (truncation_reason IN ('orpheus_limit','harness_limit')),
 native_key text,
 registered_sequence bigint NOT NULL,
 position jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(session_id,run_id) REFERENCES runs(session_id,id) ON DELETE CASCADE,
 UNIQUE(session_id,native_key)
);
CREATE TABLE session_events (
 session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 sequence bigint NOT NULL,
 type text NOT NULL CHECK (type IN ('run.updated','message.updated','tool_call.updated','sandbox.updated')),
 created_at timestamptz NOT NULL DEFAULT now(),
 data jsonb NOT NULL,
 PRIMARY KEY(session_id,sequence)
);
CREATE TABLE operations (
 id uuid PRIMARY KEY,
 session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 run_id uuid,
 message_id uuid REFERENCES messages(id),
 kind text NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sending','confirmed','failed','uncertain')),
 parameters jsonb NOT NULL DEFAULT '{}',
 result jsonb,
 attempted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(session_id,run_id) REFERENCES runs(session_id,id)
);
CREATE INDEX operations_session ON operations(session_id,kind);
CREATE TABLE idempotency_keys (
 operation text NOT NULL,
 resource text NOT NULL,
 key uuid NOT NULL,
 fingerprint text NOT NULL,
 session_id uuid NOT NULL REFERENCES sessions(id),
 run_id uuid NOT NULL REFERENCES runs(id),
 message_id uuid NOT NULL REFERENCES messages(id),
 PRIMARY KEY(operation,resource,key)
);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE operations;
DROP TABLE session_events;
DROP TABLE tool_calls;
ALTER TABLE runs DROP CONSTRAINT run_final_message;
DROP TABLE messages;
DROP TABLE runs;
DROP TABLE sessions;
