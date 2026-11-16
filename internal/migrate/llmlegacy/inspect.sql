WITH migration_input AS (
    SELECT
        $1::jsonb AS credential_stores,
        $2::text[] AS available_api_key_envs
), configured_stores AS (
    SELECT e.key AS name, jsonb_strip_nulls(e.value) AS settings
    FROM migration_input i
    CROSS JOIN LATERAL jsonb_each(i.credential_stores) e
), inspected AS (
    SELECT
        s.id AS session_id,
        s.configuration #>> '{public,agent,profile}' AS profile_name,
        s.configuration ->> 'harness' AS old_harness,
        s.configuration -> 'credentials' AS credentials,
        (
            s.sandbox_state = 'deleted'
            AND NOT s.slot_reserved
            AND EXISTS (SELECT 1 FROM runs r WHERE r.session_id = s.id)
            AND NOT EXISTS (
                SELECT 1 FROM runs r
                WHERE r.session_id = s.id
                  AND r.status NOT IN ('completed', 'failed', 'cancelled')
            )
            AND NOT EXISTS (
                SELECT 1 FROM hook_executions h
                WHERE h.session_id = s.id
                  AND h.status IN ('pending', 'running')
            )
            AND NOT EXISTS (
                SELECT 1 FROM operations o
                WHERE o.session_id = s.id
                  AND o.status IN ('pending', 'sending', 'uncertain')
            )
            AND NOT EXISTS (
                SELECT 1 FROM operations c
                WHERE c.session_id = s.id
                  AND c.kind = 'sandbox_create'
                  AND c.status = 'confirmed'
                  AND COALESCE(c.result ->> 'adopted', 'false') <> 'true'
                  AND NOT EXISTS (
                      SELECT 1 FROM operations d
                      WHERE d.session_id = s.id
                        AND d.kind = 'sandbox_delete'
                        AND d.status = 'confirmed'
                        AND d.parameters ->> 'create_operation_id' = c.id::text
                        AND d.parameters ->> 'sandbox_id' = c.result ->> 'sandbox_id'
                  )
            )
        ) AS credentials_not_needed
    FROM sessions s
), required AS (
    SELECT
        i.*,
        i.credentials ->> 'mode' AS old_mode,
        CASE
            WHEN NULLIF(btrim(i.old_harness), '') IS NULL THEN false
            WHEN i.credentials ->> 'mode' = 'account'
                AND jsonb_typeof(i.credentials -> 'store') = 'object'
                AND i.credentials #>> '{store,type}' = 's3'
                AND NULLIF(btrim(i.credentials #>> '{store,bucket}'), '') IS NOT NULL
                AND NULLIF(btrim(i.credentials #>> '{store,region}'), '') IS NOT NULL
                AND NULLIF(i.credentials ->> 'key', '') IS NOT NULL
            THEN true
            WHEN i.credentials ->> 'mode' = 'api_key'
                AND i.credentials ->> 'api_key_env' ~ '^[A-Za-z_][A-Za-z0-9_]*$'
            THEN true
            ELSE false
        END AS source_valid
    FROM inspected i
    WHERE NOT i.credentials_not_needed
), checked AS (
    SELECT r.*,
        (SELECT count(*) FROM configured_stores s
         WHERE s.settings = jsonb_strip_nulls(r.credentials -> 'store')) AS store_matches
    FROM required r
), problems AS (
    SELECT NULL::uuid AS session_id, NULL::text AS profile_name,
        'migration_input_missing'::text AS problem
    FROM migration_input
    WHERE credential_stores IS NULL OR available_api_key_envs IS NULL
    UNION ALL
    SELECT r.session_id, r.profile_name, checks.problem
    FROM checked r
    CROSS JOIN migration_input i
    CROSS JOIN LATERAL (
        VALUES
            ('invalid_legacy_credentials', NOT r.source_valid),
            ('unsupported_harness', r.source_valid AND r.old_harness <> 'codex'),
            ('credential_store_missing', r.source_valid AND r.old_mode = 'account'
                AND r.store_matches = 0),
            ('credential_store_ambiguous', r.source_valid AND r.old_mode = 'account'
                AND r.store_matches > 1),
            ('api_key_env_unavailable', r.source_valid AND r.old_mode = 'api_key'
                AND NOT COALESCE(
                    r.credentials ->> 'api_key_env' = ANY(i.available_api_key_envs), false))
    ) AS checks(problem, failed)
    WHERE i.credential_stores IS NOT NULL
      AND i.available_api_key_envs IS NOT NULL
      AND checks.failed
)
SELECT i.session_id, s.configuration, i.credentials_not_needed,
       COALESCE(string_agg(p.problem, ',' ORDER BY p.problem), '') AS problems
FROM inspected i JOIN sessions s ON s.id=i.session_id
LEFT JOIN problems p ON p.session_id=i.session_id
GROUP BY i.session_id,s.configuration,i.credentials_not_needed
ORDER BY i.session_id;
