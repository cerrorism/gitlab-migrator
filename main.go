package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v5"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
	"os"
)

const (
	dateFormat = "Mon, 2 Jan 2006"
	dbString   = "user=postgres password=password dbname=postgres sslmode=false"
)

var (
	githubToken string
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
	migration     *db.GitlabToGithubMigration
	gitlabProject *gitlab.Project
	qtx           *db.Queries
	githubAuth    *db.GithubAuthToken
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
	// Parse command line arguments
	var updateStoredMRs = flag.Bool("update-stored-mrs", false, "update stored merge requests from repository analysis")
	var migrateMRs = flag.Bool("migrate-mrs", false, "migrate merge requests from GitLab to GitHub")
	flag.Parse()

	// Validate that exactly one operation is specified
	if *updateStoredMRs == *migrateMRs {
		if *updateStoredMRs {
			logger.Error("cannot specify both --update-stored-mrs and --migrate-mrs")
		} else {
			logger.Error("must specify either --update-stored-mrs or --migrate-mrs")
		}
		flag.Usage()
		os.Exit(1)
	}

	ctx, githubAuth := prepareAndSetup()
	setupDb(ctx)
	defer func(database *pgx.Conn, ctx context.Context) {
		_ = database.Close(ctx)
	}(database, ctx)
	queries := db.New(database)

	// Release GitHub auth token when program exits
	defer func() {
		if githubAuth != nil {
			if err := queries.ReleaseGithubAuthToken(ctx, githubAuth.ID); err != nil {
				logger.Error("failed to release GitHub auth token", "token_id", githubAuth.ID, "error", err)
			} else {
				logger.Info("released GitHub auth token", "token_id", githubAuth.ID)
			}
		}
	}()

	migration, err := queries.GetGitLabToGithubMigration(ctx)
	if err != nil {
		logger.Error("failed to get github repo", "error", err)
		os.Exit(1)
	}
	mc := &migrationContext{
		migration:  &migration,
		qtx:        queries,
		githubAuth: githubAuth,
	}

	// Execute the appropriate operation based on command line arguments
	if *updateStoredMRs {
		logger.Info("updating stored merge requests from repository analysis")
		if err = updateStoredMergeRequests(ctx, mc); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully updated stored merge requests")
	} else if *migrateMRs {
		logger.Info("starting merge request migration from GitLab to GitHub")
		if err = migrateProject(ctx, mc); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully completed merge request migration")
	}

	if errCount > 0 {
		logger.Warn(fmt.Sprintf("encountered %d errors during operation, review log output for details", errCount))
		os.Exit(1)
	}
}
