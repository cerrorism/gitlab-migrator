package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/manicminer/gitlab-migrator/db"
	"slices"
	"strings"

	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"
)

// createGitHubRef creates a GitHub ref (branch) pointing to a specific commit SHA
func createGitHubRef(ctx context.Context, owner, repo, refName, commitSHA string) error {
	fullRefName := "refs/heads/" + refName

	// Check if ref already exists
	existingRef, _, err := gh.Git.GetRef(ctx, owner, repo, "heads/"+refName)
	if err == nil {
		// Ref exists, check if it points to the correct commit
		if existingRef.Object.GetSHA() == commitSHA {
			logger.Info("ref already points to correct commit", "ref", refName, "sha", commitSHA)
			return nil
		}

		// Update existing ref to point to new commit
		_, _, err = gh.Git.UpdateRef(ctx, owner, repo, &github.Reference{
			Ref: &fullRefName,
			Object: &github.GitObject{
				SHA: &commitSHA,
			},
		}, false)
		if err != nil {
			return fmt.Errorf("updating existing ref %s: %v", refName, err)
		}
		logger.Info("updated existing ref", "ref", refName, "sha", commitSHA)
		return nil
	}

	// Create new ref
	_, _, err = gh.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: &fullRefName,
		Object: &github.GitObject{
			SHA: &commitSHA,
		},
	})
	if err != nil {
		return fmt.Errorf("creating ref %s: %v", refName, err)
	}

	logger.Info("created new ref", "ref", refName, "sha", commitSHA)
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
			sendErr(fmt.Errorf("listing merge request notes: %v", err))
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
		prComments, _, err := gh.Issues.ListComments(ctx, owner, repoName, prNumber, &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
		if err != nil {
			return fmt.Errorf("listing pull request comments: %v", err)
		}

		logger.Info("migrating merge request comments from GitLab to GitHub", "owner", owner, "repo", repoName, "pr_number", mrInDB.PrID, "count", len(comments))

		for _, comment := range comments {
			if comment == nil || comment.System {
				continue
			}

			githubCommentAuthorName := comment.Author.Name

			commentAuthor, err := getGitlabUser(comment.Author.Username)
			if err != nil {
				sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
				break
			}
			if commentAuthor.WebsiteURL != "" {
				githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
			}

			commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

## Original Comment

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.Format("Mon, 2 Jan 2006"), comment.Body)

			foundExistingComment := false
			for _, prComment := range prComments {
				if prComment == nil {
					continue
				}

				if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
					foundExistingComment = true

					if prComment.Body == nil || *prComment.Body != commentBody {
						logger.Info("updating pull request comment", "owner", owner, "repo", repoName, "pr_number", prNumber, "comment_id", prComment.GetID())
						prComment.Body = &commentBody
						if _, _, err = gh.Issues.EditComment(ctx, owner, repoName, prComment.GetID(), prComment); err != nil {
							return fmt.Errorf("updating pull request comments: %v", err)
						}
					}
				} else {
					logger.Trace("existing pull request comment is up-to-date", "owner", owner, "repo", repoName, "pr_number", prNumber, "comment_id", prComment.GetID())
				}
			}

			if !foundExistingComment {
				logger.Info("creating pull request comment", "owner", owner, "repo", repoName, "pr_number", prNumber)
				newComment := github.IssueComment{
					Body: &commentBody,
				}
				if _, _, err = gh.Issues.CreateComment(ctx, owner, repoName, prNumber, &newComment); err != nil {
					return fmt.Errorf("creating pull request comment: %v", err)
				}
			}
		}
	}

	return nil
}

func migrateSingleMergeRequest(ctx context.Context, mc *migrationContext, mergeRequest *gitlab.MergeRequest, mrInDB *db.GitlabMergeRequest) string {
	if err := ctx.Err(); err != nil {
		sendErr(fmt.Errorf("preparing to list pull requests: %v", err))
		return "failed"
	}

	var pullRequest *github.PullRequest

	githubPath := strings.Split(mc.migration.GithubRepoName, "/")
	if len(githubPath) != 2 {
		return fmt.Sprintf("failed: invalid github repo name format: %s", mc.migration.GithubRepoName)
	}
	owner, repoName := githubPath[0], githubPath[1]

	// Generate temporary branch names for closed/merged MRs
	sourceBranch := mergeRequest.SourceBranch
	targetBranch := mergeRequest.TargetBranch

	if !strings.EqualFold(mergeRequest.State, "opened") {
		// For closed/merged MRs, create temporary refs using commit SHAs from database
		sourceBranch = fmt.Sprintf("migration-source-%d", mergeRequest.IID)
		targetBranch = fmt.Sprintf("migration-target-%d", mergeRequest.IID)

		// Validate we have the necessary commit SHAs
		if mrInDB.Parent1CommitSha == "" || mrInDB.Parent2CommitSha == "" {
			return "failed: missing commit SHAs in database"
		}

		logger.Info("creating temporary refs for closed/merged MR",
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
	}

	// Get GitLab user information for the author
	githubAuthorName := mergeRequest.Author.Name
	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
		return "failed: retrieving gitlab user"
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	// Build PR description with migration metadata
	originalState := fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)

	// Get approvers from GitLab awards
	approvers := make([]string, 0)
	awards, _, err := gl.AwardEmoji.ListMergeRequestAwardEmoji(mc.gitlabProject.ID, mergeRequest.IID, &gitlab.ListAwardEmojiOptions{PerPage: 100})
	if err != nil {
		sendErr(fmt.Errorf("listing merge request awards: %v", err))
	} else {
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approver := award.User.Name
				approverUser, err := getGitlabUser(award.User.Username)
				if err != nil {
					sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
					continue
				}
				if approverUser.WebsiteURL != "" {
					approver = "@" + strings.TrimPrefix(strings.ToLower(approverUser.WebsiteURL), "https://github.com/")
				}
				approvers = append(approvers, approver)
			}
		}
	}

	description := mergeRequest.Description
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = "_No approvers_"
	}

	closeDate := ""
	if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.Format(dateFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.Format(dateFormat))
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
> | **GitLab Project** | [%[4]s/%[5]s](https://%[10]s/%[4]s/%[5]s) |
> | **GitLab Merge Request** | [%[11]s](https://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **GitLab MR Number** | [%[2]d](https://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Date Originally Opened** | %[6]s |%[7]s
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[9]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, gitlabPath[0], gitlabPath[1], mergeRequest.CreatedAt.Format(dateFormat), closeDate, approval, originalState, "gitlab.com", mergeRequestTitle)

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
		sendErr(fmt.Errorf("creating pull request: %v", err))
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
		return fmt.Sprintf("failed: create pull request in DB - PR %d", pullRequest.GetNumber())
	}

	// Close PR if the original MR was closed/merged
	if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
		logger.Info("closing pull request", "owner", owner, "repo", repoName, "pr_number", pullRequest.GetNumber())
		pullRequest.State = pointer("closed")
		if pullRequest, _, err = gh.PullRequests.Edit(ctx, owner, repoName, pullRequest.GetNumber(), pullRequest); err != nil {
			sendErr(fmt.Errorf("updating pull request: %v", err))
			return fmt.Sprintf("failed: updating pull request - %s", err.Error())
		}
	}

	// Clean up temporary refs for closed/merged MRs
	logger.Info("deleting temporary refs for closed pull request", "owner", owner, "repo", repoName, "pr_number", pullRequest.GetNumber(), "source_ref", sourceBranch, "target_ref", targetBranch)

	if err := deleteGitHubRef(ctx, owner, repoName, sourceBranch); err != nil {
		sendErr(fmt.Errorf("deleting source ref: %v", err))
	}

	if err := deleteGitHubRef(ctx, owner, repoName, targetBranch); err != nil {
		sendErr(fmt.Errorf("deleting target ref: %v", err))
	}

	return "success"
}
