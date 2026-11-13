-- +goose Up
-- Shared with other catalogs; concurrent test-schema migrations serialize extension creation.
SELECT pg_advisory_xact_lock(4857668146153);
CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;

CREATE TABLE templates (
    id uuid PRIMARY KEY,
    template_name text NOT NULL,
    tag text,
    name text COLLATE "C" GENERATED ALWAYS AS (template_name || COALESCE(':' || tag, '')) STORED NOT NULL,
    agentbox_template_id text NOT NULL,
    description text,
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (name),
    UNIQUE NULLS NOT DISTINCT (agentbox_template_id, tag)
);
CREATE INDEX templates_page_idx ON templates (name, id);
CREATE INDEX templates_name_trgm_idx ON templates USING gin (name public.gin_trgm_ops);

-- +goose Down
DROP TABLE templates;
-- pg_trgm belongs to the database and is intentionally retained.
