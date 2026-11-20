-- name: CreateLLMAccount :one
INSERT INTO llm_accounts (id, name, harness, auth_mode, active, api_key_secret_id, credential_store, credential_key, provider)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *;

-- name: ReadLLMAccount :one
SELECT * FROM llm_accounts WHERE id=$1;

-- name: LockLLMAccount :one
SELECT * FROM llm_accounts WHERE id=$1 FOR UPDATE;

-- name: ShareLLMAccount :one
SELECT * FROM llm_accounts WHERE id=$1 FOR SHARE;

-- name: UpdateLLMAccount :one
UPDATE llm_accounts SET name=$2, active=$3, updated_at=clock_timestamp() WHERE id=$1 RETURNING *;

-- name: DeleteLLMAccount :execrows
-- Reject references without taking row locks in the referencing tables. An
-- edit locks its profile before its account; entering the FK check while
-- holding the account could otherwise reverse that order and deadlock.
DELETE FROM llm_accounts a WHERE a.id=$1
AND NOT EXISTS(SELECT 1 FROM model_profiles p WHERE p.llm_account_id=a.id)
AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.llm_account_id=a.id);

-- name: GetLLMAccount :one
SELECT a.*, ARRAY(SELECT p.name FROM model_profiles p WHERE p.llm_account_id=a.id ORDER BY p.name)::text[] AS profiles
FROM llm_accounts a WHERE a.id=$1;

-- name: AccountSources :many
SELECT a.*, COALESCE(encode(k.nonce,'hex'),'')::text AS key_version,
    ARRAY(SELECT p.name FROM model_profiles p WHERE p.llm_account_id=a.id ORDER BY p.name)::text[] AS profiles
FROM llm_accounts a LEFT JOIN secrets k ON k.id=a.api_key_secret_id
WHERE (a.auth_mode='account' OR (a.auth_mode='api_key' AND a.provider='kimi-code-plan-global'))
AND (sqlc.narg(account_ids)::uuid[] IS NULL OR a.id=ANY(sqlc.narg(account_ids)::uuid[])) ORDER BY a.id;

-- name: AccountStoreNames :many
SELECT DISTINCT credential_store::text FROM llm_accounts WHERE credential_store IS NOT NULL ORDER BY credential_store;

-- name: CreateModelProfile :one
INSERT INTO model_profiles (id, name, description, llm_account_id, model, instructions, codex, active, opencode_variant)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *;

-- name: LockModelProfile :one
SELECT * FROM model_profiles WHERE id=$1 FOR UPDATE;

-- name: ModelProfileForAdmission :one
SELECT * FROM model_profiles WHERE name=$1 FOR SHARE;

-- name: UpdateModelProfile :one
UPDATE model_profiles SET description=$2, llm_account_id=$3, model=$4, instructions=$5, codex=$6, active=$7, opencode_variant=$8, updated_at=clock_timestamp()
WHERE id=$1 RETURNING *;

-- name: DeleteModelProfile :execrows
DELETE FROM model_profiles WHERE id=$1;

-- name: GetModelProfile :one
SELECT p.*, a.name AS account_name, a.active AS account_active, a.harness
FROM model_profiles p JOIN llm_accounts a ON a.id=p.llm_account_id WHERE p.id=$1;

-- name: ListLLMAccounts :many
SELECT a.*, ARRAY(SELECT p.name FROM model_profiles p WHERE p.llm_account_id=a.id ORDER BY p.name)::text[] AS profiles FROM llm_accounts a
WHERE (sqlc.narg(active)::boolean IS NULL OR a.active = sqlc.narg(active))
  AND (sqlc.narg(exact_name)::text IS NULL OR a.name = sqlc.narg(exact_name))
  AND (sqlc.narg(name_pattern)::text IS NULL OR a.name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
  AND (sqlc.arg(first_page)::boolean OR
      (NOT sqlc.arg(backward)::boolean AND (a.name, a.id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(backward)::boolean AND (a.name, a.id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(inclusive)::boolean AND (a.name, a.id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
ORDER BY
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN a.name END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN a.name END DESC,
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN a.id END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN a.id END DESC
LIMIT sqlc.arg(page_limit);

-- name: LLMAccountsPageBoundary :one
SELECT EXISTS (
    SELECT 1 FROM llm_accounts a
    WHERE (sqlc.narg(active)::boolean IS NULL OR a.active = sqlc.narg(active))
      AND (sqlc.narg(exact_name)::text IS NULL OR a.name = sqlc.narg(exact_name))
      AND (sqlc.narg(name_pattern)::text IS NULL OR a.name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
      AND ((sqlc.arg(backward)::boolean AND (a.name, a.id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (NOT sqlc.arg(backward)::boolean AND (a.name, a.id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (sqlc.arg(inclusive)::boolean AND (a.name, a.id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
) AS available;

-- name: ListModelProfiles :many
SELECT a.*, b.name AS account_name, b.active AS account_active, b.harness FROM model_profiles a JOIN llm_accounts b ON b.id=a.llm_account_id
WHERE (sqlc.narg(available)::boolean IS NULL OR (a.active AND b.active) = sqlc.narg(available))
  AND (sqlc.narg(active)::boolean IS NULL OR a.active = sqlc.narg(active))
  AND (sqlc.narg(exact_name)::text IS NULL OR a.name = sqlc.narg(exact_name))
  AND (sqlc.narg(name_pattern)::text IS NULL OR a.name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
  AND (sqlc.arg(first_page)::boolean OR
      (NOT sqlc.arg(backward)::boolean AND (a.name, a.id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(backward)::boolean AND (a.name, a.id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(inclusive)::boolean AND (a.name, a.id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
ORDER BY
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN a.name END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN a.name END DESC,
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN a.id END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN a.id END DESC
LIMIT sqlc.arg(page_limit);

-- name: ModelProfilesPageBoundary :one
SELECT EXISTS (
    SELECT 1 FROM model_profiles a JOIN llm_accounts b ON b.id=a.llm_account_id
    WHERE (sqlc.narg(available)::boolean IS NULL OR (a.active AND b.active) = sqlc.narg(available))
  AND (sqlc.narg(active)::boolean IS NULL OR a.active = sqlc.narg(active))
      AND (sqlc.narg(exact_name)::text IS NULL OR a.name = sqlc.narg(exact_name))
      AND (sqlc.narg(name_pattern)::text IS NULL OR a.name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
      AND ((sqlc.arg(backward)::boolean AND (a.name, a.id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (NOT sqlc.arg(backward)::boolean AND (a.name, a.id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (sqlc.arg(inclusive)::boolean AND (a.name, a.id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
) AS available;

-- name: KimiLimitCredentials :many
SELECT a.id, a.harness, k.nonce, k.ciphertext
FROM llm_accounts a JOIN secrets k ON k.id = a.api_key_secret_id
WHERE a.active AND a.auth_mode='api_key' AND a.provider='kimi-code-plan-global'
ORDER BY a.id;
