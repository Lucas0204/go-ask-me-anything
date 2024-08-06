-- name: GetRoom :one
SELECT
    *
FROM rooms
WHERE id = $1;