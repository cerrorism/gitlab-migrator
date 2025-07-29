package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
)

// RateLimiter implements a leaky bucket rate limiter
type RateLimiter struct {
	tokens    chan struct{}
	ticker    *time.Ticker
	done      chan struct{}
	closeOnce sync.Once
}

// NewRateLimiter creates a new rate limiter
// rate: operations per second
// burstSize: maximum burst capacity (bucket size)
func NewRateLimiter(rate float64, burstSize int) *RateLimiter {
	rl := &RateLimiter{
		tokens: make(chan struct{}, burstSize),
		ticker: time.NewTicker(time.Duration(float64(time.Second) / rate)),
		done:   make(chan struct{}),
	}

	// Fill bucket initially to allow some immediate operations
	for i := 0; i < burstSize; i++ {
		select {
		case rl.tokens <- struct{}{}:
		default:
			break
		}
	}

	// Start the token replenishment goroutine
	go rl.refill()

	return rl
}

// Wait blocks until a token is available, respecting context cancellation
func (rl *RateLimiter) Wait(ctx context.Context) error {
	select {
	case <-rl.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-rl.done:
		return fmt.Errorf("rate limiter closed")
	}
}

// refill adds tokens to the bucket at the specified rate
func (rl *RateLimiter) refill() {
	defer rl.ticker.Stop()

	for {
		select {
		case <-rl.ticker.C:
			// Try to add a token (non-blocking)
			select {
			case rl.tokens <- struct{}{}:
			default:
				// Bucket is full, skip this token
			}
		case <-rl.done:
			return
		}
	}
}

// Close stops the rate limiter
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		close(rl.done)
	})
}

const (
	MergeRequestStatusMRFound             = "MR_FOUND"
	MergeRequestStatusPRCreatedOngoing    = "PR_CREATE_ONGOING"
	MergeRequestStatusPRCreated           = "PR_CREATED"
	MergeRequestStatusPRDiscussionOngoing = "PR_DISCUSSION_ONGOING"
	MergeRequestStatusPRDiscussionCreated = "PR_DISCUSSION_CREATED"
	MergeRequestStatusReleaseOngoing      = "RELEASE_ONGOING"
	MergeRequestStatusReleaseCreated      = "RELEASE_CREATED"
)

func updateStoredMergeRequests(ctx context.Context, mc *migrationContext) error {
	existingMergeCommitSHAs, err := mc.qtx.GetAllGitLabToGithubMigrationSHAs(ctx, mc.migration.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing merge commit SHAs: %v", err)
	}

	mergeRequests, err := getMergeRequestInfoFromLocal(ctx, mc.migration.GithubRepoName)
	if err != nil {
		return fmt.Errorf("failed to get merge requests: %v", err)
	}
	for _, mr := range mergeRequests {
		if slices.Contains(existingMergeCommitSHAs, mr.MergeCommitSHA) {
			continue
		}
		_, err := mc.qtx.CreateGitlabMergeRequest(ctx, db.CreateGitlabMergeRequestParams{
			MigrationID:      mc.migration.ID,
			MrIid:            int64(mr.MrIID),
			MergeCommitSha:   mr.MergeCommitSHA,
			Parent1CommitSha: mr.BaseParent,
			Parent2CommitSha: mr.HeadParent,
			Status:           mr.Status,
		})
		if err != nil {
			return fmt.Errorf("failed to create merge request: %v", err)
		}
	}
	return nil
}

func migrateProject(ctx context.Context, mc *migrationContext, step string) error {
	project, _, err := gl.Projects.GetProject(mc.migration.GitlabProjectName, &gitlab.GetProjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to get gitlab project: %v", err)
	}

	if project == nil {
		return fmt.Errorf("no matching GitLab project found: %s", mc.gitlabProject.Name)
	}
	mc.gitlabProject = project

	migratePullRequests(ctx, mc, step)

	return nil
}

func migratePullRequests(ctx context.Context, mc *migrationContext, step string) {
	// Create rate limiter for 470 PRs/hour with smooth distribution
	// 470 PRs/hour = 0.1306 PRs/second ≈ 1 PR every 7.66 seconds
	// Using 0.125 PRs/second (1 PR every 8 seconds) for extra safety
	// Burst size of 3 allows for some initial rapid processing but prevents spikes
	rateLimiter := NewRateLimiter(0.125, 3)
	defer rateLimiter.Close()

	// Determine operation description based on step
	var operationDesc string
	var sourceStatus string
	var middleStatus string
	var targetStatus string
	switch step {
	case "migrate-mrs":
		operationDesc = "PR migration"
		sourceStatus = MergeRequestStatusMRFound
		middleStatus = MergeRequestStatusPRCreatedOngoing
		targetStatus = MergeRequestStatusPRCreated
	case "migrate-discussions":
		operationDesc = "discussion migration"
		sourceStatus = MergeRequestStatusPRCreated
		middleStatus = MergeRequestStatusPRDiscussionOngoing
		targetStatus = MergeRequestStatusPRDiscussionCreated
	case "migrate-releases":
		operationDesc = "release creation"
		sourceStatus = MergeRequestStatusPRDiscussionCreated
		middleStatus = MergeRequestStatusReleaseOngoing
		targetStatus = MergeRequestStatusReleaseCreated
	default:
		operationDesc = "migration"
	}

	mrs, err := mc.qtx.GetGitlabMergeRequests(ctx, db.GetGitlabMergeRequestsParams{
		MigrationID: mc.migration.ID,
		Limit:       merge_request_limit,
		FromState:   sourceStatus,
		ToState:     middleStatus,
	})
	if err != nil {
		logger.Error("failed to get merge requests", "error", err)
		return
	}

	logger.Info(fmt.Sprintf("starting %s with rate limiting", operationDesc),
		"total_mrs", len(mrs),
		"step", step,
		"rate_limit", "470 operations/hour (1 operation every 8 seconds)",
		"estimated_duration", fmt.Sprintf("%.1f hours", float64(len(mrs))/58.75)) // 470/8 = 58.75 operations per hour

	successCount := 0
	errorCount := 0
	startTime := time.Now()

	for i, mr := range mrs {
		// Wait for rate limiter before processing each MR
		if err := rateLimiter.Wait(ctx); err != nil {
			logger.Error("rate limiter wait failed", "error", err)
			return
		}

		logger.Info(fmt.Sprintf("processing merge request for %s", operationDesc),
			"mr_iid", mr.MrIid,
			"progress", fmt.Sprintf("%d/%d", i+1, len(mrs)),
			"success_count", successCount,
			"error_count", errorCount,
			"step", step)

		mergeRequest, _, err := gl.MergeRequests.GetMergeRequest(mc.gitlabProject.ID, int(mr.MrIid), &gitlab.GetMergeRequestsOptions{})
		if err != nil {
			if errors.Is(err, gitlab.ErrNotFound) {
				logger.Info("skip non-existing merge request", "mr_iid", mr.MrIid)
				continue
			} else {
				sendErr(fmt.Errorf("retrieving gitlab merge request %d: %v", mr.MrIid, err))
				errorCount++
				continue
			}
		}
		if mergeRequest == nil {
			continue
		}

		// Execute the appropriate function based on step
		var result string
		switch step {
		case "migrate-mrs":
			result = migrateSingleMergeRequest(ctx, mc, mergeRequest, &mr)
		case "migrate-discussions":
			err := migrateComments(ctx, mc, mergeRequest, &mr)
			if err != nil {
				result = fmt.Sprintf("failed: %s", err.Error())
			} else {
				result = "success"
			}
		case "migrate-releases":
			err := createReleaseForMergeRequest(ctx, mc, mergeRequest, &mr)
			if err != nil {
				result = fmt.Sprintf("failed: %s", err.Error())
			} else {
				result = "success"
			}
		default:
			result = "failed: unknown step"
		}

		// Track success/failure
		if result == "success" {
			mc.qtx.UpdateGitlabMergeRequestStatus(ctx, db.UpdateGitlabMergeRequestStatusParams{
				ID:     mr.ID,
				Status: targetStatus,
			})
			successCount++
		} else {
			mc.qtx.UpdateGitlabMergeRequestStatus(ctx, db.UpdateGitlabMergeRequestStatusParams{
				ID:     mr.ID,
				Status: targetStatus + "_FAILED",
			})
			errorCount++
		}

		err = mc.qtx.UpdateGitlabMergeRequestNotes(ctx, db.UpdateGitlabMergeRequestNotesParams{
			Notes: result,
			ID:    mr.ID,
		})
		if err != nil {
			logger.Error("failed to update merge request in database", "mr_iid", mr.MrIid, "error", err)
			continue
		}

		// Log progress every 10 MRs
		if (i+1)%10 == 0 {
			elapsed := time.Since(startTime)
			avgRate := float64(i+1) / elapsed.Hours()
			remaining := len(mrs) - (i + 1)
			estimatedRemaining := time.Duration(float64(remaining)/0.125) * time.Second

			logger.Info("migration progress",
				"completed", i+1,
				"total", len(mrs),
				"success", successCount,
				"errors", errorCount,
				"elapsed", elapsed.Round(time.Second),
				"avg_rate_per_hour", fmt.Sprintf("%.1f", avgRate),
				"estimated_remaining", estimatedRemaining.Round(time.Minute),
				"step", step)
		}
	}

	elapsed := time.Since(startTime)
	finalRate := float64(len(mrs)) / elapsed.Hours()

	logger.Info(fmt.Sprintf("%s completed", operationDesc),
		"total_processed", len(mrs),
		"successful", successCount,
		"errors", errorCount,
		"total_time", elapsed.Round(time.Second),
		"final_rate_per_hour", fmt.Sprintf("%.1f", finalRate),
		"step", step)
}
