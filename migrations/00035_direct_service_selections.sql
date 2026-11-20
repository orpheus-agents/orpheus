-- +goose Up
-- Old snapshots are the authoritative record of which references each selection
-- contributed. Never infer them from the installation's current configuration.
-- +goose StatementBegin
CREATE FUNCTION pg_temp.direct_sources(services jsonb, refs jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
WITH snapshots AS (
    SELECT value FROM jsonb_array_elements(services)
), codes AS (
    SELECT DISTINCT value->>'code' AS code FROM snapshots
), manual AS (
    SELECT value AS name FROM jsonb_array_elements_text(refs)
    EXCEPT
    SELECT jsonb_array_elements_text(value->'env_from') FROM snapshots
)
SELECT jsonb_build_object(
    'services', ARRAY(SELECT code FROM codes ORDER BY code COLLATE "C"),
    'env_from', ARRAY(SELECT name FROM manual ORDER BY name COLLATE "C")
)
$$;
-- +goose StatementEnd

UPDATE sessions SET configuration = jsonb_set(configuration, '{public,sandbox}',
    (configuration #> '{public,sandbox}') || pg_temp.direct_sources(
        configuration #> '{public,sandbox,services}', configuration #> '{public,sandbox,env_from}'))
WHERE configuration #> '{public,sandbox}' IS NOT NULL;

WITH converted AS MATERIALIZED (
    SELECT id, pg_temp.direct_sources(services, to_jsonb(env_from)) AS settings FROM runs
)
UPDATE runs r SET services = c.settings->'services',
    env_from = ARRAY(SELECT jsonb_array_elements_text(c.settings->'env_from'))
FROM converted c WHERE r.id = c.id;

UPDATE session_events e SET data = data ||
    pg_temp.direct_sources(data->'services', data->'env_from') ||
    jsonb_build_object('harness_env_names', NULL, 'hooks',
        COALESCE((SELECT jsonb_agg(hook || '{"env_names":null}' ORDER BY ord)
                  FROM jsonb_array_elements(data->'hooks') WITH ORDINALITY AS h(hook,ord)), '[]'::jsonb))
WHERE type = 'run.updated';
DROP FUNCTION pg_temp.direct_sources(jsonb,jsonb);

-- +goose Down
-- The old metadata cannot be reconstructed from codes. Require a backup for
-- rollback when a service selection exists instead of fabricating snapshots.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM sessions WHERE jsonb_array_length(configuration #> '{public,sandbox,services}') > 0)
       OR EXISTS (SELECT FROM runs WHERE services <> '[]'::jsonb)
       OR EXISTS (SELECT FROM session_events WHERE type='run.updated' AND jsonb_array_length(data->'services') > 0)
    THEN
        RAISE EXCEPTION 'Restoring service snapshots requires the pre-upgrade database backup';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE session_events SET data = (data - 'harness_env_names') || jsonb_build_object('hooks',
    COALESCE((SELECT jsonb_agg(hook - 'env_names' ORDER BY ord)
              FROM jsonb_array_elements(data->'hooks') WITH ORDINALITY AS h(hook,ord)), '[]'::jsonb))
WHERE type = 'run.updated';
