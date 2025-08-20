package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
)

// RateLimiter implements GitHub API header-based rate limiting
type RateLimiter struct {
	mu                      sync.RWMutex
	resources               map[string]*ResourceRateLimit // Track rate limits per resource
	done                    chan struct{}
	closeOnce               sync.Once
	secondaryRateLimitReset time.Time // Track secondary rate limit reset time
}

// ResourceRateLimit tracks rate limit info for a specific GitHub API resource
type ResourceRateLimit struct {
	Resource  string
	Limit     int
	Remaining int
	Used      int
	Reset     time.Time
	lastCall  time.Time
}

// NewRateLimiter creates a new GitHub API header-based rate limiter
// The rate and burstSize parameters are ignored as we use GitHub's actual rate limits
func NewRateLimiter(rate float64, burstSize int) *RateLimiter {
	return &RateLimiter{
		resources: make(map[string]*ResourceRateLimit),
		done:      make(chan struct{}),
	}
}

// Wait blocks until it's safe to make the next API call based on GitHub rate limits
func (rl *RateLimiter) Wait(ctx context.Context) error {
	return rl.WaitForResource(ctx, "core") // Default to core resource
}

// WaitForResource waits for a specific GitHub API resource to be available
func (rl *RateLimiter) WaitForResource(ctx context.Context, resource string) error {
	rl.mu.RLock()
	resourceLimit := rl.resources[resource]
	secondaryReset := rl.secondaryRateLimitReset
	rl.mu.RUnlock()

	// Check secondary rate limit first (takes precedence)
	if !secondaryReset.IsZero() && time.Now().Before(secondaryReset) {
		waitTime := time.Until(secondaryReset)
		logger.Warn("waiting for GitHub API secondary rate limit",
			"wait_time", waitTime.String(),
			"reset_time", secondaryReset.Format(time.RFC3339))

		select {
		case <-time.After(waitTime):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-rl.done:
			return fmt.Errorf("rate limiter closed")
		}
	}

	// If we don't have rate limit info for this resource, allow the call
	// (the response will provide the rate limit info)
	if resourceLimit == nil {
		return nil
	}

	// Calculate wait time based on primary rate limit headers
	waitTime := rl.calculateWaitTime(resourceLimit)

	if waitTime <= 0 {
		return nil // No wait needed
	}

	logger.Info("waiting for GitHub API primary rate limit",
		"resource", resource,
		"remaining", resourceLimit.Remaining,
		"wait_time", waitTime.String(),
		"reset_time", resourceLimit.Reset.Format(time.RFC3339))

	select {
	case <-time.After(waitTime):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-rl.done:
		return fmt.Errorf("rate limiter closed")
	}
}

// calculateWaitTime calculates how long to wait before the next API call
func (rl *RateLimiter) calculateWaitTime(resourceLimit *ResourceRateLimit) time.Duration {
	now := time.Now()

	// If rate limit has reset, no wait needed
	if now.After(resourceLimit.Reset) {
		return 0
	}

	// If no requests remaining, wait until reset
	if resourceLimit.Remaining <= 0 {
		return time.Until(resourceLimit.Reset)
	}

	// Calculate time until reset
	timeUntilReset := time.Until(resourceLimit.Reset)
	if timeUntilReset <= 0 {
		return 0 // Rate limit should have reset
	}

	// Calculate optimal spacing between requests
	// Example: 300 remaining, 15 minutes left = 15*60/300 = 3 seconds per request
	optimalDelay := timeUntilReset / time.Duration(resourceLimit.Remaining)

	// Ensure minimum delay of 100ms to prevent burst behavior
	minDelay := 100 * time.Millisecond
	if optimalDelay < minDelay {
		optimalDelay = minDelay
	}

	// Check how long since last call for this resource
	timeSinceLastCall := now.Sub(resourceLimit.lastCall)

	// If enough time has passed, no additional wait needed
	if timeSinceLastCall >= optimalDelay {
		return 0
	}

	// Wait for the remaining time
	return optimalDelay - timeSinceLastCall
}

// Close stops the rate limiter
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		close(rl.done)
	})
}

// UpdateFromResponse updates rate limit info from a GitHub API response
func (rl *RateLimiter) UpdateFromResponse(resp *github.Response) {
	if resp == nil || resp.Response == nil {
		return
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Get the resource from the response header
	resource := resp.Header.Get("X-Ratelimit-Resource")
	if resource == "" {
		resource = "core" // Default to core if not specified
	}

	// Parse rate limit headers from the response
	limit := resp.Header.Get("X-Ratelimit-Limit")
	remaining := resp.Header.Get("X-Ratelimit-Remaining")
	used := resp.Header.Get("X-Ratelimit-Used")
	reset := resp.Header.Get("X-Ratelimit-Reset")

	if limit == "" || remaining == "" || reset == "" {
		return // Required headers missing
	}

	// Parse the values
	limitInt, err := strconv.Atoi(limit)
	if err != nil {
		return
	}

	remainingInt, err := strconv.Atoi(remaining)
	if err != nil {
		return
	}

	var usedInt int
	if used != "" {
		usedInt, _ = strconv.Atoi(used)
	}

	resetTime, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return
	}

	// Update or create resource rate limit info
	rl.resources[resource] = &ResourceRateLimit{
		Resource:  resource,
		Limit:     limitInt,
		Remaining: remainingInt,
		Used:      usedInt,
		Reset:     time.Unix(resetTime, 0),
		lastCall:  time.Now(),
	}

	logger.Info("updated rate limit from response",
		"resource", resource,
		"limit", limitInt,
		"remaining", remainingInt,
		"used", usedInt,
		"reset", time.Unix(resetTime, 0).Format(time.RFC3339))

	// Check for secondary rate limit indicators
	rl.checkSecondaryRateLimit(resp.Response)
}

// checkSecondaryRateLimit checks for secondary rate limit headers and updates accordingly
func (rl *RateLimiter) checkSecondaryRateLimit(resp *http.Response) {
	if resp == nil {
		return
	}

	// Check for Retry-After header (secondary rate limit)
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		if retrySeconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil {
			resetTime := time.Now().Add(time.Duration(retrySeconds) * time.Second)
			rl.secondaryRateLimitReset = resetTime

			logger.Warn("secondary rate limit detected",
				"retry_after_seconds", retrySeconds,
				"reset_time", resetTime.Format(time.RFC3339))
		}
	}
}

// UpdateFromError updates rate limiter state from GitHub API errors
func (rl *RateLimiter) UpdateFromError(err error) {
	if err == nil {
		return
	}

	// Check if this is a github.AbuseRateLimitError
	if rateLimitErr, ok := err.(*github.AbuseRateLimitError); ok {
		rl.mu.Lock()
		defer rl.mu.Unlock()

		if rateLimitErr.RetryAfter != nil {
			rl.secondaryRateLimitReset = time.Now().Add(*rateLimitErr.RetryAfter)

			logger.Warn("abuse/secondary rate limit error detected",
				"retry_after", rateLimitErr.RetryAfter.String(),
				"reset_time", rl.secondaryRateLimitReset.Format(time.RFC3339),
				"message", rateLimitErr.Message)
		}
	}

	// Also check for github.RateLimitError which might contain reset time
	if rateLimitErr, ok := err.(*github.RateLimitError); ok {
		rl.mu.Lock()
		defer rl.mu.Unlock()

		// Use the reset time from the rate limit error
		rl.secondaryRateLimitReset = rateLimitErr.Rate.Reset.Time

		logger.Warn("primary rate limit error detected",
			"reset_time", rateLimitErr.Rate.Reset.Time.Format(time.RFC3339),
			"message", rateLimitErr.Message)
	}
}

// GetRateLimitStatus returns current rate limit status for a resource
func (rl *RateLimiter) GetRateLimitStatus(resource string) *ResourceRateLimit {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	if resourceLimit, exists := rl.resources[resource]; exists {
		// Return a copy to avoid race conditions
		copy := *resourceLimit
		return &copy
	}
	return nil
}

// GetSecondaryRateLimitStatus returns the secondary rate limit reset time
func (rl *RateLimiter) GetSecondaryRateLimitStatus() time.Time {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.secondaryRateLimitReset
}

const (
	MergeRequestStatusMRFound           = "MR_FOUND"
	MergeRequestStatusPRCreatedOngoing  = "PR_CREATE_ONGOING"
	MergeRequestStatusPRCreated         = "PR_CREATED"
	MergeRequestStatusDiscussionOngoing = "DISCUSSION_ONGOING"
	MergeRequestStatusDiscussionCreated = "DISCUSSION_CREATED"
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

func migrateMergeRequests(ctx context.Context, mc *migrationContext, step string) error {
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
	// Create rate limiter for 470 GitHub resources/hour with smooth distribution
	// This includes PRs, comments, refs, tags, and releases to stay within GitHub API limits
	// 470 resources/hour = 0.1306 resources/second ≈ 1 resource every 7.66 seconds
	// Using 0.125 resources/second (1 resource every 8 seconds) for extra safety
	// Burst size of 3 allows for some initial rapid processing but prevents spikes
	mc.rateLimiter = NewRateLimiter(0.125, 3)
	defer mc.rateLimiter.Close()

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
	case "migrate-mrs-retry":
		operationDesc = "PR retry migration"
		sourceStatus = MergeRequestStatusPRCreated + "_FAILED"
		middleStatus = MergeRequestStatusPRCreatedOngoing
		targetStatus = MergeRequestStatusPRCreated
	case "migrate-discussions":
		operationDesc = "discussion migration"
		sourceStatus = MergeRequestStatusPRCreated
		middleStatus = MergeRequestStatusDiscussionOngoing
		targetStatus = MergeRequestStatusDiscussionCreated
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

	logArgs := []interface{}{
		"total_mrs", len(mrs),
		"step", step,
		"rate_limiting", "Dynamic based on GitHub API response headers",
	}

	if step == "migrate-mrs-retry" {
		logArgs = append(logArgs, "retry_source_status", sourceStatus)
	}

	logger.Info(fmt.Sprintf("starting %s with GitHub API header-based rate limiting", operationDesc), logArgs...)

	successCount := 0
	errorCount := 0
	startTime := time.Now()

	for i, mr := range mrs {
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
		case "migrate-mrs", "migrate-mrs-retry":
			result = migrateSingleMergeRequest(ctx, mc, mergeRequest, &mr)
		case "migrate-discussions":
			err := migrateComments(ctx, mc, mergeRequest, &mr)
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

			// Get current rate limit status
			coreStatus := mc.rateLimiter.GetRateLimitStatus("core")
			secondaryReset := mc.rateLimiter.GetSecondaryRateLimitStatus()
			rateLimitInfo := []interface{}{}
			if coreStatus != nil {
				rateLimitInfo = append(rateLimitInfo,
					"rate_limit_remaining", coreStatus.Remaining,
					"rate_limit_reset", coreStatus.Reset.Format("15:04:05"))
			}
			if !secondaryReset.IsZero() && time.Now().Before(secondaryReset) {
				rateLimitInfo = append(rateLimitInfo,
					"secondary_rate_limit_reset", secondaryReset.Format("15:04:05"))
			}

			logArgs := []interface{}{
				"completed", i + 1,
				"total", len(mrs),
				"success", successCount,
				"errors", errorCount,
				"elapsed", elapsed.Round(time.Second),
				"avg_rate_per_hour", fmt.Sprintf("%.1f", avgRate),
				"step", step,
			}
			logArgs = append(logArgs, rateLimitInfo...)

			logger.Info("migration progress", logArgs...)
		}
	}

	elapsed := time.Since(startTime)
	finalRate := float64(len(mrs)) / elapsed.Hours()

	// Get final rate limit status
	finalLogArgs := []interface{}{
		"total_processed", len(mrs),
		"successful", successCount,
		"errors", errorCount,
		"total_time", elapsed.Round(time.Second),
		"final_rate_per_hour", fmt.Sprintf("%.1f", finalRate),
		"step", step,
	}

	coreStatus := mc.rateLimiter.GetRateLimitStatus("core")
	secondaryReset := mc.rateLimiter.GetSecondaryRateLimitStatus()
	if coreStatus != nil {
		finalLogArgs = append(finalLogArgs,
			"final_rate_limit_remaining", coreStatus.Remaining,
			"rate_limit_reset", coreStatus.Reset.Format("15:04:05"))
	}
	if !secondaryReset.IsZero() && time.Now().Before(secondaryReset) {
		finalLogArgs = append(finalLogArgs,
			"secondary_rate_limit_reset", secondaryReset.Format("15:04:05"))
	}

	logger.Info(fmt.Sprintf("%s completed", operationDesc), finalLogArgs...)
}
