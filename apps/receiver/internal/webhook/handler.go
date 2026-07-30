// Package webhook implements the GitHub webhook receiver.
//
// Responsibilities:
//   - Verify the X-Hub-Signature-256 HMAC on every request.
//   - Filter to pull_request events (opened/reopened/synchronize) and
//     issue_comment events mentioning @BoopPr.
//   - Render the Job template and submit it via the in-cluster K8s API.
//   - Be idempotent: re-deliveries for the same head SHA are no-ops.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
)

const BotMention = "@BoopPr"

// mentionRegex matches `@BoopPr` only when it is not part of a longer
// GitHub username (e.g. `@BoopPr-bot`). GitHub usernames may contain
// `[A-Za-z0-9-]`, so we require the character after `r` (if any) to
// not be one of those.
var mentionRegex = regexp.MustCompile(`(?i)@BoopPr(?:$|[^A-Za-z0-9_-])`)

func mentionsBot(body string) bool {
	return mentionRegex.MatchString(body)
}

type Config struct {
	Port            string
	WebhookSecret   string
	JobImage        string
	TargetNamespace string
	BotLogin        string // GitHub login of the bot, used to ignore self-comments
}

type Handler struct {
	cfg      Config
	logger   *slog.Logger
	kube     kubernetes.Interface
	ghClient *boopgithub.Client
}

func NewHandler(cfg Config, ghClient *boopgithub.Client, logger *slog.Logger) (*Handler, error) {
	kube, err := newInClusterClient()
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	return &Handler{cfg: cfg, logger: logger, kube: kube, ghClient: ghClient}, nil
}

func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.logger.Warn("read body", "delivery", deliveryID, "err", err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if !verifySignature(sig, body, h.cfg.WebhookSecret) {
		h.logger.Warn("invalid signature", "delivery", deliveryID)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch event {
	case "pull_request":
		h.handlePullRequest(ctx, w, deliveryID, body)
	case "issue_comment":
		h.handleIssueComment(ctx, w, deliveryID, body)
	default:
		h.logger.Debug("ignored event", "delivery", deliveryID, "event", event)
		writeAck(w, "ignored", "event not handled", deliveryID)
	}
}

func (h *Handler) handlePullRequest(ctx context.Context, w http.ResponseWriter, delivery string, body []byte) {
	pr, err := parsePullRequest(body)
	if err != nil {
		h.logger.Warn("parse pull_request", "delivery", delivery, "err", err)
		http.Error(w, "parse pull_request", http.StatusBadRequest)
		return
	}
	if !isReviewableAction(pr.Action) {
		h.logger.Debug("ignored action", "delivery", delivery, "action", pr.Action)
		writeAck(w, "ignored", "action not reviewable", delivery)
		return
	}
	// Check for duplicate before posting a status comment — otherwise a
	// re-delivery of the same head SHA leaves a stranded "👀" comment
	// that no runner will update.
	if !h.claimJobSlot(ctx, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, w) {
		return
	}
	// No trigger comment to react to for plain PR events. Post a fresh
	// status comment and pass its id to the Job.
	statusID, err := h.postStatus(ctx, pr.Owner, pr.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, ""))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
		// Non-fatal — the Job still runs.
	}
	h.submitJob(ctx, w, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, fmt.Sprintf("pull_request.%s", pr.Action), 0, statusID)
}

func (h *Handler) handleIssueComment(ctx context.Context, w http.ResponseWriter, delivery string, body []byte) {
	ic, err := parseIssueComment(body)
	if err != nil {
		h.logger.Warn("parse issue_comment", "delivery", delivery, "err", err)
		http.Error(w, "parse issue_comment", http.StatusBadRequest)
		return
	}
	if ic.Action != "created" {
		h.logger.Debug("ignored comment action", "delivery", delivery, "action", ic.Action)
		writeAck(w, "ignored", "comment action not created", delivery)
		return
	}
	if ic.IssuePullRequestURL == "" {
		h.logger.Debug("comment on a plain issue, not a PR", "delivery", delivery, "issue", ic.IssueNumber)
		writeAck(w, "ignored", "comment is on an issue, not a PR", delivery)
		return
	}
	if h.cfg.BotLogin != "" && strings.EqualFold(ic.SenderLogin, h.cfg.BotLogin) {
		h.logger.Debug("ignored self-mention", "delivery", delivery, "sender", ic.SenderLogin)
		writeAck(w, "ignored", "self-mention", delivery)
		return
	}
	if !mentionsBot(ic.CommentBody) {
		h.logger.Debug("comment does not mention bot", "delivery", delivery)
		writeAck(w, "ignored", "no bot mention", delivery)
		return
	}

	// React to the trigger comment with 👀 so the user sees we saw it.
	if err := h.ghClient.AddCommentReaction(ctx, ic.Owner, ic.Repo, ic.CommentID, "eyes"); err != nil {
		h.logger.Warn("add reaction", "delivery", delivery, "err", err)
	}

	pr, err := h.ghClient.FetchPullRequest(ctx, ic.Owner, ic.Repo, ic.IssueNumber)
	if err != nil {
		h.logger.Error("fetch pull request", "delivery", delivery, "pr", fmt.Sprintf("%s/%s#%d", ic.Owner, ic.Repo, ic.IssueNumber), "err", err)
		http.Error(w, "fetch pr", http.StatusBadGateway)
		return
	}

	// Check for duplicate (active or succeeded) before posting a status
	// comment — otherwise a duplicate trigger leaves a stranded
	// "👀 reviewing..." comment that the runner never updates.
	if !h.claimJobSlot(ctx, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, w) {
		return
	}

	// Post a status comment the runner will PATCH as it progresses.
	statusID, err := h.postStatus(ctx, ic.Owner, ic.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, ic.SenderLogin))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
	}

	h.submitJob(ctx, w, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, fmt.Sprintf("issue_comment.by=%s", ic.SenderLogin), ic.CommentID, statusID)
}

// Status stages used in renderStatusBody. The runner can refer to the
// same stage constants when it PATCHes the comment.
const (
	StatusInitial = "initial"
	StatusAuth    = "auth"
	StatusClone   = "clone"
	StatusReview  = "review"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

func renderStatusBody(stage, sha, by string) string {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	byLine := ""
	if by != "" {
		byLine = fmt.Sprintf("Triggered by @%s\n\n", by)
	}
	switch stage {
	case StatusInitial:
		return fmt.Sprintf("👀 **boop is reviewing this PR...**\n\n%sLast commit: `%s`. Updates will appear here.", byLine, short)
	case StatusAuth:
		return fmt.Sprintf("🔐 **boop is reviewing this PR** — authenticated with GitHub.\n\n%sLast commit: `%s`.", byLine, short)
	case StatusClone:
		return fmt.Sprintf("📥 **boop is reviewing this PR** — cloned the repo at `%s`.\n\n%sChecking out the PR head and starting the multi-lens review.", short, byLine)
	case StatusReview:
		return fmt.Sprintf("🧠 **boop is reviewing this PR** — running the multi-lens review (boop skill) on `%s`.", short)
	case StatusDone:
		return fmt.Sprintf("✅ **boop review complete.** See the review comment below.")
	case StatusFailed:
		return fmt.Sprintf("❌ **boop review failed.** Check the Job logs for details.")
	}
	return fmt.Sprintf("boop status: %s", stage)
}

func (h *Handler) postStatus(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	return h.ghClient.PostIssueComment(ctx, owner, repo, number, body)
}

// claimJobSlot checks whether a Job already exists for this head SHA. If
// the job is active or already succeeded, it writes the duplicate ack
// and returns false so the caller skips posting a status comment. If
// the job is failed, deletes it and returns true so a fresh one is
// created. If missing, returns true. On kube errors it writes a 500.
func (h *Handler) claimJobSlot(ctx context.Context, delivery, owner, repo string, number int, headSHA string, w http.ResponseWriter) bool {
	jobName := buildJobName(owner, repo, number, headSHA)
	status, err := h.jobStatus(ctx, jobName)
	if err != nil {
		h.logger.Error("check job", "delivery", delivery, "job", jobName, "err", err)
		http.Error(w, "kube error", http.StatusInternalServerError)
		return false
	}
	switch status {
	case "active":
		h.logger.Info("duplicate delivery, job still active", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false
	case "succeeded":
		h.logger.Info("duplicate delivery, job already succeeded", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false
	case "failed":
		h.logger.Info("replacing failed job", "delivery", delivery, "job", jobName)
		if err := h.deleteJob(ctx, jobName); err != nil && !isNotFound(err) {
			h.logger.Error("delete failed job", "delivery", delivery, "job", jobName, "err", err)
			http.Error(w, "kube error", http.StatusInternalServerError)
			return false
		}
	}
	return true
}

func (h *Handler) submitJob(ctx context.Context, w http.ResponseWriter, delivery, owner, repo string, number int, headSHA, baseRef, reason string, reactionCommentID, statusCommentID int64) {
	jobName := buildJobName(owner, repo, number, headSHA)

	job, err := renderJobTemplate(jobTemplate, templateVars{
		Owner:            owner,
		Repo:             repo,
		Number:           fmt.Sprintf("%d", number),
		SHA:              headSHA,
		SHA7:             shortSHA(headSHA),
		BaseRef:          baseRef,
		Image:            h.cfg.JobImage,
		StatusCommentID:  fmt.Sprintf("%d", statusCommentID),
		ReactionCommentID: fmt.Sprintf("%d", reactionCommentID),
	})
	if err != nil {
		h.logger.Error("render job", "delivery", delivery, "err", err)
		http.Error(w, "render job", http.StatusInternalServerError)
		return
	}

	if err := h.createJob(ctx, job); err != nil {
		h.logger.Error("create job", "delivery", delivery, "job", jobName, "err", err)
		http.Error(w, "create job", http.StatusInternalServerError)
		return
	}

	h.logger.Info("job created",
		"delivery", delivery,
		"job", jobName,
		"pr", fmt.Sprintf("%s/%s#%d", owner, repo, number),
		"sha", headSHA,
		"reason", reason,
		"status_comment_id", statusCommentID,
		"reaction_comment_id", reactionCommentID,
	)
	writeAck(w, "accepted", jobName, delivery)
}

type templateVars struct {
	Owner            string
	Repo             string
	Number           string
	SHA              string
	SHA7             string
	BaseRef          string
	Image            string
	StatusCommentID  string // GitHub comment id for live status updates (empty if none)
	ReactionCommentID string // GitHub comment id that received the trigger reaction (empty if none)
}

func renderJobTemplate(tpl string, v templateVars) (*batchv1.Job, error) {
	rendered := tpl
	for _, p := range []struct{ old, new string }{
		{"__OWNER__", v.Owner},
		{"__REPO__", v.Repo},
		{"__NUMBER__", v.Number},
		{"__SHA__", v.SHA},
		{"__SHA7__", v.SHA7},
		{"__BASE_REF__", v.BaseRef},
		{"__IMAGE__", v.Image},
		{"__STATUS_COMMENT_ID__", v.StatusCommentID},
		{"__REACTION_COMMENT_ID__", v.ReactionCommentID},
	} {
		rendered = strings.ReplaceAll(rendered, p.old, p.new)
	}

	job := &batchv1.Job{}
	if err := yamlUnmarshal([]byte(rendered), job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	return job, nil
}

type prMeta struct {
	Action  string
	Owner   string
	Repo    string
	Number  int
	HeadSHA string
	BaseRef string
}

func parsePullRequest(body []byte) (prMeta, error) {
	var out prMeta
	event, err := github.ParseWebHook("pull_request", body)
	if err != nil {
		return out, fmt.Errorf("parse webhook: %w", err)
	}
	pr, ok := event.(*github.PullRequestEvent)
	if !ok || pr.PullRequest == nil || pr.Repo == nil || pr.Repo.Owner == nil {
		return out, errors.New("malformed pull_request event")
	}
	if pr.Action == nil || pr.PullRequest.Head == nil || pr.PullRequest.Head.SHA == nil ||
		pr.PullRequest.Base == nil || pr.PullRequest.Base.Ref == nil {
		return out, errors.New("missing required pull_request fields")
	}
	out.Action = *pr.Action
	out.Owner = *pr.Repo.Owner.Login
	out.Repo = *pr.Repo.Name
	out.Number = pr.GetNumber()
	out.HeadSHA = *pr.PullRequest.Head.SHA
	out.BaseRef = *pr.PullRequest.Base.Ref
	return out, nil
}

// issueCommentMeta is the slice of the issue_comment payload the
// receiver needs. The webhook only fires for PR comments when the
// user mentions the bot.
type issueCommentMeta struct {
	Action              string
	Owner               string
	Repo                string
	IssueNumber         int
	IssuePullRequestURL string // non-empty => this is a PR comment, not a plain issue
	CommentID           int64
	CommentBody         string
	SenderLogin         string
}

func parseIssueComment(body []byte) (issueCommentMeta, error) {
	var out issueCommentMeta
	event, err := github.ParseWebHook("issue_comment", body)
	if err != nil {
		return out, fmt.Errorf("parse webhook: %w", err)
	}
	ic, ok := event.(*github.IssueCommentEvent)
	if !ok || ic.Issue == nil || ic.Repo == nil || ic.Repo.Owner == nil || ic.Comment == nil || ic.Sender == nil {
		return out, errors.New("malformed issue_comment event")
	}
	if ic.Action == nil || ic.Comment.Body == nil || ic.Sender.Login == nil || ic.Issue.Number == nil {
		return out, errors.New("missing required issue_comment fields")
	}
	out.Action = *ic.Action
	out.Owner = *ic.Repo.Owner.Login
	out.Repo = *ic.Repo.Name
	out.IssueNumber = ic.Issue.GetNumber()
	if ic.Issue.PullRequestLinks != nil {
		out.IssuePullRequestURL = ic.Issue.PullRequestLinks.GetURL()
	}
	out.CommentID = ic.Comment.GetID()
	out.CommentBody = *ic.Comment.Body
	out.SenderLogin = *ic.Sender.Login
	return out, nil
}

func isReviewableAction(a string) bool {
	switch a {
	case "opened", "reopened", "synchronize", "ready_for_review":
		return true
	}
	return false
}

var jobNameSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

func buildJobName(owner, repo string, number int, sha string) string {
	raw := fmt.Sprintf("boop-%s-%s-%d-%s", owner, repo, number, shortSHA(sha))
	return jobNameSanitizer.ReplaceAllString(strings.ToLower(raw), "-")
}

func shortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

// jobStatus returns one of: "missing", "active", "failed", "succeeded".
// Used to decide whether to create, skip (duplicate), or replace
// (failed) a Job for a given head SHA.
func (h *Handler) jobStatus(ctx context.Context, name string) (string, error) {
	job, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return "missing", nil
		}
		return "", err
	}
	if job.Status.Failed > 0 {
		return "failed", nil
	}
	if job.Status.Succeeded > 0 {
		return "succeeded", nil
	}
	return "active", nil
}

func (h *Handler) deleteJob(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationBackground
	return h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}

func (h *Handler) createJob(ctx context.Context, job *batchv1.Job) error {
	_, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

func verifySignature(header string, body []byte, secret string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(header, "sha256=")
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

func writeAck(w http.ResponseWriter, status, detail, delivery string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   status,
		"detail":   detail,
		"delivery": delivery,
		"ts":       time.Now().UTC().Format(time.RFC3339),
	})
}
