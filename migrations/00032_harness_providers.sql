-- +goose Up
ALTER TABLE llm_accounts DROP CONSTRAINT llm_accounts_harness_check;
ALTER TABLE llm_accounts ADD COLUMN provider text NOT NULL DEFAULT 'openai';
ALTER TABLE llm_accounts ADD CONSTRAINT llm_accounts_harness_provider_check CHECK (
    (harness = 'codex' AND provider = 'openai') OR
    (harness = 'opencode' AND char_length(provider) BETWEEN 1 AND 128 AND provider = btrim(provider))
);

-- +goose Down
ALTER TABLE llm_accounts DROP CONSTRAINT llm_accounts_harness_provider_check;
ALTER TABLE llm_accounts ADD CONSTRAINT llm_accounts_harness_check CHECK (harness = 'codex');
ALTER TABLE llm_accounts DROP COLUMN provider;
