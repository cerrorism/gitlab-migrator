-- name: GetGitLabToGithubMigration :one
SELECT * FROM gitlab_to_github_migration WHERE status = 'ONGOING' order by id FOR UPDATE SKIP LOCKED limit 1;

-- name: GetAllGitLabToGithubMigrationIIDs :many
SELECT mr_iid FROM gitlab_merge_request WHERE migration_id = $1;

-- name: CreateGitlabMergeRequest :one
INSERT INTO gitlab_merge_request (migration_id, mr_iid, merge_commit_sha, parent1_commit_sha, parent2_commit_sha, status) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetGitlabMergeRequests :many
UPDATE gitlab_merge_request SET status='ONGOING' WHERE id in (SELECT id FROM gitlab_merge_request as gmr WHERE gmr.migration_id = $1 and gmr.status = 'MR_FOUND' order by id FOR UPDATE SKIP LOCKED limit 1000) RETURNING *;

-- name: UpdateGitlabMergeRequestPRID :exec
UPDATE gitlab_merge_request SET pr_id = $1, status='PR_CREATED' WHERE id = $2;

-- name: UpdateGitlabMergeRequestNotes :exec
UPDATE gitlab_merge_request SET notes = $1 WHERE id = $2;
