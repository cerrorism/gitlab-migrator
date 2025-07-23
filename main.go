package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/xanzy/go-gitlab"
)

const (
	dateFormat = "Mon, 2 Jan 2006"
)

var githubRepo, githubToken, githubUser, gitlabProject, gitlabToken string

var (
	cache    *objectCache
	errCount int
	logger   hclog.Logger
	gh       *github.Client
	gl       *gitlab.Client
)

type Project = []string

func sendErr(err error) {
	errCount++
	logger.Error(err.Error())
}

func main() {
	var err error

	// Bypass pre-emptive rate limit checks in the GitHub client, as we will handle these via go-retryablehttp
	valueCtx := context.WithValue(context.Background(), github.BypassRateLimitCheck, true)

	// Assign a Done channel so we can abort on Ctrl-c
	ctx, cancel := context.WithCancel(valueCtx)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "gitlab-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	cache = newObjectCache()

	githubToken = os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		logger.Error("missing environment variable", "name", "GITHUB_TOKEN")
		os.Exit(1)
	}

	gitlabToken = os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
		os.Exit(1)
	}
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&gitlabProject, "gitlab-project", "", "the gitlab project to migrate")

	flag.Parse()

	if githubUser == "" {
		githubUser = os.Getenv("GITHUB_USER")
	}

	if githubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	retryClient := &retryablehttp.Client{
		HTTPClient:   cleanhttp.DefaultPooledClient(),
		Logger:       nil,
		RetryMax:     2,
		RetryWaitMin: 30 * time.Second,
		RetryWaitMax: 300 * time.Second,
	}

	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) (sleep time.Duration) {
		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		defer func() {
			logger.Trace("waiting before retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "sleep", sleep, "attempt", attemptNum, "max_attempts", retryClient.RetryMax)
		}()

		if resp != nil {
			// Check the Retry-After header
			if s, ok := resp.Header["Retry-After"]; ok {
				if retryAfter, err := strconv.ParseInt(s[0], 10, 64); err == nil {
					sleep = time.Second * time.Duration(retryAfter)
					return
				}
			}

			// Reference:
			// - https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28
			// - https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28
			if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
				if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && remaining == 0 {

					// If x-ratelimit-reset is present, this indicates the UTC timestamp when we can retry
					if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
						if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
							// Add 30 seconds to recovery timestamp for clock differences
							sleep = roundDuration(time.Until(time.Unix(recoveryEpoch+30, 0)), time.Second)
							return
						}
					}

					// Otherwise, wait for 60 seconds
					sleep = 60 * time.Second
					return
				}
			}
		}

		// Exponential backoff
		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		wait := time.Duration(mult)
		if float64(wait) != mult || wait > max {
			wait = max
		}

		sleep = wait
		return
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return false, err
		}

		// Potential connection reset
		if resp == nil {
			return true, nil
		}

		retryableStatuses := []int{
			http.StatusTooManyRequests, // rate-limiting
			http.StatusForbidden,       // rate-limiting

			http.StatusRequestTimeout,
			http.StatusFailedDependency,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {
				logger.Trace("retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode)
				return true, nil
			}
		}

		return false, nil
	}

	client := githubpagination.NewClient(&retryablehttp.RoundTripper{Client: retryClient}, githubpagination.WithPerPage(100))

	gh = github.NewClient(client).WithAuthToken(githubToken)

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		sendErr(err)
		os.Exit(1)
	}

	proj := Project{gitlabProject, githubRepo}

	if err = migrateProject(ctx, proj); err != nil {
		sendErr(err)
		os.Exit(1)
	} else if errCount > 0 {
		logger.Warn(fmt.Sprintf("encountered %d errors during migration, review log output for details", errCount))
		os.Exit(1)
	}
}

func migrateProject(ctx context.Context, proj []string) error {
	githubPath := strings.Split(proj[1], "/")

	project, _, err := gl.Projects.GetProject(gitlabProject, &gitlab.GetProjectOptions{})
	if err != nil {
		return err
	}

	if project == nil {
		return fmt.Errorf("no matching GitLab project found: %s", proj[0])
	}

	repo, err := getLocalRepo(ctx, project, githubPath)
	if err != nil {
		return err
	}
	migratePullRequests(ctx, githubPath, project, repo)

	return nil
}

func getLocalRepo(ctx context.Context, project *gitlab.Project, githubPath []string) (*git.Repository, error) {
	cloneUrl, err := url.Parse(project.HTTPURLToRepo)
	if err != nil {
		return nil, fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from GitLab to GitHub", "name", project.Name, "github_org", githubPath[0], "github_repo", githubPath[1])

	logger.Info("checking for existing repository on GitHub", "owner", githubPath[0], "repo", githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, githubPath[0], githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return nil, fmt.Errorf("retrieving github repo: %v", err)
	}

	if err != nil {
		logger.Error("error retrieving repository from GitHub", "owner", githubPath[0], "repo", githubPath[1], "error", err)
		return nil, fmt.Errorf("retrieving github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	logger.Info("cloning repository", "name", project.Name, "url", project.HTTPURLToRepo)
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitlab",
		Mirror:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("cloning gitlab repo: %v", err)
	}

	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, "github.com", githubPath[0], githubPath[1])

	if _, err = repo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return nil, fmt.Errorf("adding github remote: %v", err)
	}

	if err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		upToDateError := errors.New("already up-to-date")
		if !errors.As(err, &upToDateError) {
			return nil, fmt.Errorf("pushing to github repo: %v", err)
		}
	}
	return repo, nil
}

func migratePullRequests(ctx context.Context, githubPath []string, project *gitlab.Project, repo *git.Repository) {

	var successCount, failureCount int
	for mrId := 1; mrId <= 2; mrId++ {
		mergeRequest, _, err := gl.MergeRequests.GetMergeRequest(project.ID, mrId, &gitlab.GetMergeRequestsOptions{})
		if err != nil {
			if errors.Is(err, gitlab.ErrNotFound) {
				logger.Info(fmt.Sprintf("skip non-existing merge request ID = %d", mrId))
				continue
			} else {
				sendErr(fmt.Errorf("retrieving gitlab merge request %d: %v", mrId, err))
				return
			}

		}

		if mergeRequest == nil {
			continue
		}
		logger.Info(fmt.Sprintf("migrating merge request ID = %d", mrId))
		migrateSingleMergeRequest(ctx, githubPath, project, repo, mergeRequest)
	}

	logger.Info("migrated merge requests from GitLab to GitHub", "successful", successCount, "failed", failureCount)
}
