// Package github wraps the GitHub App auth flow and exposes a small
// surface the webhook handler needs: mint installation tokens (cached)
// and fetch pull requests by number.
package github

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
)

// Client is bound to a single GitHub App installation. It mints and
// caches its own installation token; safe for concurrent use.
type Client struct {
	m              *Manager
	installationID int64

	mu      sync.Mutex
	cached  *github.Client
	expires time.Time
}

// installationClient returns a GitHub client authenticated as this
// installation, reusing a cached one if its token is still valid.
func (c *Client) installationClient(ctx context.Context) (*github.Client, error) {
	c.mu.Lock()
	if c.cached != nil && time.Until(c.expires) > tokenTTLRefresh {
		defer c.mu.Unlock()
		return c.cached, nil
	}
	c.mu.Unlock()

	jwtStr, err := c.m.appJWT(time.Now())
	if err != nil {
		return nil, fmt.Errorf("mint app JWT: %w", err)
	}
	app := github.NewClient(c.m.baseHTTP).WithAuthToken(jwtStr)
	tok, _, err := app.Apps.CreateInstallationToken(ctx, c.installationID, nil)
	if err != nil {
		return nil, fmt.Errorf("create installation token: %w", err)
	}
	inst := github.NewClient(c.m.baseHTTP).WithAuthToken(tok.GetToken())

	c.mu.Lock()
	c.cached = inst
	c.expires = tok.GetExpiresAt().Time
	c.mu.Unlock()

	return inst, nil
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

// priorReviewHeaderRegex matches the headers Boop's runner posts on a
// summary comment, for any review number. Used to count how many
// reviews have already been posted on a PR so the next run can label
// itself #N (or "re-review #N" when N > 1).
//
// Must stay in lockstep with ReviewSummaryHeader and
// apps/runner/src/review-header.mjs.
//
//	## 🐾 Boop's review
//	## 🐾 Boop's re-review
//	## 🐾 Boop's re-review #2
var priorReviewHeaderRegex = regexp.MustCompile(`(?m)^##\s+🐾\s+Boop's\s+(?:re-)?review(?:\s+#\d+)?\b`)

// priorReviewHeadSHARegex matches the hidden SHA marker the runner
// appends to every summary comment. Carries the full head SHA so
// re-reviews can diff the delta from the previously reviewed commit
// instead of the full main..head range. Must stay in lockstep with
// the footer emitted in apps/runner/src/index.mjs:postReview.
//
//	<!-- boop-head-sha: 87bcc09...full 40-char sha... -->
var priorReviewHeadSHARegex = regexp.MustCompile(`<!--\s*boop-head-sha:\s*([0-9a-f]{7,40})\s*-->`)

// ReviewSummaryHeader is the H2 the runner posts at the top of each
// summary comment. n is 1-based; n <= 1 is the first review.
// Keep identical to apps/runner/src/review-header.mjs.
func ReviewSummaryHeader(n int) string {
	if n <= 1 {
		return "## 🐾 Boop's review"
	}
	return fmt.Sprintf("## 🐾 Boop's re-review #%d", n)
}

// IsBoopReviewSummary reports whether a comment body is a Boop
// review summary (matched on its leading H2 header).
func IsBoopReviewSummary(body string) bool {
	return priorReviewHeaderRegex.MatchString(body)
}

// extractPriorReviewSHA returns the head SHA hidden in a Boop
// summary comment, or "" if the marker is absent (older summaries
// posted before the marker existed, or comments that aren't Boop
// summaries — the caller should pre-check via IsBoopReviewSummary).
func extractPriorReviewSHA(body string) string {
	m := priorReviewHeadSHARegex.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// CountPriorReviews returns the number of Boop summary comments
// already on the issue/PR, plus the head SHA of the MOST RECENT one
// (empty if no SHA marker was present). The next review's number is
// count + 1; lastReviewedSHA lets the next run diff only the delta
// from that commit instead of the full main..head range.
//
// Paginates with a large page size; PRs rarely carry more than a
// handful of reviews, so the cost is negligible.
func (c *Client) CountPriorReviews(ctx context.Context, owner, repo string, issueNumber int) (int, string, error) {
	client, err := c.installationClient(ctx)
	if err != nil {
		return 0, "", err
	}
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	count := 0
	lastSHA := ""
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, issueNumber, opts)
		if err != nil {
			return 0, "", fmt.Errorf("list comments: %w", err)
		}
		// GitHub returns comments oldest-first; the last Boop summary
		// we see is the most recent prior review.
		for _, comment := range comments {
			body := comment.GetBody()
			if !IsBoopReviewSummary(body) {
				continue
			}
			count++
			if sha := extractPriorReviewSHA(body); sha != "" {
				lastSHA = sha
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, lastSHA, nil
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
