// Package github wraps the GitHub App auth flow and exposes a small
// surface the webhook handler needs: mint installation tokens (with
// in-memory caching) and fetch pull requests by number.
package github

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v68/github"
)

// AppConfig holds the static credentials of the GitHub App.
type AppConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKey     *rsa.PrivateKey
}

// Client mints installation tokens (cached until ~5min before expiry)
// and exposes a small PR-fetcher used by the webhook handler.
type Client struct {
	cfg AppConfig

	mu      sync.Mutex
	cached  *github.Client
	expires time.Time
}

// NewClient returns a Client that can talk to the GitHub API on
// behalf of the App's installation. Safe for concurrent use.
func NewClient(cfg AppConfig) *Client {
	return &Client{cfg: cfg}
}

const (
	httpTimeout     = 15 * time.Second
	tokenTTLRefresh = 5 * time.Minute
	appJWTTTL       = 10 * time.Minute
	appJWTLeeway    = 30 * time.Second
)

// appJWT mints a short-lived JWT signed with the App's private key,
// used to call the /app endpoints.
func (c *Client) appJWT(now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", c.cfg.AppID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-appJWTLeeway)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTTTL)),
	})
	return tok.SignedString(c.cfg.PrivateKey)
}

// installationClient returns a GitHub client authenticated as the
// installation, reusing a cached one if its token is still valid.
func (c *Client) installationClient(ctx context.Context) (*github.Client, error) {
	c.mu.Lock()
	if c.cached != nil && time.Until(c.expires) > tokenTTLRefresh {
		defer c.mu.Unlock()
		return c.cached, nil
	}
	c.mu.Unlock()

	jwtStr, err := c.appJWT(time.Now())
	if err != nil {
		return nil, fmt.Errorf("mint app JWT: %w", err)
	}
	client := github.NewClient(&http.Client{Timeout: httpTimeout}).WithAuthToken(jwtStr)
	tok, _, err := client.Apps.CreateInstallationToken(ctx, c.cfg.InstallationID, nil)
	if err != nil {
		return nil, fmt.Errorf("create installation token: %w", err)
	}
	client = github.NewClient(&http.Client{Timeout: httpTimeout}).WithAuthToken(tok.GetToken())

	c.mu.Lock()
	c.cached = client
	c.expires = tok.GetExpiresAt().Time
	c.mu.Unlock()

	return client, nil
}

// PullRequest fetches a pull request by owner/repo/number and returns
// the head SHA + base ref used to build a Job.
type PullRequest struct {
	Number  int
	HeadSHA string
	BaseRef string
	Title   string
}

// FetchPullRequest retrieves the PR's current head SHA and base ref.
func (c *Client) FetchPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	client, err := c.installationClient(ctx)
	if err != nil {
		return nil, err
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get pull request: %w", err)
	}
	if pr.Head == nil || pr.Head.SHA == nil || pr.Base == nil || pr.Base.Ref == nil {
		return nil, fmt.Errorf("pull request missing head/base fields")
	}
	return &PullRequest{
		Number:  pr.GetNumber(),
		HeadSHA: pr.GetHead().GetSHA(),
		BaseRef: pr.GetBase().GetRef(),
		Title:   pr.GetTitle(),
	}, nil
}

// AddCommentReaction drops an emoji reaction on an issue or PR comment.
// `content` is one of: "+1", "-1", "laugh", "hooray", "confused",
// "heart", "rocket", "eyes".
func (c *Client) AddCommentReaction(ctx context.Context, owner, repo string, commentID int64, content string) error {
	client, err := c.installationClient(ctx)
	if err != nil {
		return err
	}
	_, _, err = client.Reactions.CreateCommentReaction(ctx, owner, repo, commentID, content)
	return err
}

// PostIssueComment creates a new comment on an issue or PR and returns
// the new comment ID.
func (c *Client) PostIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) (int64, error) {
	client, err := c.installationClient(ctx)
	if err != nil {
		return 0, err
	}
	comment, _, err := client.Issues.CreateComment(ctx, owner, repo, issueNumber, &github.IssueComment{Body: ptrString(body)})
	if err != nil {
		return 0, err
	}
	return comment.GetID(), nil
}

// UpdateIssueComment edits an existing issue/PR comment in place.
func (c *Client) UpdateIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	client, err := c.installationClient(ctx)
	if err != nil {
		return err
	}
	_, _, err = client.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{Body: ptrString(body)})
	return err
}

func ptrString(s string) *string { return &s }
