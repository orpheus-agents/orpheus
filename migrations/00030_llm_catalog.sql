-- +goose Up
CREATE TABLE llm_accounts (
    id uuid PRIMARY KEY,
    name text COLLATE "C" NOT NULL CHECK (char_length(name) BETWEEN 1 AND 255 AND name = btrim(name)),
    harness text NOT NULL CHECK (harness = 'codex'),
    auth_mode text NOT NULL CHECK (auth_mode IN ('account', 'api_key')),
    active boolean NOT NULL DEFAULT true,
    api_key_secret_id uuid UNIQUE REFERENCES secrets(id) ON DELETE RESTRICT,
    credential_store text,
    credential_key text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((auth_mode = 'account' AND api_key_secret_id IS NULL AND credential_store IS NOT NULL AND credential_store <> '' AND credential_key IS NOT NULL AND credential_key <> '')
        OR (auth_mode = 'api_key' AND api_key_secret_id IS NOT NULL AND credential_store IS NULL AND credential_key IS NULL))
);
CREATE INDEX llm_accounts_page_idx ON llm_accounts (name, id);
CREATE INDEX llm_accounts_name_trgm_idx ON llm_accounts USING gin (name public.gin_trgm_ops);

CREATE TABLE model_profiles (
    id uuid PRIMARY KEY,
    name text COLLATE "C" NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 255 AND name = btrim(name)),
    description text,
    llm_account_id uuid NOT NULL REFERENCES llm_accounts(id) ON DELETE RESTRICT,
    model text NOT NULL CHECK (char_length(model) BETWEEN 1 AND 255 AND model = btrim(model)),
    instructions text NOT NULL DEFAULT '',
    codex jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(codex) = 'object'),
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX model_profiles_account_idx ON model_profiles(llm_account_id);
CREATE INDEX model_profiles_page_idx ON model_profiles(name, id);
CREATE INDEX model_profiles_name_trgm_idx ON model_profiles USING gin (name public.gin_trgm_ops);
ALTER TABLE sessions ADD COLUMN llm_account_id uuid REFERENCES llm_accounts(id) ON DELETE RESTRICT;
CREATE INDEX sessions_llm_account_idx ON sessions(llm_account_id);

-- +goose Down
ALTER TABLE sessions DROP COLUMN llm_account_id;
DROP TABLE model_profiles;
DROP TABLE llm_accounts;
