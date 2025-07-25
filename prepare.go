package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
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

func prepareAndSetup() context.Context {
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

	inMemCache = newObjectCache()

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

	githubUser = os.Getenv("GITHUB_USER")
	if githubUser == "" {
		logger.Error("missing environment variable", "name", "GITHUB_USER")
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

	client := githubpagination.NewClient(&retryablehttp.RoundTripper{Client: retryClient}, githubpagination.WithPerPage(100))

	gh = github.NewClient(client).WithAuthToken(githubToken)

	gitlabOpts := make([]gitlab.ClientOptionFunc, 0)
	if gl, err = gitlab.NewClient(gitlabToken, gitlabOpts...); err != nil {
		sendErr(err)
		os.Exit(1)
	}

	return ctx
}
