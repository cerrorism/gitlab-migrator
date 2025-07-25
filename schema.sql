CREATE TABLE IF NOT EXISTS gitlab_to_github_migration (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    gitlab_project_name TEXT NOT NULL,
    github_repo_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS gitlab_merge_request (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    migration_id BIGINT NOT NULL REFERENCES gitlab_to_github_migration(id),
    mr_iid BIGINT NOT NULL,
    merge_commit_sha TEXT NOT NULL,
    parent1_commit_sha TEXT NOT NULL,
    parent2_commit_sha TEXT NOT NULL,
    pr_id BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
