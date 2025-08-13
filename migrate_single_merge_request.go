package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/manicminer/gitlab-migrator/db"

	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"
)

// createGitHubRef creates a GitHub ref (branch) pointing to a specific commit SHA
func createGitHubRef(ctx context.Context, owner, repo, refName, commitSHA string, mc *migrationContext) error {
	return createGitHubRefWithType(ctx, owner, repo, refName, commitSHA, "heads", mc)
}

// createGitHubRefWithType creates a GitHub ref of the specified type pointing to a specific commit SHA
func createGitHubRefWithType(ctx context.Context, owner, repo, refName, commitSHA, refType string, mc *migrationContext) error {
	fullRefName := "refs/" + refType + "/" + refName

	// Apply rate limiting before creating GitHub resource
	if err := mc.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter wait failed: %v", err)
	}

	// Create ref directly (no existence check for speed)
	_, resp, err := gh.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: &fullRefName,
		Object: &github.GitObject{
			SHA: &commitSHA,
		},
	})
	if err != nil {
		// Update rate limiter with error info (might contain secondary rate limit)
		mc.rateLimiter.UpdateFromError(err)
		return fmt.Errorf("creating ref %s: %v", refName, err)
	}

	// Update rate limiter with response headers
	mc.rateLimiter.UpdateFromResponse(resp)

	logger.Info("created ref", "ref", refName, "type", refType, "sha", commitSHA)
	return nil
}

// deleteGitHubRef deletes a GitHub ref
func deleteGitHubRef(ctx context.Context, owner, repo, refName string) error {
	_, err := gh.Git.DeleteRef(ctx, owner, repo, "heads/"+refName)
	if err != nil {
		return fmt.Errorf("deleting ref %s: %v", refName, err)
	}
	logger.Info("deleted ref", "ref", refName)
	return nil
}

// migrateComments migrates comments from a GitLab merge request to a GitHub pull request
func migrateComments(ctx context.Context, mc *migrationContext, mergeRequest *gitlab.MergeRequest, mrInDB *db.GitlabMergeRequest) error {
	githubPath := strings.Split(mc.migration.GithubRepoName, "/")
	if len(githubPath) != 2 {
		return fmt.Errorf("failed: invalid github repo name format: %s", mc.migration.GithubRepoName)
	}
	owner, repoName := githubPath[0], githubPath[1]
	var comments []*gitlab.Note
	skipComments := false
	opts := &gitlab.ListMergeRequestNotesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	// Fetch all GitLab merge request notes
	for {
		result, resp, err := gl.Notes.ListMergeRequestNotes(mc.gitlabProject.ID, mergeRequest.IID, opts)
		if err != nil {
			recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("listing merge request notes: %v", err))
			skipComments = true
			break
		}

		comments = append(comments, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	prNumber := int(mrInDB.PrID)
	if !skipComments {
		logger.Info("retrieving GitHub pull request comments", "owner", owner, "repo", repoName, "pr_number", mrInDB.PrID)

		// Check original GitLab MR commits to determine if we can use inline comments
		gitlabCommits, _, err := gl.MergeRequests.GetMergeRequestCommits(mc.gitlabProject.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{})
		if err != nil {
			return fmt.Errorf("listing GitLab merge request commits: %v", err)
		}

		// Determine if we can use inline comments based on original GitLab MR
		var inlineCommitSHA string
		canCreateInlineComments := false
		if len(gitlabCommits) == 1 {
			// Single commit in GitLab MR - use the source commit SHA from database
			inlineCommitSHA = mrInDB.Parent2CommitSha
			canCreateInlineComments = true
			logger.Info("GitLab MR had single commit, will use inline comments", "gitlab_commits", len(gitlabCommits), "commit_sha", inlineCommitSHA)
		} else {
			logger.Info("GitLab MR had multiple commits, will use PR-level comments only", "gitlab_commits", len(gitlabCommits))
		}

		// Get existing comments - we need both types now
		prComments, resp, err := gh.Issues.ListComments(ctx, owner, repoName, prNumber, &github.IssueListCommentsOptions{})
		if err != nil {
			return fmt.Errorf("listing pull request comments: %v", err)
		}
		mc.rateLimiter.UpdateFromResponse(resp)

		var prReviewComments []*github.PullRequestComment
		if canCreateInlineComments {
			prReviewComments, _, err = gh.PullRequests.ListComments(ctx, owner, repoName, prNumber, &github.PullRequestListCommentsOptions{})
			if err != nil {
				return fmt.Errorf("listing pull request review comments: %v", err)
			}
		}

		logger.Info("migrating merge request comments from GitLab to GitHub", "owner", owner, "repo", repoName, "pr_number", mrInDB.PrID, "count", len(comments))

		for _, comment := range comments {
			if comment == nil || comment.System {
				continue
			}

			githubCommentAuthorName := comment.Author.Name

			commentAuthor, err := getGitlabUser(comment.Author.Username)
			if err != nil {
				recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("retrieving gitlab user: %v", err))
				break
			}
			if commentAuthor.WebsiteURL != "" {
				githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
			}

			// Build comment body with comprehensive author information
			authorInfo := githubCommentAuthorName
			if comment.Author.Username != "" && githubCommentAuthorName != "@"+comment.Author.Username {
				// Include original GitLab username if different from GitHub username
				authorInfo = fmt.Sprintf("%s (GitLab: @%s)", githubCommentAuthorName, comment.Author.Username)
			}

			// Add comment creation date
			commentDate := ""
			if comment.CreatedAt != nil {
				commentDate = fmt.Sprintf(" on %s", comment.CreatedAt.Format("Jan 2, 2006"))
			}

			var commentBody string
			isInlineComment := comment.Position != nil && comment.Position.NewPath != ""
			willCreateInlineComment := isInlineComment && canCreateInlineComments

			// Check if this is an inline comment (diff note) with position information
			if isInlineComment {
				// This is an inline comment on a specific file/line
				lineInfo := ""
				if comment.Position.NewLine > 0 {
					lineInfo = fmt.Sprintf(" (line %d)", comment.Position.NewLine)
				} else if comment.Position.OldLine > 0 {
					lineInfo = fmt.Sprintf(" (line %d in old version)", comment.Position.OldLine)
				}

				if willCreateInlineComment {
					// For single-commit GitLab MRs, create true inline comments
					commentBody = fmt.Sprintf("**%s**%s:\n\n%s", authorInfo, commentDate, comment.Body)
				} else {
					// For multi-commit GitLab MRs, include file/line info in the comment body
					commentBody = fmt.Sprintf("**%s** commented on `%s`%s%s:\n\n%s",
						authorInfo, comment.Position.NewPath, lineInfo, commentDate, comment.Body)
				}
			} else {
				// Regular discussion comment
				commentBody = fmt.Sprintf("**%s**%s:\n\n%s", authorInfo, commentDate, comment.Body)
			}

			// Create unique identifier for this comment to detect existing ones
			commentIdentifier := fmt.Sprintf("gitlab-comment-%d", comment.ID)

			foundExistingComment := false

			// Check appropriate comment type for existing ones
			if willCreateInlineComment {
				// Check review comments for inline comments
				for _, prComment := range prReviewComments {
					if prComment == nil {
						continue
					}

					if strings.Contains(prComment.GetBody(), commentIdentifier) {
						foundExistingComment = true

						if prComment.Body == nil || *prComment.Body != commentBody {
							logger.Info("updating pull request review comment", "owner", owner, "repo", repoName, "pr_number", prNumber, "comment_id", prComment.GetID())
							prComment.Body = &commentBody
							if _, _, err = gh.PullRequests.EditComment(ctx, owner, repoName, prComment.GetID(), prComment); err != nil {
								return fmt.Errorf("updating pull request review comment: %v", err)
							}
						}
						break
					}
				}
			} else {
				// Check issue comments for PR-level comments
				for _, prComment := range prComments {
					if prComment == nil {
						continue
					}

					if strings.Contains(prComment.GetBody(), commentIdentifier) {
						foundExistingComment = true

						if prComment.Body == nil || *prComment.Body != commentBody {
							logger.Info("updating pull request comment", "owner", owner, "repo", repoName, "pr_number", prNumber, "comment_id", prComment.GetID())
							prComment.Body = &commentBody
							if _, _, err = gh.Issues.EditComment(ctx, owner, repoName, prComment.GetID(), prComment); err != nil {
								return fmt.Errorf("updating pull request comment: %v", err)
							}
						}
						break
					}
				}
			}

			if !foundExistingComment {
				// Add the identifier as a hidden HTML comment for future detection
				commentBodyWithIdentifier := fmt.Sprintf("%s\n\n<!-- %s -->", commentBody, commentIdentifier)

				logger.Info("creating pull request comment", "owner", owner, "repo", repoName, "pr_number", prNumber, "is_inline", willCreateInlineComment)

				if willCreateInlineComment {
					// Create inline review comment for single-commit GitLab MRs
					newComment := &github.PullRequestComment{
						Body:     &commentBodyWithIdentifier,
						CommitID: &inlineCommitSHA,
						Path:     &comment.Position.NewPath,
					}

					// Set line and side information
					if comment.Position.NewLine > 0 {
						newComment.Line = &comment.Position.NewLine
						newComment.Side = pointer("RIGHT")
					} else if comment.Position.OldLine > 0 {
						newComment.Line = &comment.Position.OldLine
						newComment.Side = pointer("LEFT")
					}

					// Try to create inline comment, fallback to PR-level if it fails
					// Apply rate limiting before creating GitHub resource
					if err := mc.rateLimiter.Wait(ctx); err != nil {
						return fmt.Errorf("rate limiter wait failed: %v", err)
					}

					_, _, err = gh.PullRequests.CreateComment(ctx, owner, repoName, prNumber, newComment)
					if err != nil {
						// Inline comment failed (likely line not in diff), fallback to PR-level comment
						logger.Warn("inline comment creation failed, falling back to PR-level comment",
							"error", err, "file", comment.Position.NewPath, "line", comment.Position.NewLine)

						// Create fallback comment body with file/line info
						lineInfo := ""
						if comment.Position.NewLine > 0 {
							lineInfo = fmt.Sprintf(" (line %d)", comment.Position.NewLine)
						} else if comment.Position.OldLine > 0 {
							lineInfo = fmt.Sprintf(" (line %d in old version)", comment.Position.OldLine)
						}

						fallbackBody := fmt.Sprintf("**%s** commented on `%s`%s%s:\n\n%s\n\n<!-- %s -->",
							authorInfo, comment.Position.NewPath, lineInfo, commentDate, comment.Body, commentIdentifier)

						// Create PR-level comment as fallback
						fallbackComment := &github.IssueComment{
							Body: &fallbackBody,
						}

						// Apply rate limiting before creating fallback GitHub resource
						if err := mc.rateLimiter.Wait(ctx); err != nil {
							return fmt.Errorf("rate limiter wait failed: %v", err)
						}

						if _, _, err = gh.Issues.CreateComment(ctx, owner, repoName, prNumber, fallbackComment); err != nil {
							return fmt.Errorf("creating fallback pull request comment: %v", err)
						}
					}
				} else {
					// Create PR-level comment using Issues API
					newComment := &github.IssueComment{
						Body: &commentBodyWithIdentifier,
					}

					// Apply rate limiting before creating GitHub resource
					if err := mc.rateLimiter.Wait(ctx); err != nil {
						return fmt.Errorf("rate limiter wait failed: %v", err)
					}

					if _, _, err = gh.Issues.CreateComment(ctx, owner, repoName, prNumber, newComment); err != nil {
						return fmt.Errorf("creating pull request comment: %v", err)
					}
				}
			}
		}
	}

	return nil
}

func migrateSingleMergeRequest(ctx context.Context, mc *migrationContext, mergeRequest *gitlab.MergeRequest, mrInDB *db.GitlabMergeRequest) string {
	if err := ctx.Err(); err != nil {
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("preparing to list pull requests: %v", err))
		return "failed"
	}

	var pullRequest *github.PullRequest

	githubPath := strings.Split(mc.migration.GithubRepoName, "/")
	if len(githubPath) != 2 {
		return fmt.Sprintf("failed: invalid github repo name format: %s", mc.migration.GithubRepoName)
	}
	owner, repoName := githubPath[0], githubPath[1]

	// Generate temporary branch names for merged MRs (all MRs are merged since they're from master branch)
	sourceBranch := fmt.Sprintf("migration-source-%d", mergeRequest.IID)
	targetBranch := fmt.Sprintf("migration-target-%d", mergeRequest.IID)

	// Validate we have the necessary commit SHAs
	if mrInDB.Parent1CommitSha == "" || mrInDB.Parent2CommitSha == "" {
		return "failed: missing commit SHAs in database"
	}

	logger.Info("creating temporary refs for merged MR",
		"mr_iid", mergeRequest.IID,
		"source_ref", sourceBranch,
		"target_ref", targetBranch,
		"source_sha", mrInDB.Parent2CommitSha,
		"target_sha", mrInDB.Parent1CommitSha)

	// Create target ref (base branch) pointing to Parent1CommitSha
	if err := createGitHubRef(ctx, owner, repoName, targetBranch, mrInDB.Parent1CommitSha, mc); err != nil {
		return fmt.Sprintf("failed: creating target ref - %s", err.Error())
	}

	// Create source ref (head branch) pointing to Parent2CommitSha
	if err := createGitHubRef(ctx, owner, repoName, sourceBranch, mrInDB.Parent2CommitSha, mc); err != nil {
		return fmt.Sprintf("failed: creating source ref - %s", err.Error())
	}

	// Get GitLab user information for the author
	githubAuthorName := mergeRequest.Author.Name
	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("retrieving gitlab user: %v", err))
		return "failed: retrieving gitlab user"
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	// Build PR description with migration metadata
	originalState := "> This merge request was originally **merged** on GitLab"

	description := mergeRequest.Description
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	// All MRs are merged since they're from master branch
	mergedDate := ""
	if mergeRequest.MergedAt != nil {
		mergedDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(dateFormat))
	}

	mergeRequestTitle := mergeRequest.Title
	if len(mergeRequestTitle) > 40 {
		mergeRequestTitle = mergeRequestTitle[:40] + "..."
	}

	gitlabPath := strings.Split(mc.migration.GitlabProjectName, "/")
	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | [%[4]s/%[5]s](https://%[9]s/%[4]s/%[5]s) |
> | **GitLab Merge Request** | [%[10]s](https://%[9]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **GitLab MR Number** | [%[2]d](https://%[9]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Date Originally Opened** | %[6]s |%[7]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format(dateFormat), mergedDate, originalState, "gitlab.com", mergeRequestTitle)

	// Create the pull request
	logger.Info("creating pull request", "source_branch", sourceBranch, "target_branch", targetBranch)
	newPullRequest := github.NewPullRequest{
		Title:               &mergeRequest.Title,
		Head:                &sourceBranch,
		Base:                &targetBranch,
		Body:                &body,
		MaintainerCanModify: pointer(true),
		Draft:               &mergeRequest.Draft,
	}

	// Apply rate limiting before creating GitHub PR
	if err := mc.rateLimiter.Wait(ctx); err != nil {
		return fmt.Sprintf("failed: rate limiter wait failed - %s", err.Error())
	}

	var resp *github.Response
	if pullRequest, resp, err = gh.PullRequests.Create(ctx, owner, repoName, &newPullRequest); err != nil {
		// Update rate limiter with error info (might contain secondary rate limit)
		mc.rateLimiter.UpdateFromError(err)
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("creating pull request: %v", err))
		requestStr, _ := json.MarshalIndent(newPullRequest, "", "  ")
		logger.Error(fmt.Sprintf("request: %s", requestStr))
		return fmt.Sprintf("failed: creating pull request- %s", err.Error())
	}

	// Update rate limiter with response headers from PR creation
	mc.rateLimiter.UpdateFromResponse(resp)

	// Store PR in database
	err = mc.qtx.UpdateGitlabMergeRequestPRID(ctx, db.UpdateGitlabMergeRequestPRIDParams{
		PrID: int64(pullRequest.GetNumber()),
		ID:   mrInDB.ID,
	})
	if err != nil {
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("create pull request in DB - PR %d: %v", pullRequest.GetNumber(), err))
		return fmt.Sprintf("failed: create pull request in DB - PR %d", pullRequest.GetNumber())
	}

	// Record successful PR creation
	recordInfo(ctx, mc.qtx, mrInDB.ID, fmt.Sprintf("successfully created pull request #%d", pullRequest.GetNumber()))

	// Update mrInDB with the new PR ID
	mrInDB.PrID = int64(pullRequest.GetNumber())

	// Merge PR (all MRs are merged since they're from master branch)
	logger.Info("merging pull request using squash and merge", "owner", owner, "repo", repoName, "pr_number", pullRequest.GetNumber())

	mergeOptions := &github.PullRequestOptions{
		MergeMethod: "squash", // Use squash and merge
	}

	mergeResult, _, err := gh.PullRequests.Merge(ctx, owner, repoName, pullRequest.GetNumber(), "", mergeOptions)
	if err != nil {
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("merging pull request: %v", err))
		return fmt.Sprintf("failed: merging pull request - %s", err.Error())
	}

	logger.Info("successfully merged pull request",
		"owner", owner,
		"repo", repoName,
		"pr_number", pullRequest.GetNumber(),
		"merge_sha", mergeResult.GetSHA())

	// Record successful merge
	recordInfo(ctx, mc.qtx, mrInDB.ID, fmt.Sprintf("successfully merged pull request #%d with SHA %s", pullRequest.GetNumber(), mergeResult.GetSHA()))

	// Clean up temporary refs after merging
	logger.Info("deleting temporary refs for merged pull request", "owner", owner, "repo", repoName, "pr_number", pullRequest.GetNumber(), "source_ref", sourceBranch, "target_ref", targetBranch)

	if err := deleteGitHubRef(ctx, owner, repoName, sourceBranch); err != nil {
		recordWarning(ctx, mc.qtx, mrInDB.ID, fmt.Sprintf("deleting source ref: %v", err))
	}

	if err := deleteGitHubRef(ctx, owner, repoName, targetBranch); err != nil {
		recordWarning(ctx, mc.qtx, mrInDB.ID, fmt.Sprintf("deleting target ref: %v", err))
	}

	return "success"
}
