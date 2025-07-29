package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v5"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
)

const (
	dateFormat          = "Mon, 2 Jan 2006"
	dbString            = "user=postgres password=password dbname=postgres sslmode=false"
	merge_request_limit = 10000
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

// Enhanced error function that also stores errors in the database
func recordError(ctx context.Context, qtx *db.Queries, mergeRequestID int64, err error) {
	errCount++
	logger.Error(err.Error())

	// Store error in database
	if qtx != nil && mergeRequestID > 0 {
		_, dbErr := qtx.CreateMergeRequestNote(ctx, db.CreateMergeRequestNoteParams{
			MergeRequestID: mergeRequestID,
			NoteType:       "error",
			Message:        err.Error(),
		})
		if dbErr != nil {
			logger.Error("failed to store error in database", "db_error", dbErr.Error(), "original_error", err.Error())
		}
	}
}

// Enhanced info function for storing informational notes
func recordInfo(ctx context.Context, qtx *db.Queries, mergeRequestID int64, message string) {
	logger.Info(message)

	// Store info in database
	if qtx != nil && mergeRequestID > 0 {
		_, dbErr := qtx.CreateMergeRequestNote(ctx, db.CreateMergeRequestNoteParams{
			MergeRequestID: mergeRequestID,
			NoteType:       "info",
			Message:        message,
		})
		if dbErr != nil {
			logger.Error("failed to store info in database", "db_error", dbErr.Error(), "message", message)
		}
	}
}

// Enhanced warning function for storing warnings
func recordWarning(ctx context.Context, qtx *db.Queries, mergeRequestID int64, message string) {
	logger.Warn(message)

	// Store warning in database
	if qtx != nil && mergeRequestID > 0 {
		_, dbErr := qtx.CreateMergeRequestNote(ctx, db.CreateMergeRequestNoteParams{
			MergeRequestID: mergeRequestID,
			NoteType:       "warning",
			Message:        message,
		})
		if dbErr != nil {
			logger.Error("failed to store warning in database", "db_error", dbErr.Error(), "message", message)
		}
	}
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
	var step = flag.String("step", "", "Migration step: update-stored-mrs, migrate-mrs, migrate-discussions, migrate-releases")
	flag.Parse()

	// Validate step parameter
	validSteps := []string{"update-stored-mrs", "migrate-mrs", "migrate-discussions", "migrate-releases"}
	isValidStep := false
	for _, validStep := range validSteps {
		if *step == validStep {
			isValidStep = true
			break
		}
	}

	if !isValidStep {
		logger.Error("invalid step specified", "step", *step, "valid_steps", validSteps)
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

	// Execute the appropriate operation based on step parameter
	switch *step {
	case "update-stored-mrs":
		logger.Info("updating stored merge requests from repository analysis")
		if err = updateStoredMergeRequests(ctx, mc); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully updated stored merge requests")
	case "migrate-mrs":
		logger.Info("starting merge request migration from GitLab to GitHub")
		if err = migrateProject(ctx, mc, *step); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully completed merge request migration")
	case "migrate-discussions":
		logger.Info("starting discussion migration from GitLab to GitHub")
		if err = migrateProject(ctx, mc, *step); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully completed discussion migration")
	case "migrate-releases":
		logger.Info("starting release creation for merge requests")
		if err = migrateProject(ctx, mc, *step); err != nil {
			sendErr(err)
			os.Exit(1)
		}
		logger.Info("successfully completed release creation")
	}

	if errCount > 0 {
		logger.Warn(fmt.Sprintf("encountered %d errors during operation, review log output for details", errCount))
		os.Exit(1)
	}
}
