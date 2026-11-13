-- name: CreateTemplate :one
INSERT INTO templates (id, template_name, tag, agentbox_template_id, description, active)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetTemplate :one
SELECT * FROM templates WHERE id = $1;

-- name: UpdateTemplate :one
UPDATE templates SET
    active = COALESCE(sqlc.narg(active)::boolean, active),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN sqlc.narg(description)::text ELSE description END,
    updated_at = clock_timestamp()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteTemplate :execrows
DELETE FROM templates WHERE id = $1;

-- name: ListTemplates :many
SELECT * FROM templates
WHERE (sqlc.narg(active)::boolean IS NULL OR active = sqlc.narg(active))
  AND (sqlc.narg(exact_name)::text IS NULL OR name = sqlc.narg(exact_name))
  AND (sqlc.narg(name_pattern)::text IS NULL OR name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
  AND (sqlc.arg(first_page)::boolean OR
      (NOT sqlc.arg(backward)::boolean AND (name, id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(backward)::boolean AND (name, id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
      (sqlc.arg(inclusive)::boolean AND (name, id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
ORDER BY
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN name END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN name END DESC,
    CASE WHEN NOT sqlc.arg(backward)::boolean THEN id END ASC,
    CASE WHEN sqlc.arg(backward)::boolean THEN id END DESC
LIMIT sqlc.arg(page_limit);

-- name: TemplatePageBoundary :one
SELECT EXISTS (
    SELECT 1 FROM templates
    WHERE (sqlc.narg(active)::boolean IS NULL OR active = sqlc.narg(active))
      AND (sqlc.narg(exact_name)::text IS NULL OR name = sqlc.narg(exact_name))
      AND (sqlc.narg(name_pattern)::text IS NULL OR name ILIKE sqlc.narg(name_pattern) ESCAPE '\')
      AND ((sqlc.arg(backward)::boolean AND (name, id) < (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (NOT sqlc.arg(backward)::boolean AND (name, id) > (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)) OR
          (sqlc.arg(inclusive)::boolean AND (name, id) = (sqlc.arg(position_name)::text COLLATE "C", sqlc.arg(position_id)::uuid)))
) AS available;
