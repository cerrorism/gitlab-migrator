package main

import (
	"context"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v5"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
	"os"
)

const (
	dateFormat = "Mon, 2 Jan 2006"
	dbString   = "user=zhengguang dbname=postgres sslmode=false"
)

var (
	githubToken string
	githubUser  string
	gitlabToken string
	inMemCache  *objectCache
	errCount    int
	logger      hclog.Logger
	database    *pgx.Conn
	queries     *db.Queries
	gh          *github.Client
	gl          *gitlab.Client
)

func sendErr(err error) {
	errCount++
	logger.Error(err.Error())
}

type migrationContext struct {
	githubRepo           *db.GithubRepo
	gitlabProject        *db.GitlabProject
	gitlabProjectFromAPI *gitlab.Project
	qtx                  *db.Queries
	gitRepository        *git.Repository
}

func setupDb(ctx context.Context) {
	var err error
	database, err = pgx.Connect(ctx, dbString)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
}

func main() {
	prepareAndSetup()
	ctx := context.Background()
	setupDb(ctx)
	defer func(database *pgx.Conn, ctx context.Context) {
		_ = database.Close(ctx)
	}(database, ctx)

	tx, err := database.Begin(ctx)
	if err != nil {
		logger.Error("failed to get tx", "error", err)
		os.Exit(1)
	}
	qtx := queries.WithTx(tx)
	defer func(tx pgx.Tx, ctx context.Context) {
		_ = tx.Rollback(ctx)
	}(tx, ctx)

	if false {
		for i := 1; i <= 47589; i++ {
			_, err := qtx.CreateGitlabMergeRequest(ctx, db.CreateGitlabMergeRequestParams{
				GitlabProjectID: 1,
				GitlabMrIid:     int64(i),
				Status:          "unknown",
			})
			if err != nil {
				logger.Error(fmt.Sprintf("cannot insert merge requests"))
				return
			}
		}
	}

	githubRepo, err := qtx.GetGithubRepo(ctx, 1)
	if err != nil {
		logger.Error("failed to get github repo", "error", err)
		os.Exit(1)
	}
	gitlabProject, err := qtx.GetGitlabProject(ctx, 1)
	if err != nil {
		logger.Error("failed to get gitlab project", "error", err)
		os.Exit(1)
	}
	mc := &migrationContext{
		githubRepo:    &githubRepo,
		gitlabProject: &gitlabProject,
		qtx:           qtx,
	}

	if err = migrateProject(ctx, mc); err != nil {
		sendErr(err)
		os.Exit(1)
	} else if errCount > 0 {
		logger.Warn(fmt.Sprintf("encountered %d errors during migration, review log output for details", errCount))
		os.Exit(1)
	}
	_ = tx.Commit(ctx)
}
