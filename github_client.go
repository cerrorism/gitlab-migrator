package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/go-github/v69/github"
	"github.com/hashicorp/go-retryablehttp"
)

// GitHubAppConfig holds the configuration for GitHub App authentication
type GitHubAppConfig struct {
	AppID          int64  // GitHub App ID
	InstallationID int64  // Installation ID for the app
	PrivateKeyFile string // Path to the private key file
}

// AppAuthTransport handles GitHub App authentication with automatic token refresh
type AppAuthTransport struct {
	config        GitHubAppConfig
	privateKey    *rsa.PrivateKey
	mu            sync.RWMutex
	accessToken   string
	tokenExpiry   time.Time
	baseTransport http.RoundTripper
}

// NewAppAuthTransport creates a new GitHub App authentication transport
func NewAppAuthTransport(config GitHubAppConfig, baseTransport http.RoundTripper) (*AppAuthTransport, error) {
	// Load and parse the private key
	privateKey, err := loadPrivateKeyFromFile(config.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load private key: %v", err)
	}

	transport := &AppAuthTransport{
		config:        config,
		privateKey:    privateKey,
		baseTransport: baseTransport,
	}

	// Get initial access token
	if err := transport.refreshToken(); err != nil {
		return nil, fmt.Errorf("failed to get initial access token: %v", err)
	}

	return transport, nil
}

// RoundTrip implements http.RoundTripper interface
func (t *AppAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Check if token needs refresh (refresh 5 minutes before expiry)
	t.mu.RLock()
	needsRefresh := time.Now().Add(5 * time.Minute).After(t.tokenExpiry)
	currentToken := t.accessToken
	t.mu.RUnlock()

	if needsRefresh {
		if err := t.refreshToken(); err != nil {
			logger.Error("failed to refresh GitHub App token", "error", err)
			// Continue with current token if refresh fails
		} else {
			t.mu.RLock()
			currentToken = t.accessToken
			t.mu.RUnlock()
		}
	}

	// Clone the request and add authorization header
	reqClone := req.Clone(req.Context())
	reqClone.Header.Set("Authorization", "Bearer "+currentToken)
	reqClone.Header.Set("Accept", "application/vnd.github.v3+json")

	return t.baseTransport.RoundTrip(reqClone)
}

// refreshToken gets a new installation access token
func (t *AppAuthTransport) refreshToken() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Create JWT for app authentication
	jwtToken, err := t.createJWTToken()
	if err != nil {
		return fmt.Errorf("failed to create JWT token: %v", err)
	}

	// Create a temporary client to get installation access token
	tempClient := &http.Client{Transport: t.baseTransport}

	// Create request to get installation access token
	req, err := http.NewRequest("POST",
		fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", t.config.InstallationID),
		nil)
	if err != nil {
		return fmt.Errorf("failed to create access token request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "gitlab-migrator")

	resp, err := tempClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request access token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get access token, status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Parse the response
	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := github.CheckResponse(resp); err != nil {
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %v", err)
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("failed to parse token response: %v", err)
	}

	// Update token and expiry
	t.accessToken = tokenResp.Token
	t.tokenExpiry = tokenResp.ExpiresAt

	logger.Info("refreshed GitHub App access token",
		"expires_at", t.tokenExpiry.Format(time.RFC3339),
		"valid_for", time.Until(t.tokenExpiry).Round(time.Minute))

	return nil
}

// createJWTToken creates a JWT token for GitHub App authentication
func (t *AppAuthTransport) createJWTToken() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(), // JWT expires in 10 minutes
		"iss": t.config.AppID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(t.privateKey)
}

// loadPrivateKeyFromFile loads an RSA private key from a file
func loadPrivateKeyFromFile(filename string) (*rsa.PrivateKey, error) {
	keyData, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %v", err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	var privateKey *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		privateKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse PKCS8 private key: %v", err)
		}
		var ok bool
		privateKey, ok = key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
	default:
		return nil, fmt.Errorf("unsupported private key type: %s", block.Type)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %v", err)
	}

	return privateKey, nil
}

// createGithubClientWithPAT creates a GitHub client using Personal Access Token authentication
func createGithubClientWithPAT(retryClient *retryablehttp.Client, token string) *github.Client {
	client := githubpagination.NewClient(
		&retryablehttp.RoundTripper{Client: retryClient},
		githubpagination.WithPerPage(100))

	return github.NewClient(client).WithAuthToken(token)
}

// createGithubClientWithApp creates a GitHub client using GitHub App authentication
func createGithubClientWithApp(retryClient *retryablehttp.Client, config GitHubAppConfig) (*github.Client, error) {
	// Create the app auth transport with retry client as base transport
	appTransport, err := NewAppAuthTransport(config, &retryablehttp.RoundTripper{Client: retryClient})
	if err != nil {
		return nil, fmt.Errorf("failed to create app auth transport: %v", err)
	}

	// Create pagination client with app auth transport
	paginationClient := githubpagination.NewClient(appTransport, githubpagination.WithPerPage(100))

	return github.NewClient(paginationClient), nil
}

// parseGitHubAppConfigFromEnv parses GitHub App configuration from environment variables
func parseGitHubAppConfigFromEnv() (GitHubAppConfig, error) {
	var config GitHubAppConfig
	var err error

	appIDStr := os.Getenv("GITHUB_APP_ID")
	if appIDStr == "" {
		return config, fmt.Errorf("GITHUB_APP_ID environment variable is required")
	}
	config.AppID, err = strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return config, fmt.Errorf("invalid GITHUB_APP_ID: %v", err)
	}

	installationIDStr := os.Getenv("GITHUB_INSTALLATION_ID")
	if installationIDStr == "" {
		return config, fmt.Errorf("GITHUB_INSTALLATION_ID environment variable is required")
	}
	config.InstallationID, err = strconv.ParseInt(installationIDStr, 10, 64)
	if err != nil {
		return config, fmt.Errorf("invalid GITHUB_INSTALLATION_ID: %v", err)
	}

	config.PrivateKeyFile = os.Getenv("GITHUB_PRIVATE_KEY_FILE")
	if config.PrivateKeyFile == "" {
		return config, fmt.Errorf("GITHUB_PRIVATE_KEY_FILE environment variable is required")
	}

	return config, nil
}
