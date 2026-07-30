// Package webhook implements the GitHub webhook receiver.
//
// Responsibilities:
//   - Verify the X-Hub-Signature-256 HMAC on every request.
//   - Filter to pull_request events (opened/reopened/synchronize) and
//     issue_comment events that ask for a review with `@BoopPr review`.
//   - Render the Job template and submit it via the in-cluster K8s API.
//   - Be idempotent: re-deliveries for the same head SHA are no-ops.
//   - Number every review so re-reviews get their own header instead
//     of rewriting the original.
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
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
)

const BotMention = "@BoopPr"

// reviewRequestRegex matches an explicit request to Boop to review
// the PR. We accept the literal phrase plus a small set of natural
// request modifiers so common phrasings work.
//
// Matches:
//
//	@BoopPr review
//	@BoopPr, review
//	@BoopPr review please
//	@BoopPr please review
//	@BoopPr to review
//	@BoopPr can review
//	@BoopPr can you review
//	@BoopPr could you review
//	@BoopPr re-review
//
// Does NOT match (a bare mention or a reference, not a request):
//
//	@BoopPr hi
//	@BoopPr look at this code review carefully
//	@BoopPr the prior review was great
//	@BoopPr-bot review
//	@BoopPr2 review
var reviewRequestRegex = regexp.MustCompile(
	`(?i)@BoopPr\b,?\s+(?:please\s+|to\s+|can\s+(?:you\s+)?|could\s+(?:you\s+)?|will\s+(?:you\s+)?|may\s+(?:you\s+)?)?(?:re-)?review\b`,
)

// requestsReview reports whether the comment body asks Boop to
// review the PR. Bare mentions like `@BoopPr hi` do not count.
func requestsReview(body string) bool {
	return reviewRequestRegex.MatchString(body)
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
	ghClient *boopgithub.Manager
}

func NewHandler(cfg Config, ghClient *boopgithub.Manager, logger *slog.Logger) (*Handler, error) {
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

	installationID, err := resolveInstallationID(r.Header.Get("X-GitHub-Installation-ID"), body)
	if err != nil {
		h.logger.Warn("invalid installation id", "delivery", deliveryID, "err", err)
		http.Error(w, "invalid installation id", http.StatusBadRequest)
		return
	}

	switch event {
	case "pull_request":
		h.handlePullRequest(ctx, w, deliveryID, installationID, body)
	case "issue_comment":
		h.handleIssueComment(ctx, w, deliveryID, installationID, body)
	default:
		h.logger.Debug("ignored event", "delivery", deliveryID, "event", event)
		writeAck(w, "ignored", "event not handled", deliveryID)
	}
}

func (h *Handler) handlePullRequest(ctx context.Context, w http.ResponseWriter, delivery string, installationID int64, body []byte) {
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
	client := h.ghClient.ClientFor(installationID)
	// Check for duplicate before posting a status comment — otherwise a
	// re-delivery of the same head SHA leaves a stranded "👀" comment
	// that no runner will update.
	if claimed, _ := h.claimJobSlot(ctx, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, w); !claimed {
		return
	}
	reviewNumber, err := h.computeReviewNumber(ctx, client, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		h.logger.Warn("count prior reviews", "delivery", delivery, "err", err)
		// Non-fatal — fall back to 1; the runner still runs.
		reviewNumber = 1
	}
	// No trigger comment to react to for plain PR events. Post a fresh
	// status comment and pass its id to the Job.
	statusID, err := h.postStatus(ctx, client, pr.Owner, pr.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, "", reviewNumber))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
		// Non-fatal — the Job still runs.
	}
	h.submitJob(ctx, w, delivery, pr.Owner, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, fmt.Sprintf("pull_request.%s", pr.Action), 0, statusID, installationID, reviewNumber)
}

func (h *Handler) handleIssueComment(ctx context.Context, w http.ResponseWriter, delivery string, installationID int64, body []byte) {
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
	if !requestsReview(ic.CommentBody) {
		h.logger.Debug("comment does not request review", "delivery", delivery)
		writeAck(w, "ignored", "no `@BoopPr review` request", delivery)
		return
	}

	client := h.ghClient.ClientFor(installationID)
	pr, err := client.FetchPullRequest(ctx, ic.Owner, ic.Repo, ic.IssueNumber)
	if err != nil {
		h.logger.Error("fetch pull request", "delivery", delivery, "pr", fmt.Sprintf("%s/%s#%d", ic.Owner, ic.Repo, ic.IssueNumber), "err", err)
		http.Error(w, "fetch pr", http.StatusBadGateway)
		return
	}

	// Check for duplicate (active or succeeded) before reacting or
	// posting a status comment — otherwise a same-SHA `@BoopPr review`
	// leaves 👀 / a stranded "reviewing..." comment with no runner.
	claimed, dupeStatus := h.claimJobSlot(ctx, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, w)
	if !claimed {
		if dupeStatus == "active" || dupeStatus == "succeeded" {
			h.replyDuplicateReview(ctx, client, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, dupeStatus)
		}
		return
	}

	// React only after we know a run will start, so no-ops do not look
	// like work is underway.
	if err := client.AddCommentReaction(ctx, ic.Owner, ic.Repo, ic.CommentID, "eyes"); err != nil {
		h.logger.Warn("add reaction", "delivery", delivery, "err", err)
	}

	reviewNumber, err := h.computeReviewNumber(ctx, client, ic.Owner, ic.Repo, pr.Number)
	if err != nil {
		h.logger.Warn("count prior reviews", "delivery", delivery, "err", err)
		reviewNumber = 1
	}

	// Post a status comment the runner will PATCH as it progresses.
	statusID, err := h.postStatus(ctx, client, ic.Owner, ic.Repo, pr.Number, renderStatusBody(StatusInitial, pr.HeadSHA, ic.SenderLogin, reviewNumber))
	if err != nil {
		h.logger.Warn("post status comment", "delivery", delivery, "err", err)
	}

	h.submitJob(ctx, w, delivery, ic.Owner, ic.Repo, pr.Number, pr.HeadSHA, pr.BaseRef, fmt.Sprintf("issue_comment.by=%s", ic.SenderLogin), ic.CommentID, statusID, installationID, reviewNumber)
}

// computeReviewNumber returns the 1-based index of the review this
// run will produce. Counts existing Boop summary comments on the PR
// and adds one. Falls back to 1 on error so a transient GitHub
// hiccup never blocks a review.
func (h *Handler) computeReviewNumber(ctx context.Context, client *boopgithub.Client, owner, repo string, number int) (int, error) {
	n, err := client.CountPriorReviews(ctx, owner, repo, number)
	if err != nil {
		return 0, err
	}
	return n + 1, nil
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

	// statusTimelineSep is appended to the initial status body so the
	// runner's PATCH path can stay append-only — it never overwrites
	// the receiver-supplied header (which carries the review label and
	// trigger attribution).
	statusTimelineSep = "<!-- boop-timeline -->"
)

func renderStatusBody(stage, sha, by string, reviewNumber int) string {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	byLine := ""
	if by != "" {
		byLine = fmt.Sprintf("Triggered by @%s\n\n", by)
	}
	// reviewLabel disambiguates re-reviews in the status thread so
	// users can tell at a glance which run produced the comments.
	reviewLabel := "review"
	if reviewNumber > 1 {
		reviewLabel = fmt.Sprintf("re-review #%d", reviewNumber)
	}
	switch stage {
	case StatusInitial:
		return fmt.Sprintf("👀 **boop is reviewing this PR...** (%s)\n\n%sLast commit: `%s`. Updates will appear here.\n\n%s", reviewLabel, byLine, short, statusTimelineSep)
	case StatusAuth:
		return fmt.Sprintf("🔐 **boop is reviewing this PR** — authenticated with GitHub. (%s)\n\n%sLast commit: `%s`.", reviewLabel, byLine, short)
	case StatusClone:
		return fmt.Sprintf("📥 **boop is reviewing this PR** — cloned the repo at `%s`. (%s)\n\n%sChecking out the PR head and starting the multi-lens review.", short, reviewLabel, byLine)
	case StatusReview:
		return fmt.Sprintf("🧠 **boop is reviewing this PR** — running the multi-lens review (boop skill) on `%s`. (%s)", short, reviewLabel)
	case StatusDone:
		return fmt.Sprintf("✅ **boop %s complete.** See the review comment below.", reviewLabel)
	case StatusFailed:
		return fmt.Sprintf("❌ **boop %s failed.** Check the Job logs for details.", reviewLabel)
	}
	return fmt.Sprintf("boop status: %s", stage)
}

func (h *Handler) postStatus(ctx context.Context, client *boopgithub.Client, owner, repo string, number int, body string) (int64, error) {
	return client.PostIssueComment(ctx, owner, repo, number, body)
}

// claimJobSlot checks whether a Job already exists for this head SHA.
// Returns (true, "") when the caller may create a job.
// Returns (false, status) when the caller must skip: status is
// "active", "succeeded", or "error". On "active"/"succeeded" it writes
// a duplicate ack. On kube errors it writes a 500 and status "error".
// A failed job is deleted so a fresh one can be created.
func (h *Handler) claimJobSlot(ctx context.Context, delivery, owner, repo string, number int, headSHA string, w http.ResponseWriter) (bool, string) {
	jobName := buildJobName(owner, repo, number, headSHA)
	jobStatus, err := h.jobStatus(ctx, jobName)
	if err != nil {
		h.logger.Error("check job", "delivery", delivery, "job", jobName, "err", err)
		http.Error(w, "kube error", http.StatusInternalServerError)
		return false, "error"
	}
	switch jobStatus {
	case "active":
		h.logger.Info("duplicate delivery, job still active", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false, "active"
	case "succeeded":
		h.logger.Info("duplicate delivery, job already succeeded", "delivery", delivery, "job", jobName)
		writeAck(w, "duplicate", jobName, delivery)
		return false, "succeeded"
	case "failed":
		h.logger.Info("replacing failed job", "delivery", delivery, "job", jobName)
		if err := h.deleteJob(ctx, jobName); err != nil && !isNotFound(err) {
			h.logger.Error("delete failed job", "delivery", delivery, "job", jobName, "err", err)
			http.Error(w, "kube error", http.StatusInternalServerError)
			return false, "error"
		}
	}
	return true, ""
}

// replyDuplicateReview posts a short PR comment explaining why a same-SHA
// `@BoopPr review` request was a no-op. Non-fatal on API errors.
func (h *Handler) replyDuplicateReview(ctx context.Context, client *boopgithub.Client, delivery, owner, repo string, number int, headSHA, status string) {
	body := duplicateReviewReply(status, headSHA)
	if body == "" {
		return
	}
	if _, err := client.PostIssueComment(ctx, owner, repo, number, body); err != nil {
		h.logger.Warn("post duplicate-review reply", "delivery", delivery, "err", err)
	}
}

// duplicateReviewReply explains a same-SHA no-op in Boop's chrome
// voice (friendly pug, mascot OK). Findings stay plain; this is status.
func duplicateReviewReply(status, headSHA string) string {
	short := shortSHA(headSHA)
	switch status {
	case "active":
		return fmt.Sprintf(
			"🐾 **Already on it.** I'm still sniffing through `%s`. Hang tight — I'll post when that run finishes.",
			short,
		)
	case "succeeded":
		return fmt.Sprintf(
			"🐾 **Already sniffed `%s`.** Push a new commit and I'll take another pass. Want the same SHA again? Delete the Job for this head and ask me to review.",
			short,
		)
	default:
		return ""
	}
}

func (h *Handler) submitJob(ctx context.Context, w http.ResponseWriter, delivery, owner, repo string, number int, headSHA, baseRef, reason string, reactionCommentID, statusCommentID, installationID int64, reviewNumber int) {
	jobName := buildJobName(owner, repo, number, headSHA)

	job, err := renderJobTemplate(jobTemplate, templateVars{
		Owner:             owner,
		Repo:              repo,
		Number:            fmt.Sprintf("%d", number),
		SHA:               headSHA,
		SHA7:              shortSHA(headSHA),
		BaseRef:           baseRef,
		Image:             h.cfg.JobImage,
		StatusCommentID:   fmt.Sprintf("%d", statusCommentID),
		ReactionCommentID: fmt.Sprintf("%d", reactionCommentID),
		ReviewNumber:      fmt.Sprintf("%d", reviewNumber),
		InstallationID:    fmt.Sprintf("%d", installationID),
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
		"installation_id", installationID,
		"status_comment_id", statusCommentID,
		"reaction_comment_id", reactionCommentID,
		"review_number", reviewNumber,
	)
	writeAck(w, "accepted", jobName, delivery)
}

type templateVars struct {
	Owner             string
	Repo              string
	Number            string
	SHA               string
	SHA7              string
	BaseRef           string
	Image             string
	StatusCommentID   string // GitHub comment id for live status updates (empty if none)
	ReactionCommentID string // GitHub comment id that received the trigger reaction (empty if none)
	ReviewNumber      string // 1-based review number for this run; runner uses it for headers
	InstallationID    string // GitHub App installation ID, sourced from the webhook header
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
		{"__REVIEW_NUMBER__", v.ReviewNumber},
		{"__INSTALLATION_ID__", v.InstallationID},
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

// parseInstallationID reads the X-GitHub-Installation-ID header and
// returns the installation ID. Returns an error if the header is
// missing or unparseable.
func parseInstallationID(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("X-GitHub-Installation-ID header missing")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid X-GitHub-Installation-ID %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid X-GitHub-Installation-ID %q: must be positive", s)
	}
	return id, nil
}

// resolveInstallationID returns the installation ID for a webhook delivery.
// Tries the X-GitHub-Installation-ID header first (a proxy may inject it
// for routing), then falls back to installation.id in the JSON payload,
// which is the canonical location for GitHub App webhooks. Returns an
// error only if neither source is present — the receiver must not act
// on a webhook without knowing which installation fired it.
func resolveInstallationID(headerVal string, body []byte) (int64, error) {
	if id, err := parseInstallationID(headerVal); err == nil {
		return id, nil
	}
	var probe struct {
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Installation != nil && probe.Installation.ID > 0 {
		return probe.Installation.ID, nil
	}
	return 0, errors.New("installation id not present in header or payload")
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
