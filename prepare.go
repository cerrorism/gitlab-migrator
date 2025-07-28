package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/jackc/pgx/v5"
	"github.com/manicminer/gitlab-migrator/db"
	"github.com/xanzy/go-gitlab"
)

// logAPIResponse logs detailed information about HTTP responses, especially for failures
func logAPIResponse(resp *http.Response, err error, isRetry bool) {
	if resp == nil && err != nil {
		logger.Error("API request failed with error", "error", err.Error(), "retry", isRetry)
		return
	}

	if resp == nil {
		logger.Error("API request failed with nil response", "error", err, "retry", isRetry)
		return
	}

	requestMethod := "unknown"
	requestURL := "unknown"
	if req := resp.Request; req != nil {
		requestMethod = req.Method
		if req.URL != nil {
			requestURL = req.URL.String()
		}
	}

	// Success cases (2xx) - only log at trace level
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.Trace("API request successful",
			"method", requestMethod,
			"url", requestURL,
			"status", resp.StatusCode)
		return
	}

	// Read response body for error details
	var responseBody string
	if resp.Body != nil {
		// Don't read more than 4KB of response body
		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr == nil {
			responseBody = strings.TrimSpace(string(bodyBytes))
		} else {
			responseBody = fmt.Sprintf("failed to read response body: %v", readErr)
		}

		// Close and restore the body for potential retries
		resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
	}

	// Rate limiting cases (special handling)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		var rateLimitInfo []interface{}

		if remaining := resp.Header.Get("X-Ratelimit-Remaining"); remaining != "" {
			rateLimitInfo = append(rateLimitInfo, "remaining", remaining)
		}
		if reset := resp.Header.Get("X-Ratelimit-Reset"); reset != "" {
			rateLimitInfo = append(rateLimitInfo, "reset_time", reset)
		}
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			rateLimitInfo = append(rateLimitInfo, "retry_after", retryAfter)
		}

		logArgs := []interface{}{
			"method", requestMethod,
			"url", requestURL,
			"status", resp.StatusCode,
			"reason", "rate_limited",
			"retry", isRetry,
		}
		logArgs = append(logArgs, rateLimitInfo...)

		if responseBody != "" {
			logArgs = append(logArgs, "response_body", responseBody)
		}

		logger.Warn("API request rate limited", logArgs...)
		return
	}

	// All other error cases
	logLevel := hclog.Error
	if isRetry {
		logLevel = hclog.Warn // Use warn level for retryable errors
	}

	logArgs := []interface{}{
		"method", requestMethod,
		"url", requestURL,
		"status", resp.StatusCode,
		"retry", isRetry,
	}

	if err != nil {
		logArgs = append(logArgs, "error", err.Error())
	}

	if responseBody != "" {
		logArgs = append(logArgs, "response_body", responseBody)
	}

	// Add status-specific context
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		logArgs = append(logArgs, "reason", "unauthorized - check authentication token")
	case http.StatusNotFound:
		logArgs = append(logArgs, "reason", "resource not found")
	case http.StatusUnprocessableEntity:
		logArgs = append(logArgs, "reason", "validation error")
	case http.StatusInternalServerError:
		logArgs = append(logArgs, "reason", "server error")
	case http.StatusBadGateway:
		logArgs = append(logArgs, "reason", "bad gateway")
	case http.StatusServiceUnavailable:
		logArgs = append(logArgs, "reason", "service unavailable")
	case http.StatusGatewayTimeout:
		logArgs = append(logArgs, "reason", "gateway timeout")
	default:
		logArgs = append(logArgs, "reason", "http error")
	}

	logger.Log(logLevel, "API request failed", logArgs...)
}

func prepareAndSetup() (context.Context, *db.GithubAuthToken) {
	var err error

	// Bypass pre-emptive rate limit checks in the GitHub client, as we will handle these via go-retryablehttp
	ctx := context.WithValue(context.Background(), github.BypassRateLimitCheck, true)

	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "gitlab-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	inMemCache = newObjectCache()

	gitlabToken = os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		logger.Error("missing environment variable", "name", "GITLAB_TOKEN")
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
			logArgs := []interface{}{
				"method", requestMethod,
				"url", requestUrl,
				"status", resp.StatusCode,
				"sleep", sleep.String(),
				"attempt", attemptNum + 1,
				"max_attempts", retryClient.RetryMax,
			}

			// Add rate limiting context if present
			if remaining := resp.Header.Get("X-Ratelimit-Remaining"); remaining != "" {
				logArgs = append(logArgs, "rate_limit_remaining", remaining)
			}
			if reset := resp.Header.Get("X-Ratelimit-Reset"); reset != "" {
				logArgs = append(logArgs, "rate_limit_reset", reset)
			}

			logger.Info("retrying API request after backoff", logArgs...)
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
			logAPIResponse(resp, err, false)
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

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {
				logAPIResponse(resp, err, true) // Log retryable errors
				return true, nil
			}
		}

		// For non-retryable errors, log them as final failures
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			logAPIResponse(resp, err, false)
		}

		return false, nil
	}

		// Connect to database to select an available GitHub auth token
	dbString := "user=postgres password=password dbname=postgres sslmode=false"
	dbConn, err := pgx.Connect(ctx, dbString)
	if err != nil {
		logger.Error("failed to connect to database for token selection", "error", err)
		os.Exit(1)
	}
	defer dbConn.Close(ctx)

	queries := db.New(dbConn)
	
	// Select an available GitHub auth token
	githubAuthToken, err := queries.GetAvailableGithubAuthToken(ctx)
	if err != nil {
		logger.Error("failed to acquire GitHub auth token", "error", err)
		logger.Info("make sure you have GitHub auth tokens configured in the github_auth_token table")
		logger.Info("example SQL to add a PAT token:")
		logger.Info("  INSERT INTO github_auth_token (auth_type, token) VALUES ('PAT', 'ghp_your_token');")
		logger.Info("example SQL to add a GitHub App:")
		logger.Info("  INSERT INTO github_auth_token (auth_type, app_id, installation_id, private_key_file) VALUES ('APP', 123456, 12345678, '/path/to/key.pem');")
		os.Exit(1)
	}

	logger.Info("acquired GitHub auth token", 
		"token_id", githubAuthToken.ID,
		"auth_type", githubAuthToken.AuthType,
		"rate_limit_remaining", githubAuthToken.RateLimitRemaining.Int32)

	// Create GitHub client based on auth token type
	if githubAuthToken.AuthType == "PAT" {
		// Use Personal Access Token authentication
		if !githubAuthToken.Token.Valid {
			logger.Error("PAT token missing required fields", "token_id", githubAuthToken.ID)
			os.Exit(1)
		}
		
		githubToken = githubAuthToken.Token.String
		
		logger.Info("using GitHub Personal Access Token authentication", "token_id", githubAuthToken.ID)
		gh = createGithubClientWithPAT(retryClient, githubToken)
		
	} else if githubAuthToken.AuthType == "APP" {
		// Use GitHub App authentication
		if !githubAuthToken.AppID.Valid || !githubAuthToken.InstallationID.Valid || !githubAuthToken.PrivateKeyFile.Valid {
			logger.Error("GitHub App token missing required fields", "token_id", githubAuthToken.ID)
			os.Exit(1)
		}

		appConfig := GitHubAppConfig{
			AppID:          githubAuthToken.AppID.Int64,
			InstallationID: githubAuthToken.InstallationID.Int64,
			PrivateKeyFile: githubAuthToken.PrivateKeyFile.String,
		}

		gh, err = createGithubClientWithApp(retryClient, appConfig)
		if err != nil {
			logger.Error("failed to create GitHub App client", "error", err, "token_id", githubAuthToken.ID)
			os.Exit(1)
		}
		
		// Get installation access token for git operations
		tempTransport, err := NewAppAuthTransport(appConfig, &retryablehttp.RoundTripper{Client: retryClient})
		if err != nil {
			logger.Error("failed to create temp transport for git token", "error", err, "token_id", githubAuthToken.ID)
			os.Exit(1)
		}
		
		// Extract the access token for git cloning
		tempTransport.mu.RLock()
		githubToken = tempTransport.accessToken
		tempTransport.mu.RUnlock()
		
		logger.Info("successfully configured GitHub App authentication", 
			"app_id", appConfig.AppID,
			"installation_id", appConfig.InstallationID)
	} else {
		logger.Error("unsupported GitHub auth type", "auth_type", githubAuthToken.AuthType, "token_id", githubAuthToken.ID)
		os.Exit(1)
	}

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		sendErr(err)
		os.Exit(1)
	}

	return ctx, &githubAuthToken
}
