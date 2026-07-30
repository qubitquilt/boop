package webhook

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Review is one row in the /api/reviews response. Fields are JSON-tagged so
// the JSON shape is stable and decoupled from the K8s API types.
type Review struct {
	Name           string  `json:"name"`
	Namespace      string  `json:"namespace"`
	Owner          string  `json:"owner,omitempty"`
	Repo           string  `json:"repo,omitempty"`
	PR             int     `json:"pr,omitempty"`
	Commit         string  `json:"commit,omitempty"`
	BaseRef        string  `json:"baseRef,omitempty"`
	StartTime      string  `json:"startTime,omitempty"`
	CompletionTime string  `json:"completionTime,omitempty"`
	Duration       string  `json:"duration,omitempty"`
	Status         string  `json:"status"`
	Active         int32   `json:"active"`
	Succeeded      int32   `json:"succeeded"`
	Failed         int32   `json:"failed"`
}

// ReviewsResponse is the body of GET /api/reviews. The three slices are
// independent: a Job appears in exactly one bucket. Each list is sorted
// newest-first by StartTime.
type ReviewsResponse struct {
	Active []Review `json:"active"`
	Recent []Review `json:"recent"`
	Failed []Review `json:"failed"`
}

// reviewGrouping windows control how long a completed Job is kept in
// "recent" or "failed" before it ages out of the response. Tuned to be
// long enough for a human watching the dashboard but short enough that
// stale runs do not crowd the view.
const (
	recentWindow = 24 * time.Hour
	failedWindow = 7 * 24 * time.Hour
)

// ListReviews handles GET /api/reviews. It lists Jobs in the target
// namespace with label `app=boop`, groups them into active/recent/failed,
// and writes the result as JSON.
//
// The K8s API call is the only external dependency; a transient failure
// produces a 503 so the dashboard can retry. Non-boop Jobs in the
// namespace are filtered out at the API call via label selector.
func (h *Handler) ListReviews(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.kube.BatchV1().Jobs(h.cfg.TargetNamespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: "app=boop",
	})
	if err != nil {
		h.logger.Warn("list jobs", "namespace", h.cfg.TargetNamespace, "err", err)
		http.Error(w, "kube error", http.StatusServiceUnavailable)
		return
	}

	resp := collectReviews(jobs.Items, time.Now())
	writeJSON(w, http.StatusOK, resp)
}

// collectReviews is the pure, testable heart of ListReviews. It groups
// Jobs into active/recent/failed based on their K8s Status fields. Kept
// as a free function so tests can feed in a synthetic Job list without
// touching the fake clientset.
func collectReviews(jobs []batchv1.Job, now time.Time) ReviewsResponse {
	var resp ReviewsResponse
	for i := range jobs {
		job := &jobs[i]
		r := reviewFromJob(job)
		switch classifyJob(job, now) {
		case "active":
			resp.Active = append(resp.Active, r)
		case "recent":
			resp.Recent = append(resp.Recent, r)
		case "failed":
			resp.Failed = append(resp.Failed, r)
		}
	}
	sortReviews(resp.Active)
	sortReviews(resp.Recent)
	sortReviews(resp.Failed)
	return resp
}

// classifyJob picks the bucket for a single Job. The rules are:
//
//	active  — still running: Status.Active > 0, or has a StartTime and
//	          no CompletionTime and no Failed condition.
//	recent  — completed successfully within recentWindow.
//	failed  — has a Failed condition, or Status.Failed > 0, and the run
//	          started within failedWindow. (Active failures that have
//	          not yet produced a Completed condition are bucketed as
//	          "active" so the dashboard shows them as in-progress, not
//	          silently dropped.)
func classifyJob(job *batchv1.Job, now time.Time) string {
	st := job.Status

	switch {
	case hasFailedCondition(job):
		if st.StartTime != nil && now.Sub(st.StartTime.Time) <= failedWindow {
			return "failed"
		}
		return ""
	case st.Failed > 0:
		if st.StartTime != nil && now.Sub(st.StartTime.Time) <= failedWindow {
			return "failed"
		}
		return ""
	case st.Succeeded > 0:
		if st.CompletionTime != nil && now.Sub(st.CompletionTime.Time) <= recentWindow {
			return "recent"
		}
		return ""
	}

	if st.Active > 0 {
		return "active"
	}
	if st.StartTime != nil && st.CompletionTime == nil {
		return "active"
	}
	return ""
}

// hasFailedCondition reports whether K8s has marked the Job as Failed
// (typically after backoffLimit is exceeded).
func hasFailedCondition(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			return true
		}
	}
	return false
}

// reviewFromJob builds the public Review DTO from a Job. Owner/Repo/PR/Commit
// come from the labels and annotations the receiver sets when the Job is
// submitted (see job-template.yaml). Missing fields are omitted via
// `omitempty` so older Jobs without the labels still serialize cleanly.
func reviewFromJob(job *batchv1.Job) Review {
	r := Review{
		Name:      job.Name,
		Namespace: job.Namespace,
		Status:    humanStatus(job),
		Active:    job.Status.Active,
		Succeeded: job.Status.Succeeded,
		Failed:    job.Status.Failed,
	}
	if v, ok := job.Annotations["boop/owner"]; ok {
		r.Owner = v
	}
	if v, ok := job.Annotations["boop/repo"]; ok {
		r.Repo = v
	}
	if v, ok := job.Labels["pr-number"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			r.PR = n
		}
	}
	if v, ok := job.Labels["sha"]; ok {
		r.Commit = v
	}
	if v, ok := job.Annotations["boop/base-ref"]; ok {
		r.BaseRef = v
	}
	if job.Status.StartTime != nil {
		r.StartTime = job.Status.StartTime.UTC().Format(time.RFC3339)
	}
	if job.Status.CompletionTime != nil {
		r.CompletionTime = job.Status.CompletionTime.UTC().Format(time.RFC3339)
	}
	if d := jobDuration(job); d != "" {
		r.Duration = d
	}
	return r
}

// humanStatus is the label shown on the dashboard. It favors K8s'
// coarse state ("Running", "Complete", "Failed") over counts so a Job
// with Succeeded=1 and Active=1 (terminating pods) reads naturally.
func humanStatus(job *batchv1.Job) string {
	switch {
	case hasFailedCondition(job):
		return "Failed"
	case job.Status.Failed > 0:
		return "Failed"
	case job.Status.Succeeded > 0:
		return "Complete"
	case job.Status.Active > 0:
		return "Running"
	}
	return "Pending"
}

func jobDuration(job *batchv1.Job) string {
	if job.Status.StartTime == nil {
		return ""
	}
	end := time.Now()
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	}
	d := end.Sub(job.Status.StartTime.Time)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func sortReviews(rs []Review) {
	sort.Slice(rs, func(i, j int) bool {
		return rs[i].StartTime > rs[j].StartTime
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}