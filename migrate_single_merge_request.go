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
func createGitHubRef(ctx context.Context, owner, repo, refName, commitSHA string) error {
	return createGitHubRefWithType(ctx, owner, repo, refName, commitSHA, "heads")
}

// createGitHubTag creates a GitHub tag pointing to a specific commit SHA
func createGitHubTag(ctx context.Context, owner, repo, tagName, commitSHA string) error {
	return createGitHubRefWithType(ctx, owner, repo, tagName, commitSHA, "tags")
}

// createGitHubRefWithType creates a GitHub ref of the specified type pointing to a specific commit SHA
func createGitHubRefWithType(ctx context.Context, owner, repo, refName, commitSHA, refType string) error {
	fullRefName := "refs/" + refType + "/" + refName

	// Create ref directly (no existence check for speed)
	_, _, err := gh.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: &fullRefName,
		Object: &github.GitObject{
			SHA: &commitSHA,
		},
	})
	if err != nil {
		return fmt.Errorf("creating ref %s: %v", refName, err)
	}

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

// createReleaseForMergeRequest creates a GitHub release for a migrated merge request
func createReleaseForMergeRequest(ctx context.Context, mc *migrationContext, mergeRequest *gitlab.MergeRequest, mrInDB *db.GitlabMergeRequest) error {
	githubPath := strings.Split(mc.migration.GithubRepoName, "/")
	if len(githubPath) != 2 {
		return fmt.Errorf("invalid github repo name format: %s", mc.migration.GithubRepoName)
	}
	owner, repoName := githubPath[0], githubPath[1]

	// Create tag name
	tagName := fmt.Sprintf("GitLab_MR_%d", mergeRequest.IID)

	logger.Info("creating release for merge request",
		"mr_iid", mergeRequest.IID,
		"tag", tagName,
		"commit_sha", mrInDB.MergeCommitSha)

	// Create tag using the reusable createGitHubTag function
	err := createGitHubTag(ctx, owner, repoName, tagName, mrInDB.MergeCommitSha)
	if err != nil {
		return fmt.Errorf("creating tag: %v", err)
	}

	// Get all commits between base and head parents
	commitsList, err := getCommitsBetweenParents(ctx, owner, repoName, mrInDB.Parent1CommitSha, mrInDB.Parent2CommitSha)
	if err != nil {
		logger.Warn("failed to get commits between parents, continuing without commit list",
			"error", err, "base", mrInDB.Parent1CommitSha, "head", mrInDB.Parent2CommitSha)
		commitsList = []string{} // Continue with empty list
	}

	// Build release body with MR information
	gitlabPath := strings.Split(mc.migration.GitlabProjectName, "/")

	// Format merge request state information
	stateInfo := ""
	if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		stateInfo = fmt.Sprintf("**Status:** Merged on %s\n\n", mergeRequest.MergedAt.Format(dateFormat))
	} else if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		stateInfo = fmt.Sprintf("**Status:** Closed on %s\n\n", mergeRequest.ClosedAt.Format(dateFormat))
	} else {
		stateInfo = fmt.Sprintf("**Status:** %s\n\n", strings.Title(mergeRequest.State))
	}

	// Get author information
	githubAuthorName := mergeRequest.Author.Name
	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err == nil && author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	// Format commits list
	commitsInfo := ""
	if len(commitsList) > 0 {
		commitsInfo = "\n## Commits Included\n\n"
		for i, commitSHA := range commitsList {
			if i < 10 { // Limit to first 10 commits to avoid overly long release notes
				commitsInfo += fmt.Sprintf("- [`%s`](https://github.com/%s/commit/%s)\n", commitSHA[:8], mc.migration.GithubRepoName, commitSHA)
			} else if i == 10 {
				commitsInfo += fmt.Sprintf("- ... and %d more commits\n", len(commitsList)-10)
				break
			}
		}
		commitsInfo += fmt.Sprintf("\n**Total commits:** %d\n", len(commitsList))
	}

	releaseBody := fmt.Sprintf(`# GitLab Merge Request %d

%s**Original Author:** %s

**Original GitLab Link:** [%s/%s!%d](https://%s/%s/%s/-/merge_requests/%d)

**GitHub Pull Request:** [#%d](https://github.com/%s/pull/%d)

## Merge Information

- **Merge Commit:** [%s](https://github.com/%s/commit/%s)
- **Target Branch:** %s
- **Source Branch:** [%s](https://github.com/%s/commit/%s)%s

---

> This release was automatically created during GitLab to GitHub migration to preserve merge request history.
`,
		mergeRequest.IID,
		stateInfo,
		githubAuthorName,
		gitlabPath[0], gitlabPath[1], mergeRequest.IID,
		"gitlab.com", gitlabPath[0], gitlabPath[1], mergeRequest.IID,
		mrInDB.PrID, mc.migration.GithubRepoName, mrInDB.PrID,
		mrInDB.MergeCommitSha[:8], mc.migration.GithubRepoName, mrInDB.MergeCommitSha,
		mergeRequest.TargetBranch,
		mrInDB.Parent2CommitSha[:8], mc.migration.GithubRepoName, mrInDB.Parent2CommitSha,
		commitsInfo)

	// Create release
	releaseName := fmt.Sprintf("GitLab MR %d: %s", mergeRequest.IID, mergeRequest.Title)
	if len(releaseName) > 100 {
		// Truncate title if too long
		maxTitleLen := 100 - len(fmt.Sprintf("GitLab MR %d: ", mergeRequest.IID)) - 3
		releaseName = fmt.Sprintf("GitLab MR %d: %s...", mergeRequest.IID, mergeRequest.Title[:maxTitleLen])
	}

	release := &github.RepositoryRelease{
		TagName:    &tagName,
		Name:       &releaseName,
		Body:       &releaseBody,
		Draft:      pointer(false),
		Prerelease: pointer(true), // Mark as prerelease since these are MR releases
	}

	createdRelease, _, err := gh.Repositories.CreateRelease(ctx, owner, repoName, release)
	if err != nil {
		return fmt.Errorf("creating release: %v", err)
	}

	logger.Info("successfully created release for merge request",
		"mr_iid", mergeRequest.IID,
		"tag", tagName,
		"release_id", createdRelease.GetID(),
		"pr_id", mrInDB.PrID)

	return nil
}

// getCommitsBetweenParents gets all commit SHAs between base and head parents
func getCommitsBetweenParents(ctx context.Context, owner, repo, baseSHA, headSHA string) ([]string, error) {
	// Use GitHub's compare API to get commits between base and head
	comparison, _, err := gh.Repositories.CompareCommits(ctx, owner, repo, baseSHA, headSHA, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("comparing commits: %v", err)
	}

	commits := make([]string, 0, len(comparison.Commits))
	for _, commit := range comparison.Commits {
		if commit.SHA != nil {
			commits = append(commits, *commit.SHA)
		}
	}

	return commits, nil
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
		prComments, _, err := gh.Issues.ListComments(ctx, owner, repoName, prNumber, &github.IssueListCommentsOptions{})
		if err != nil {
			return fmt.Errorf("listing pull request comments: %v", err)
		}

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

						if _, _, err = gh.Issues.CreateComment(ctx, owner, repoName, prNumber, fallbackComment); err != nil {
							return fmt.Errorf("creating fallback pull request comment: %v", err)
						}
					}
				} else {
					// Create PR-level comment using Issues API
					newComment := &github.IssueComment{
						Body: &commentBodyWithIdentifier,
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
	if err := createGitHubRef(ctx, owner, repoName, targetBranch, mrInDB.Parent1CommitSha); err != nil {
		return fmt.Sprintf("failed: creating target ref - %s", err.Error())
	}

	// Create source ref (head branch) pointing to Parent2CommitSha
	if err := createGitHubRef(ctx, owner, repoName, sourceBranch, mrInDB.Parent2CommitSha); err != nil {
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

	if pullRequest, _, err = gh.PullRequests.Create(ctx, owner, repoName, &newPullRequest); err != nil {
		recordError(ctx, mc.qtx, mrInDB.ID, fmt.Errorf("creating pull request: %v", err))
		requestStr, _ := json.MarshalIndent(newPullRequest, "", "  ")
		logger.Error(fmt.Sprintf("request: %s", requestStr))
		return fmt.Sprintf("failed: creating pull request- %s", err.Error())
	}

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

	// Update mrInDB with the new PR ID for comment migration
	mrInDB.PrID = int64(pullRequest.GetNumber())

	// Migrate comments while PR is still open
	logger.Info("migrating comments for pull request", "owner", owner, "repo", repoName, "pr_number", pullRequest.GetNumber())
	if err := migrateComments(ctx, mc, mergeRequest, mrInDB); err != nil {
		// Log error but don't fail the entire migration - comments are not critical
		recordWarning(ctx, mc.qtx, mrInDB.ID, fmt.Sprintf("migrating comments for PR %d: %v", pullRequest.GetNumber(), err))
		logger.Warn("failed to migrate comments, continuing with merge", "pr_number", pullRequest.GetNumber(), "error", err)
	}

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
