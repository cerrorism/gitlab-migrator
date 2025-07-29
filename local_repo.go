package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// MergeRequestInfo represents information extracted from a merge commit
type MergeRequestInfo struct {
	MrIID          int    // Merge Request Internal ID
	MergeCommitSHA string // SHA of the merge commit
	BaseParent     string // SHA of the base branch parent (first parent)
	HeadParent     string // SHA of the feature branch parent (second parent)
	CommitMessage  string // First line of the commit message
	Status         string // Status: MR_FOUND, NO_MR, NOT_MERGE
}

// getMergeRequestInfoFromLocal clones a GitHub repository and extracts merge request information
// from the commit history by analyzing merge commits in the master/main branch
func getMergeRequestInfoFromLocal(ctx context.Context, githubRepoName string) ([]MergeRequestInfo, error) {
	var results []MergeRequestInfo

	// Clone the repository
	repo, err := cloneRepoForAnalysis(ctx, githubRepoName)
	if err != nil {
		return nil, fmt.Errorf("failed to clone repository: %v", err)
	}

	logger.Info("analyzing commits", "branch", "master", "repo", githubRepoName)

	// Get the branch reference
	ref, err := repo.Reference(plumbing.ReferenceName("refs/heads/master"), true)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch reference: %v", err)
	}

	// Get commit iterator starting from the branch head
	commitIter, err := repo.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, fmt.Errorf("failed to get commit log: %v", err)
	}
	defer commitIter.Close()

	// Regex patterns for matching merge commits and extracting MR IID
	mergePattern := regexp.MustCompile(`(?i)Merge branch '([^']+)' into 'master'`)
	mrIIDPattern := regexp.MustCompile(`(?i)serviceone/front-porch!(\d+)`)

	totalProcessed := 0
	foundCount := 0

	// Iterate through commits
	err = commitIter.ForEach(func(commit *object.Commit) error {
		totalProcessed++

		// Progress indicator
		if totalProcessed%1000 == 0 {
			logger.Info("processing commits", "processed", totalProcessed, "found", foundCount)
		}

		commitMessage := strings.TrimSpace(commit.Message)
		firstLine := strings.Split(commitMessage, "\n")[0]

		// Skip revert commits - these are not original feature work
		if strings.HasPrefix(commitMessage, "Revert") {
			return nil
		}

		// Check if this is a merge commit with the expected pattern
		mergeMatch := mergePattern.FindStringSubmatch(commitMessage)

		if mergeMatch != nil {
			// Verify this is actually a merge commit (should have 2+ parents)
			if len(commit.ParentHashes) >= 2 {
				baseParent := commit.ParentHashes[0].String() // First parent: base branch
				headParent := commit.ParentHashes[1].String() // Second parent: feature branch head

				// Look for MR IID in the commit message
				mrMatch := mrIIDPattern.FindStringSubmatch(commitMessage)

				if mrMatch != nil {
					// Parse MR IID
					mrIID, parseErr := strconv.Atoi(mrMatch[1])
					if parseErr != nil {
						logger.Warn("failed to parse MR IID", "iid", mrMatch[1], "commit", commit.Hash.String())
						return nil
					}

					results = append(results, MergeRequestInfo{
						MrIID:          mrIID,
						MergeCommitSHA: commit.Hash.String(),
						BaseParent:     baseParent,
						HeadParent:     headParent,
						CommitMessage:  firstLine,
						Status:         "MR_FOUND",
					})
					foundCount++
				} else {
					// Merge commit without MR reference
					results = append(results, MergeRequestInfo{
						MrIID:          0, // No MR IID
						MergeCommitSHA: commit.Hash.String(),
						BaseParent:     baseParent,
						HeadParent:     headParent,
						CommitMessage:  firstLine,
						Status:         "NO_MR",
					})
				}
			} else {
				// Not actually a merge commit (single parent), but matched the pattern
				results = append(results, MergeRequestInfo{
					MrIID:          0,
					MergeCommitSHA: commit.Hash.String(),
					BaseParent:     "",
					HeadParent:     "",
					CommitMessage:  firstLine,
					Status:         "NOT_MERGE",
				})
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error iterating commits: %v", err)
	}

	logger.Info("analysis complete",
		"total_processed", totalProcessed,
		"merge_requests_found", foundCount,
		"total_results", len(results))

	return results, nil
}

// cloneRepoForAnalysis clones a GitHub repository for commit analysis
func cloneRepoForAnalysis(ctx context.Context, githubRepoName string) (*git.Repository, error) {
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s",
		githubToken, githubRepoName)

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	logger.Info("cloning repository for analysis", "repo", githubRepoName, "url_masked", fmt.Sprintf("https://***@github.com/%s", githubRepoName))

	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:           cloneURL,
		Auth:          nil,
		RemoteName:    "origin",
		Mirror:        false,                                       // We don't need a mirror, just the commit history
		ReferenceName: plumbing.ReferenceName("refs/heads/master"), // Specifically clone master branch
	})
	if err != nil {
		return nil, fmt.Errorf("cloning GitHub repo: %v", err)
	}

	return repo, nil
}

// getMasterOrMainBranch determines whether to use 'master' or 'main' branch
func getMasterOrMainBranch(repo *git.Repository) (string, error) {
	// Try 'master' first
	_, err := repo.Reference(plumbing.ReferenceName("refs/heads/master"), true)
	if err == nil {
		return "master", nil
	}

	// Try 'main' if 'master' doesn't exist
	_, err = repo.Reference(plumbing.ReferenceName("refs/heads/main"), true)
	if err == nil {
		logger.Info("using 'main' branch instead of 'master'")
		return "main", nil
	}

	return "", fmt.Errorf("neither 'master' nor 'main' branch found")
}
