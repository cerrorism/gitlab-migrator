-- name: CreateGithubPullRequest :one
INSERT INTO github_pull_request (github_repo_id, github_pr_id, gitlab_merge_request_id, status) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: CreateGitlabMergeRequest :one
INSERT INTO gitlab_merge_request (gitlab_project_id, gitlab_mr_iid, status) VALUES ($1, $2, $3) RETURNING *;

-- name: GetGitlabProject :one
SELECT * FROM gitlab_project WHERE id = $1;

-- name: GetGithubRepo :one
SELECT * FROM github_repo WHERE id = $1;

-- name: GetUnknownMergeRequests :many
SELECT * FROM gitlab_merge_request WHERE gitlab_project_id = $1 and status = 'unknown' order by id FOR UPDATE SKIP LOCKED limit 10;

-- name: UpdateMergeRequestMigrationDone :exec
UPDATE gitlab_merge_request SET status = 'done' WHERE id = $1;

-- name: UpdateMergeRequestMigrationFailed :exec
UPDATE gitlab_merge_request SET status = 'failed' WHERE id = $1;

-- name: UpdateMergeRequestMigrationSkipped :exec
UPDATE gitlab_merge_request SET status = 'skipped' WHERE id = $1;