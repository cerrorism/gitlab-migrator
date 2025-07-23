package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/google/go-github/v69/github"
	"github.com/xanzy/go-gitlab"
)

func migrateSingleMergeRequest(ctx context.Context, githubPath []string, project *gitlab.Project, repo *git.Repository, mergeRequest *gitlab.MergeRequest) (int, int) {
	successCount := 0
	failureCount := 0
	if err := ctx.Err(); err != nil {
		sendErr(fmt.Errorf("preparing to list pull requests: %v", err))
		return successCount, failureCount
	}

	var cleanUpBranch bool
	var pullRequest *github.PullRequest

	logger.Info("searching for any existing pull request", "owner", githubPath[0], "repo", githubPath[1], "merge_request_id", mergeRequest.IID)
	//query := fmt.Sprintf("repo:%s/%s is:pr head:%s", githubPath[0], githubPath[1], mergeRequest.SourceBranch)
	query := fmt.Sprintf("repo:%s/%s is:pr", githubPath[0], githubPath[1])
	searchResult, err := getGithubSearchResults(ctx, query)
	if err != nil {
		sendErr(fmt.Errorf("listing pull requests: %v", err))
		return successCount, failureCount
	}

	// Look for an existing GitHub pull request
	skip := false
	for _, issue := range searchResult.Issues {
		if issue == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			sendErr(fmt.Errorf("preparing to retrieve pull request: %v", err))
			break
		}

		if issue.IsPullRequest() {
			// Extract the PR number from the URL
			prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
			if err != nil {
				sendErr(fmt.Errorf("parsing pull request url: %v", err))
				skip = true
				break
			}

			if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
				prNumber, _ := strconv.Atoi(m[1])
				pr, err := getGithubPullRequest(ctx, githubPath[0], githubPath[1], prNumber)
				if err != nil {
					sendErr(fmt.Errorf("retrieving pull request: %v", err))
					skip = true
					break
				}

				if strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | %d", mergeRequest.IID)) ||
					strings.Contains(pr.GetBody(), fmt.Sprintf("**GitLab MR Number** | [%d]", mergeRequest.IID)) {
					logger.Info("found existing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pr.GetNumber())
					pullRequest = pr
				}
			}
		}
	}
	if skip {
		return successCount, failureCount
	}

	// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
	if pullRequest == nil && !strings.EqualFold(mergeRequest.State, "opened") {

		// Only create temporary branches if the source branch has been deleted
		if _, err = repo.Reference(plumbing.ReferenceName(mergeRequest.SourceBranch), false); err != nil {
			// Create a worktree
			worktree, err := repo.Worktree()
			if err != nil {
				sendErr(fmt.Errorf("creating worktree: %v", err))
				failureCount++
				return successCount, failureCount
			}

			// Generate temporary branch names
			mergeRequest.SourceBranch = fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
			mergeRequest.TargetBranch = fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

			mergeRequestCommits, _, err := gl.MergeRequests.GetMergeRequestCommits(project.ID, mergeRequest.IID, &gitlab.GetMergeRequestCommitsOptions{OrderBy: "created_at", Sort: "asc"})
			if err != nil {
				sendErr(fmt.Errorf("retrieving merge request commits: %v", err))
				failureCount++
				return successCount, failureCount
			}

			// Some merge requests have no commits, disregard these
			if len(mergeRequestCommits) == 0 {
				return successCount, failureCount
			}

			// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
			sort.Slice(mergeRequestCommits, func(i, j int) bool {
				return mergeRequestCommits[i].CommittedDate.Before(*mergeRequestCommits[j].CommittedDate)
			})

			if mergeRequestCommits[0] == nil {
				sendErr(fmt.Errorf("start commit for merge request %d is nil", mergeRequest.IID))
				failureCount++
				return successCount, failureCount
			}
			if mergeRequestCommits[len(mergeRequestCommits)-1] == nil {
				sendErr(fmt.Errorf("end commit for merge request %d is nil", mergeRequest.IID))
				failureCount++
				return successCount, failureCount
			}

			startCommit, err := object.GetCommit(repo.Storer, plumbing.NewHash(mergeRequestCommits[0].ID))
			if err != nil {
				sendErr(fmt.Errorf("loading start commit: %v", err))
				failureCount++
				return successCount, failureCount
			}

			if startCommit.NumParents() == 0 {
				// Orphaned commit, start with an empty branch
				// TODO: this isn't working as hoped, try to figure this out. in the meantime, we'll skip MRs from orphaned branches
				//if err = repo.Storer.SetReference(plumbing.NewSymbolicReference("HEAD", plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", mergeRequest.TargetBranch)))); err != nil {
				//	return fmt.Errorf("creating empty branch: %s", err)
				//}
				sendErr(fmt.Errorf("start commit %s for merge request %d has no parents", mergeRequestCommits[0].ShortID, mergeRequest.IID))

				return successCount, failureCount
			} else {
				// Sometimes we will be starting from a merge commit, so look for a suitable parent commit to branch out from
				var startCommitParent *object.Commit
				for i := 0; i < startCommit.NumParents(); i++ {
					startCommitParent, err = startCommit.Parent(0)
					if err != nil {
						sendErr(fmt.Errorf("loading parent commit: %s", err))
					}

					continue
				}

				if startCommitParent == nil {
					sendErr(fmt.Errorf("identifying suitable parent of start commit %s for merge request %d", mergeRequestCommits[0].ShortID, mergeRequest.IID))
					failureCount++
				}

				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: true,
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(mergeRequest.TargetBranch),
					Hash:   startCommitParent.Hash,
				}); err != nil {
					sendErr(fmt.Errorf("checking out temporary target branch: %v", err))
					failureCount++

					return successCount, failureCount
				}
			}

			endHash := plumbing.NewHash(mergeRequestCommits[len(mergeRequestCommits)-1].ID)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: true,
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(mergeRequest.SourceBranch),
				Hash:   endHash,
			}); err != nil {
				sendErr(fmt.Errorf("checking out temporary source branch: %v", err))
				failureCount++

				return successCount, failureCount
			}

			logger.Info("pushing branches for merged/closed merge request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			if err = repo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.SourceBranch)),
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", mergeRequest.TargetBranch)),
				},
				Force: true,
			}); err != nil {
				upToDateError := errors.New("already up-to-date")
				if errors.As(err, &upToDateError) {
					logger.Info("branch already exists and is up-to-date on GitHub", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
				} else {
					sendErr(fmt.Errorf("pushing temporary branches to github: %v", err))
					failureCount++

					return successCount, failureCount
				}
			}

			// We will clean up these temporary branches after configuring and closing the pull request
			cleanUpBranch = true
		}
	}

	githubAuthorName := mergeRequest.Author.Name

	author, err := getGitlabUser(mergeRequest.Author.Username)
	if err != nil {
		sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
		failureCount++

		return successCount, failureCount
	}
	if author.WebsiteURL != "" {
		githubAuthorName = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	approvers := make([]string, 0)
	awards, _, err := gl.AwardEmoji.ListMergeRequestAwardEmoji(project.ID, mergeRequest.IID, &gitlab.ListAwardEmojiOptions{PerPage: 100})
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

	gitlabPath := strings.Split(gitlabProject, "/")
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

	if pullRequest == nil {
		logger.Info("creating pull request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		newPullRequest := github.NewPullRequest{
			Title:               &mergeRequest.Title,
			Head:                &mergeRequest.SourceBranch,
			Base:                &mergeRequest.TargetBranch,
			Body:                &body,
			MaintainerCanModify: pointer(true),
			Draft:               &mergeRequest.Draft,
		}
		if pullRequest, _, err = gh.PullRequests.Create(ctx, githubPath[0], githubPath[1], &newPullRequest); err != nil {
			sendErr(fmt.Errorf("creating pull request: %v", err))
			requestStr, _ := json.MarshalIndent(newPullRequest, "", "  ")
			logger.Error(fmt.Sprintf("request: %s", requestStr))
			return 0, 1
		}

		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			logger.Info("closing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.State = pointer("closed")
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				sendErr(fmt.Errorf("updating pull request: %v", err))
				return 0, 1
			}
		}

	} else {
		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = pointer("open")
		case "closed", "merged":
			newState = pointer("closed")
		}

		if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
			(pullRequest.Title == nil || *pullRequest.Title != mergeRequest.Title) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) ||
			(pullRequest.Draft == nil || *pullRequest.Draft != mergeRequest.Draft) {
			logger.Info("updating pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &mergeRequest.Title
			pullRequest.Body = &body
			pullRequest.Draft = &mergeRequest.Draft
			pullRequest.State = newState
			pullRequest.MaintainerCanModify = nil
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				sendErr(fmt.Errorf("updating pull request: %v", err))
				return 0, 1
			}
		} else {
			logger.Trace("existing pull request is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	if cleanUpBranch {
		logger.Info("deleting temporary branches for closed pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
		if err = repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.SourceBranch)),
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", mergeRequest.TargetBranch)),
			},
			Force: true,
		}); err != nil {
			upToDateError := errors.New("already up-to-date")
			if errors.As(err, &upToDateError) {
				logger.Trace("branches already deleted on GitHub", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "source_branch", mergeRequest.SourceBranch, "target_branch", mergeRequest.TargetBranch)
			} else {
				sendErr(fmt.Errorf("pushing branch deletions to github: %v", err))
				return 0, 1
			}
		}
	}

	var comments []*gitlab.Note
	skipComments := false
	opts := &gitlab.ListMergeRequestNotesOptions{
		OrderBy: pointer("created_at"),
		Sort:    pointer("asc"),
	}

	for {
		result, resp, err := gl.Notes.ListMergeRequestNotes(project.ID, mergeRequest.IID, opts)
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

	if skipComments {
		failureCount++
	} else {
		logger.Info("retrieving GitHub pull request comments", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
		prComments, _, err := gh.Issues.ListComments(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
		if err != nil {
			sendErr(fmt.Errorf("listing pull request comments: %v", err))
		} else {
			logger.Info("migrating merge request comments from GitLab to GitHub", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

			for _, comment := range comments {
				if comment == nil || comment.System {
					continue
				}

				githubCommentAuthorName := comment.Author.Name

				commentAuthor, err := getGitlabUser(comment.Author.Username)
				if err != nil {
					sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
					failureCount++
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
							logger.Info("updating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
							prComment.Body = &commentBody
							if _, _, err = gh.Issues.EditComment(ctx, githubPath[0], githubPath[1], prComment.GetID(), prComment); err != nil {
								sendErr(fmt.Errorf("updating pull request comments: %v", err))
								failureCount++
								break
							}
						}
					} else {
						logger.Trace("existing pull request comment is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
					}
				}

				if !foundExistingComment {
					logger.Info("creating pull request comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", pullRequest.GetNumber())
					newComment := github.IssueComment{
						Body: &commentBody,
					}
					if _, _, err = gh.Issues.CreateComment(ctx, githubPath[0], githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
						sendErr(fmt.Errorf("creating pull request comment: %v", err))
						failureCount++
						break
					}
				}
			}
		}
	}
	return successCount, failureCount
}
