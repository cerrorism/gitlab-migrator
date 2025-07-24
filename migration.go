package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/manicminer/gitlab-migrator/db"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"
)

func migrateProject(ctx context.Context, mc *migrationContext) error {
	githubPath := strings.Split(mc.githubRepo.Name, "/")

	project, _, err := gl.Projects.GetProject(mc.gitlabProject.Name, &gitlab.GetProjectOptions{})
	if err != nil {
		return err
	}

	if project == nil {
		return fmt.Errorf("no matching GitLab project found: %s", mc.gitlabProject.Name)
	}
	mc.gitlabProjectFromAPI = project

	repo, err := getLocalRepo(ctx, project, githubPath)
	if err != nil {
		return err
	}
	mc.gitRepository = repo
	migratePullRequests(ctx, mc)

	return nil
}

func getLocalRepo(ctx context.Context, project *gitlab.Project, githubPath []string) (*git.Repository, error) {
	cloneUrl, err := url.Parse(project.HTTPURLToRepo)
	if err != nil {
		return nil, fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from GitLab to GitHub", "name", project.Name, "github_org", githubPath[0], "github_repo", githubPath[1])

	logger.Info("checking for existing repository on GitHub", "owner", githubPath[0], "repo", githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, githubPath[0], githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return nil, fmt.Errorf("retrieving github repo: %v", err)
	}

	if err != nil {
		logger.Error("error retrieving repository from GitHub", "owner", githubPath[0], "repo", githubPath[1], "error", err)
		return nil, fmt.Errorf("retrieving github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	logger.Info("cloning repository", "name", project.Name, "url", project.HTTPURLToRepo)
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("cloning gitlab repo: %v", err)
	}

	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, "github.com", githubPath[0], githubPath[1])

	if _, err = repo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: false,
	}); err != nil {
		return nil, fmt.Errorf("adding github remote: %v", err)
	}

	if err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		upToDateError := errors.New("already up-to-date")
		if !errors.As(err, &upToDateError) {
			return nil, fmt.Errorf("pushing to github repo: %v", err)
		}
	}
	return repo, nil
}

func migratePullRequests(ctx context.Context, mc *migrationContext) {

	mrs, err := mc.qtx.GetUnknownMergeRequests(ctx, mc.gitlabProject.ID)

	if err != nil {
		logger.Error("failed to get unknown merge requests", "error", err)
		return
	}

	for _, mr := range mrs {
		logger.Info(fmt.Sprintf("migrating merge request ID = %s", mr.GitlabMrIid))
		mergeRequest, _, err := gl.MergeRequests.GetMergeRequest(mc.gitlabProjectFromAPI.ID, int(mr.GitlabMrIid), &gitlab.GetMergeRequestsOptions{})
		if err != nil {
			if errors.Is(err, gitlab.ErrNotFound) {
				logger.Info(fmt.Sprintf("skip non-existing merge request ID = %d", mr.GitlabMrIid))
				continue
			} else {
				sendErr(fmt.Errorf("retrieving gitlab merge request %d: %v", mr.GitlabMrIid, err))
				return
			}
		}
		if mergeRequest == nil {
			continue
		}
		logger.Info(fmt.Sprintf("migrating merge request ID = %d", mr.GitlabMrIid))
		result := migrateSingleMergeRequest(ctx, mc, mergeRequest)
		err = mc.qtx.UpdateMergeRequestMigration(ctx, db.UpdateMergeRequestMigrationParams{
			Status: result,
			ID:     mr.ID,
		})
		if err != nil {
			return
		}
	}
}
