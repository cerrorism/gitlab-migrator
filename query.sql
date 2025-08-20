-- name: GetGitLabToGithubMigration :one
SELECT * FROM gitlab_to_github_migration WHERE status = 'ONGOING' order by id FOR UPDATE SKIP LOCKED limit 1;

-- name: GetAllGitLabToGithubMigrationIIDs :many
SELECT mr_iid FROM gitlab_merge_request WHERE migration_id = $1;

-- name: GetAllGitLabToGithubMigrationSHAs :many
SELECT merge_commit_sha FROM gitlab_merge_request WHERE migration_id = $1;

-- name: CreateGitlabMergeRequest :one
INSERT INTO gitlab_merge_request (migration_id, mr_iid, merge_commit_sha, parent1_commit_sha, parent2_commit_sha, status) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetGitlabMergeRequests :many
UPDATE gitlab_merge_request SET status=sqlc.arg(to_state) WHERE id in (SELECT id FROM gitlab_merge_request as gmr WHERE gmr.migration_id = $1 and gmr.status = sqlc.arg(from_state) order by id FOR UPDATE SKIP LOCKED limit $2) RETURNING *;

-- name: UpdateGitlabMergeRequestPRID :exec
UPDATE gitlab_merge_request SET pr_id = $1, status='PR_CREATED' WHERE id = $2;

-- name: UpdateGitlabMergeRequestStatus :exec
UPDATE gitlab_merge_request SET status = $2 WHERE id = $1;

-- name: UpdateGitlabMergeRequestNotes :exec
UPDATE gitlab_merge_request SET notes = $1 WHERE id = $2;

-- name: GetAvailableGithubAuthToken :one
UPDATE github_auth_token SET status='in_use', updated_at=CURRENT_TIMESTAMP WHERE id = (SELECT id FROM github_auth_token WHERE status = 'available' ORDER BY rate_limit_remaining DESC, id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *;

-- name: ReleaseGithubAuthToken :exec
UPDATE github_auth_token SET status='available', updated_at=CURRENT_TIMESTAMP WHERE id = $1;

-- name: UpdateGithubAuthTokenRateLimit :exec
UPDATE github_auth_token SET rate_limit_remaining = $1, rate_limit_reset = $2, updated_at=CURRENT_TIMESTAMP WHERE id = $3;

-- name: CreateMergeRequestNote :one
INSERT INTO gitlab_merge_request_note (merge_request_id, note_type, message) VALUES ($1, $2, $3) RETURNING *;

-- name: GetMergeRequestNotes :many
SELECT * FROM gitlab_merge_request_note WHERE merge_request_id = $1 ORDER BY created_at DESC;

-- name: GetMergeRequestNotesByType :many
SELECT * FROM gitlab_merge_request_note WHERE merge_request_id = $1 AND note_type = $2 ORDER BY created_at DESC;
