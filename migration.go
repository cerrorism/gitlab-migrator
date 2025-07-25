package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
	"slices"
)

func updateStoredMergeRequests(ctx context.Context, mc *migrationContext) error {
	existingMergeRequestIIDs, err := mc.qtx.GetAllGitLabToGithubMigrationIIDs(ctx, mc.migration.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing merge request IIDs: %v", err)
	}

	mergeRequests, err := getMergeRequestInfoFromLocal(ctx, mc.migration.GithubRepoName)
	if err != nil {
		return fmt.Errorf("failed to get merge requests: %v", err)
	}
	for _, mr := range mergeRequests {
		if slices.Contains(existingMergeRequestIIDs, int64(mr.MrIID)) {
			continue
		}
		_, err := mc.qtx.CreateGitlabMergeRequest(ctx, db.CreateGitlabMergeRequestParams{
			MigrationID:      mc.migration.ID,
			MrIid:            int64(mr.MrIID),
			MergeCommitSha:   mr.MergeCommitSHA,
			Parent1CommitSha: mr.BaseParent,
			Parent2CommitSha: mr.HeadParent,
		})
		if err != nil {
			return fmt.Errorf("failed to create merge request: %v", err)
		}
	}
	return nil
}

func migrateProject(ctx context.Context, mc *migrationContext) error {
	project, _, err := gl.Projects.GetProject(mc.migration.GitlabProjectName, &gitlab.GetProjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to get gitlab project: %v", err)
	}

	if project == nil {
		return fmt.Errorf("no matching GitLab project found: %s", mc.gitlabProject.Name)
	}
	mc.gitlabProject = project

	migratePullRequests(ctx, mc)

	return nil
}

func migratePullRequests(ctx context.Context, mc *migrationContext) {

	mrs, err := mc.qtx.GetGitlabMergeRequests(ctx, mc.migration.ID)

	if err != nil {
		logger.Error("failed to get unknown merge requests", "error", err)
		return
	}

	for _, mr := range mrs {
		logger.Info(fmt.Sprintf("migrating merge request ID = %d", mr.MrIid))
		mergeRequest, _, err := gl.MergeRequests.GetMergeRequest(mc.gitlabProject.ID, int(mr.MrIid), &gitlab.GetMergeRequestsOptions{})
		if err != nil {
			if errors.Is(err, gitlab.ErrNotFound) {
				logger.Info(fmt.Sprintf("skip non-existing merge request ID = %d", mr.MrIid))
				continue
			} else {
				sendErr(fmt.Errorf("retrieving gitlab merge request %d: %v", mr.MrIid, err))
				return
			}
		}
		if mergeRequest == nil {
			continue
		}
		result := migrateSingleMergeRequest(ctx, mc, mergeRequest, &mr)
		err = mc.qtx.UpdateGitlabMergeRequestNotes(ctx, db.UpdateGitlabMergeRequestNotesParams{
			Notes: result,
			ID:    mr.ID,
		})
		if err != nil {
			logger.Error(fmt.Sprintf("failed to update merge request: %s", err.Error()), err)
			continue
		}
	}
}
