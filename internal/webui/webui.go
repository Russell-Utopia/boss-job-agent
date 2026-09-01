package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

//go:embed templates/*.html assets/*.css assets/*.js
var files embed.FS

var pageTemplate = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"enabledText": func(enabled bool) string {
		if enabled {
			return "已开启"
		}
		return "已关闭"
	},
	"savedTime": func(value time.Time) string {
		return value.Local().Format("2006-01-02 15:04:05")
	},
	"discoveryStatusText": func(status discovery.Status) string {
		switch status {
		case discovery.StatusPreparing:
			return "准备中"
		case discovery.StatusRunning:
			return "运行中"
		case discovery.StatusPaused:
			return "已暂停"
		case discovery.StatusCompleted:
			return "发现完成"
		case discovery.StatusFailed:
			return "失败待处理"
		case discovery.StatusEndedEarly:
			return "已提前结束"
		default:
			return "状态异常"
		}
	},
	"platformStatusText": func(status jobpool.PlatformStatus) string {
		if status == jobpool.PlatformStatusOpen {
			return "可沟通"
		}
		return "已关闭"
	},
	"assessmentStatusText": func(status jobpool.AssessmentStatus) string {
		switch status {
		case jobpool.AssessmentStatusNotQueued:
			return "尚未安排"
		case jobpool.AssessmentStatusPending:
			return "待鉴定"
		case jobpool.AssessmentStatusProcessing:
			return "鉴定中"
		case jobpool.AssessmentStatusSuitable:
			return "适合"
		case jobpool.AssessmentStatusUnsuitable:
			return "不适合"
		case jobpool.AssessmentStatusNeedsUserConfirmation:
			return "需要人工确认"
		case jobpool.AssessmentStatusFailed:
			return "鉴定失败"
		default:
			return "尚未鉴定"
		}
	},
	"outreachStatusText": func(status jobpool.OutreachStatus) string {
		switch status {
		case jobpool.OutreachStatusNotQueued:
			return "尚未安排"
		case jobpool.OutreachStatusPending:
			return "等待真实打招呼"
		case jobpool.OutreachStatusProcessing:
			return "打招呼中"
		case jobpool.OutreachStatusContacted:
			return "已打招呼"
		case jobpool.OutreachStatusPossiblyContacted:
			return "可能已打招呼"
		case jobpool.OutreachStatusFailed:
			return "打招呼失败"
		default:
			return "尚未安排"
		}
	},
	"humanReviewStatusText": func(status jobpool.HumanReviewStatus) string {
		switch status {
		case jobpool.HumanReviewStatusSuitable:
			return "已复核 · 适合"
		case jobpool.HumanReviewStatusUnsuitable:
			return "已复核 · 不适合"
		case jobpool.HumanReviewStatusStale:
			return "待重新复核"
		default:
			return "未复核"
		}
	},
	"currentJudgmentText": func(judgment jobpool.CurrentJudgment) string {
		if !judgment.Available {
			return judgment.Reason
		}
		source := "AI 鉴定"
		if judgment.Source == jobpool.JudgmentSourceHuman {
			source = "人工复核"
		}
		verdict := "不适合"
		if judgment.Verdict == jobpool.JudgmentVerdictSuitable {
			verdict = "适合"
		}
		return source + " · " + verdict
	},
	"jsonText": func(value json.RawMessage) string {
		return string(value)
	},
	"selected": func(actual any, expected string) bool {
		return fmt.Sprint(actual) == expected
	},
}).ParseFS(files, "templates/page.html"))

type handler struct {
	dependencies Dependencies
}

type Dependencies struct {
	Resume     *onlineresume.Versions
	Discovery  *discovery.Service
	Jobs       *jobpool.Pool
	Assessment *assessment.Service
	Settings   *automationsettings.Settings
	Runlog     *runlog.Log
}

type pageData struct {
	Page               string
	PageTitle          string
	State              startupState
	ResumeFeedback     *resumeFeedback
	AssessmentFeedback string
	Job                *jobpool.JobDetailView
	ReviewSaved        bool
}

type startupState struct {
	CurrentResume         *onlineresume.Version                    `json:"currentResume"`
	ActivePolicy          assessment.Policy                        `json:"activePolicy"`
	PolicyOptimization    assessment.PolicyOptimizationView        `json:"policyOptimization"`
	Automation            automationsettings.View                  `json:"automation"`
	Actions               firstUseActions                          `json:"actions"`
	RunlogHealth          runlog.Health                            `json:"runlogHealth"`
	ActiveResumeUse       *discovery.ActiveResumeUse               `json:"activeDiscoveryResumeUse,omitempty"`
	DiscoveryRun          *discovery.RunView                       `json:"discoveryRun,omitempty"`
	Jobs                  []webJobView                             `json:"jobs"`
	JobList               *jobListState                            `json:"jobList,omitempty"`
	WorkflowSummary       *workflowSummary                         `json:"workflowSummary,omitempty"`
	EligibleOutreachCount int                                      `json:"eligibleOutreachCount,omitempty"`
	OutreachPreview       automationsettings.OutreachChangeImpact  `json:"outreachPreview,omitempty"`
	OutreachForm          automationsettings.OutreachChangeImpact  `json:"outreachForm,omitempty"`
	OutreachProposal      *automationsettings.OutreachChangeImpact `json:"-"`
	OutreachSettingsNote  string                                   `json:"outreachSettingsNote,omitempty"`
}

type resumeFeedback struct {
	Message string
	Error   bool
}

type firstUseActions struct {
	StartDiscovery    actionAvailability `json:"startDiscovery"`
	QueueRealOutreach actionAvailability `json:"queueRealOutreach"`
}

type actionAvailability struct {
	Allowed bool   `json:"allowed"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type webJobView struct {
	ID                int64                     `json:"id"`
	JobTitle          string                    `json:"jobTitle"`
	CompanyName       string                    `json:"companyName"`
	City              string                    `json:"city"`
	Salary            string                    `json:"salary"`
	JDHash            string                    `json:"jdHash"`
	PlatformStatus    jobpool.PlatformStatus    `json:"platformStatus"`
	AssessmentStatus  jobpool.AssessmentStatus  `json:"assessmentStatus"`
	HumanReviewStatus jobpool.HumanReviewStatus `json:"humanReviewStatus"`
	OutreachStatus    jobpool.OutreachStatus    `json:"outreachStatus"`
	AssessmentAction  actionAvailability        `json:"assessmentAction"`
	ReviewAction      actionAvailability        `json:"reviewAction"`
	OutreachAction    actionAvailability        `json:"outreachAction"`
}

type workflowSummary struct {
	DiscoveryCompleted      int
	DiscoveryTotal          int
	DiscoveryPercent        int
	AssessmentIncompleteURL string
	AssessmentCompleted     int
	AssessmentTotal         int
	AssessmentPercent       int
	OutreachUncontactedURL  string
	OutreachContactedURL    string
	OutreachCompleted       int
	OutreachTotal           int
	OutreachPercent         int
}

type jobListQuery struct {
	Search               string
	PlatformStatus       jobpool.PlatformStatus
	AssessmentStatus     jobpool.AssessmentStatus
	AssessmentIncomplete bool
	HumanReview          jobpool.HumanReviewStatus
	OutreachStatus       jobpool.OutreachStatus
	OutreachUncontacted  bool
	OutreachContacted    bool
	Page                 int
	PageSize             int
}

type jobListState struct {
	Search               string
	PlatformStatus       jobpool.PlatformStatus
	AssessmentStatus     jobpool.AssessmentStatus
	AssessmentIncomplete bool
	HumanReview          jobpool.HumanReviewStatus
	OutreachStatus       jobpool.OutreachStatus
	OutreachUncontacted  bool
	OutreachContacted    bool
	Page                 int
	PageSize             int
	Total                int
	TotalPages           int
	HasPrevious          bool
	HasNext              bool
	PreviousURL          string
	NextURL              string
	Pages                []jobPageLink
	PageSizes            []jobPageSize
}

type jobPageLink struct {
	Number   int
	URL      string
	Selected bool
}

type jobPageSize struct {
	Size     int
	URL      string
	Selected bool
}

func (q jobListQuery) values(page int, pageSize int) url.Values {
	values := url.Values{}
	if q.Search != "" {
		values.Set("q", q.Search)
	}
	if q.PlatformStatus != "" {
		values.Set("platformStatus", string(q.PlatformStatus))
	}
	if q.AssessmentStatus != "" {
		values.Set("assessmentStatus", string(q.AssessmentStatus))
	}
	if q.HumanReview != "" {
		values.Set("humanReview", string(q.HumanReview))
	}
	if q.AssessmentIncomplete {
		values.Set("assessmentIncomplete", "1")
	}
	if q.OutreachStatus != "" {
		values.Set("outreachStatus", string(q.OutreachStatus))
	}
	if q.OutreachUncontacted {
		values.Set("outreachUncontacted", "1")
	}
	if q.OutreachContacted {
		values.Set("outreachContacted", "1")
	}
	values.Set("page", strconv.Itoa(page))
	values.Set("pageSize", strconv.Itoa(pageSize))
	return values
}

func toWebJobViews(jobs []jobpool.JobView, greetingConfigured bool) []webJobView {
	views := make([]webJobView, 0, len(jobs))
	for _, job := range jobs {
		view := webJobView{
			ID:                job.ID,
			JobTitle:          job.JobTitle,
			CompanyName:       job.CompanyName,
			City:              job.City,
			Salary:            job.Salary,
			JDHash:            job.JDHash,
			PlatformStatus:    job.PlatformStatus,
			AssessmentStatus:  job.AssessmentStatus,
			HumanReviewStatus: job.HumanReviewStatus,
			OutreachStatus:    job.OutreachStatus,
			AssessmentAction:  toActionAvailability(job.AssessmentAction),
			ReviewAction:      toActionAvailability(job.ReviewAction),
			OutreachAction:    toActionAvailability(job.OutreachAction),
		}
		if !greetingConfigured && view.OutreachAction.Allowed {
			view.OutreachAction = actionAvailability{
				Code:   "outreach_greeting_required",
				Reason: "请先配置固定招呼语，再真实打招呼",
			}
		}
		views = append(views, view)
	}
	return views
}

func toActionAvailability(value jobpool.ActionAvailability) actionAvailability {
	return actionAvailability{Allowed: value.Allowed, Code: value.Code, Reason: value.Reason}
}

type stateQuery func(context.Context) (startupState, error)

func New(dependencies Dependencies) http.Handler {
	h := &handler{dependencies: dependencies}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /jobs", h.jobsPage)
	mux.HandleFunc("GET /jobs/{jobID}", h.jobDetailPage)
	mux.HandleFunc("POST /jobs/{jobID}/review", h.reviewJobPage)
	mux.HandleFunc("POST /jobs/{jobID}/assessment", h.assessmentCommandPage(h.dependencies.Jobs.QueueAssessments, "queued"))
	mux.HandleFunc("POST /jobs/{jobID}/assessment/retry", h.assessmentCommandPage(h.dependencies.Jobs.RetryAssessmentFailures, "retried"))
	mux.HandleFunc("GET /assessments", h.renderPage("assessments", "岗位鉴定", h.assessmentsState))
	mux.HandleFunc("POST /assessments/settings", h.configureAssessmentPage)
	mux.HandleFunc("GET /outreach", h.renderPage("outreach", "打招呼", h.outreachState))
	mux.HandleFunc("POST /outreach/settings", h.configureOutreachPage)
	mux.HandleFunc("POST /outreach/real", h.queueRealPage)
	mux.HandleFunc("GET /resume", h.renderPage("resume", "在线简历", h.resumeState))
	mux.HandleFunc("POST /resume/refresh", h.refreshResumePage)
	mux.HandleFunc("POST /discovery-runs", h.startDiscoveryPage)
	mux.HandleFunc("POST /discovery-runs/{runID}/pause", h.discoveryCommandPage(h.dependencies.Discovery.Pause))
	mux.HandleFunc("POST /discovery-runs/{runID}/continue", h.discoveryCommandPage(h.dependencies.Discovery.Continue))
	mux.HandleFunc("POST /discovery-runs/{runID}/end-early", h.discoveryCommandPage(h.dependencies.Discovery.EndEarly))
	mux.HandleFunc("GET /assets/app.css", serveCSS)
	mux.HandleFunc("GET /assets/app.js", serveJS)
	mux.HandleFunc("GET /api/startup-state", h.startupState)
	mux.HandleFunc("GET /api/runlog/health", h.runlogHealth)
	mux.HandleFunc("POST /api/runlog/recheck", h.recheckRunlogAPI)
	mux.HandleFunc("POST /runlog/recheck", h.recheckRunlogPage)
	mux.HandleFunc("POST /api/discovery-runs", h.startDiscovery)
	mux.HandleFunc("POST /api/discovery-runs/{runID}/pause", h.discoveryCommandAPI(h.dependencies.Discovery.Pause))
	mux.HandleFunc("POST /api/discovery-runs/{runID}/continue", h.discoveryCommandAPI(h.dependencies.Discovery.Continue))
	mux.HandleFunc("POST /api/discovery-runs/{runID}/end-early", h.discoveryCommandAPI(h.dependencies.Discovery.EndEarly))
	mux.HandleFunc("POST /api/assessments", h.assessmentCommandAPI(h.dependencies.Jobs.QueueAssessments))
	mux.HandleFunc("POST /api/assessments/retry", h.assessmentCommandAPI(h.dependencies.Jobs.RetryAssessmentFailures))
	mux.HandleFunc("POST /api/jobs/batch", h.batchJobsAPI)
	mux.HandleFunc("POST /api/outreach/real", h.queueReal)
	mux.HandleFunc("POST /api/outreach/settings", h.configureOutreach)
	mux.HandleFunc("POST /api/policy/draft", h.generatePolicyDraft)
	mux.HandleFunc("POST /api/policy/validate", h.validatePolicyDraft)
	mux.HandleFunc("POST /api/policy/adopt", h.adoptPolicy)
	return mux
}

func (h *handler) jobDetailPage(w http.ResponseWriter, r *http.Request) {
	jobID, ok := platformJobID(w, r)
	if !ok {
		return
	}
	detail, err := h.dependencies.Jobs.GetJobDetail(r.Context(), jobID)
	var rejection businessRejection
	if errors.As(err, &rejection) && rejection.RejectionCode() == "platform_job_not_found" {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "无法读取岗位详情", http.StatusInternalServerError)
		return
	}
	feedback := ""
	switch r.URL.Query().Get("assessment") {
	case "queued":
		feedback = "已加入 AI 鉴定队列。"
	case "retried":
		feedback = "已重新加入 AI 鉴定队列。"
	}
	h.executePage(w, http.StatusOK, pageData{
		Page: "job-detail", PageTitle: detail.JobTitle,
		State:              startupState{RunlogHealth: h.dependencies.Runlog.Health()},
		AssessmentFeedback: feedback, Job: &detail,
		ReviewSaved: r.URL.Query().Get("reviewed") == "1",
	})
}

type assessmentBatchCommand func(context.Context, []int64) (jobpool.BatchActionResult, error)

func (h *handler) assessmentCommandPage(command assessmentBatchCommand, feedback string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID, ok := platformJobID(w, r)
		if !ok {
			return
		}
		result, err := command(r.Context(), []int64{jobID})
		if err != nil {
			h.writePageCommandError(w, err)
			return
		}
		if result.Succeeded != 1 {
			if len(result.Skipped) > 0 {
				http.Error(w, result.Skipped[0].Reason, http.StatusConflict)
				return
			}
			http.Error(w, "AI 鉴定操作未生效，请刷新后重试", http.StatusConflict)
			return
		}
		//nolint:gosec // jobID is a validated positive integer and feedback is a fixed handler value.
		http.Redirect(w, r, fmt.Sprintf("/jobs/%d?assessment=%s", jobID, feedback), http.StatusSeeOther)
	}
}

func (h *handler) writePageCommandError(w http.ResponseWriter, err error) {
	var rejection businessRejection
	if errors.As(err, &rejection) {
		http.Error(w, rejection.RejectionReason(), http.StatusConflict)
		return
	}
	http.Error(w, "AI 鉴定操作失败，请稍后重试", http.StatusInternalServerError)
}

func (h *handler) reviewJobPage(w http.ResponseWriter, r *http.Request) {
	jobID, ok := platformJobID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "人工复核内容无效", http.StatusBadRequest)
		return
	}
	verdict := jobpool.HumanVerdict(r.PostFormValue("verdict"))
	if verdict != jobpool.HumanVerdictSuitable && verdict != jobpool.HumanVerdictUnsuitable {
		http.Error(w, "请选择适合或不适合", http.StatusBadRequest)
		return
	}
	err := h.dependencies.Jobs.Review(r.Context(), []jobpool.ReviewDecision{{
		JobID:          jobID,
		ExpectedJDHash: r.PostFormValue("jdHash"),
		Verdict:        verdict,
		Note:           r.PostFormValue("note"),
	}})
	if err == nil {
		//nolint:gosec // jobID is a validated positive integer and cannot supply a redirect host or path.
		http.Redirect(w, r, fmt.Sprintf("/jobs/%d?reviewed=1", jobID), http.StatusSeeOther)
		return
	}
	var rejection businessRejection
	if errors.As(err, &rejection) {
		http.Error(w, rejection.RejectionReason(), http.StatusConflict)
		return
	}
	http.Error(w, "保存人工复核失败，请稍后重试", http.StatusInternalServerError)
}

func platformJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	jobID, err := strconv.ParseInt(r.PathValue("jobID"), 10, 64)
	if err != nil || jobID <= 0 {
		http.Error(w, "岗位编号无效", http.StatusBadRequest)
		return 0, false
	}
	return jobID, true
}

func (h *handler) jobsPage(w http.ResponseWriter, r *http.Request) {
	state, err := h.jobsStateForQuery(r.Context(), parseJobListQuery(r.URL.Query()))
	if err != nil {
		http.Error(w, "无法读取当前业务状态", http.StatusInternalServerError)
		return
	}
	h.executePage(w, http.StatusOK, pageData{Page: "jobs", PageTitle: "岗位工作台", State: state})
}

func (h *handler) jobsStateForQuery(ctx context.Context, query jobListQuery) (startupState, error) {
	resume, err := h.dependencies.Resume.GetCurrent(ctx)
	if err != nil {
		return startupState{}, err
	}
	availability, err := h.dependencies.Discovery.StartAvailability(ctx)
	if err != nil {
		return startupState{}, err
	}
	run, err := h.dependencies.Discovery.GetLatestRun(ctx)
	if err != nil {
		return startupState{}, err
	}
	jobs, err := h.dependencies.Jobs.ListJobs(ctx)
	if err != nil {
		return startupState{}, err
	}
	settings, err := h.dependencies.Settings.GetDiscoveryHints(ctx)
	if err != nil {
		return startupState{}, err
	}
	views := toWebJobViews(jobs, settings.OutreachGreeting != nil)
	filtered := filterJobViews(views, query)
	list, pageJobs := buildJobListState(query, filtered)
	summary := summarizeWorkflow(run, views, query)
	return startupState{
		CurrentResume:   resume,
		DiscoveryRun:    run,
		Jobs:            pageJobs,
		JobList:         list,
		WorkflowSummary: summary,
		Automation:      settings,
		Actions: firstUseActions{
			StartDiscovery: fromDiscoveryAvailability(availability),
		},
		RunlogHealth: h.dependencies.Runlog.Health(),
	}, nil
}

func defaultJobListQuery() jobListQuery {
	return jobListQuery{Page: 1, PageSize: 10}
}

func parseJobListQuery(values url.Values) jobListQuery {
	query := defaultJobListQuery()
	query.Search = strings.TrimSpace(values.Get("q"))
	if query.Search == "" {
		query.Search = strings.TrimSpace(values.Get("search"))
	}
	query.PlatformStatus = parsePlatformStatus(values.Get("platformStatus"))
	query.AssessmentStatus = parseAssessmentStatus(values.Get("assessmentStatus"))
	query.AssessmentIncomplete = values.Get("assessmentIncomplete") == "1"
	query.HumanReview = parseHumanReviewStatus(values.Get("humanReview"))
	query.OutreachStatus = parseOutreachStatus(values.Get("outreachStatus"))
	query.OutreachUncontacted = values.Get("outreachUncontacted") == "1"
	query.OutreachContacted = values.Get("outreachContacted") == "1"
	if page, err := strconv.Atoi(values.Get("page")); err == nil && page > 0 {
		query.Page = page
	}
	if pageSize, err := strconv.Atoi(values.Get("pageSize")); err == nil && validJobPageSize(pageSize) {
		query.PageSize = pageSize
	}
	return query
}

func parsePlatformStatus(value string) jobpool.PlatformStatus {
	status := jobpool.PlatformStatus(value)
	if status == jobpool.PlatformStatusOpen || status == jobpool.PlatformStatusClosed {
		return status
	}
	return ""
}

func parseAssessmentStatus(value string) jobpool.AssessmentStatus {
	status := jobpool.AssessmentStatus(value)
	switch status {
	case jobpool.AssessmentStatusNotQueued, jobpool.AssessmentStatusPending,
		jobpool.AssessmentStatusProcessing, jobpool.AssessmentStatusSuitable,
		jobpool.AssessmentStatusUnsuitable, jobpool.AssessmentStatusNeedsUserConfirmation,
		jobpool.AssessmentStatusFailed:
		return status
	default:
		return ""
	}
}

func parseHumanReviewStatus(value string) jobpool.HumanReviewStatus {
	status := jobpool.HumanReviewStatus(value)
	switch status {
	case jobpool.HumanReviewStatusUnreviewed, jobpool.HumanReviewStatusSuitable,
		jobpool.HumanReviewStatusUnsuitable, jobpool.HumanReviewStatusStale:
		return status
	default:
		return ""
	}
}

func parseOutreachStatus(value string) jobpool.OutreachStatus {
	status := jobpool.OutreachStatus(value)
	switch status {
	case jobpool.OutreachStatusNotQueued, jobpool.OutreachStatusPending,
		jobpool.OutreachStatusProcessing, jobpool.OutreachStatusContacted,
		jobpool.OutreachStatusPossiblyContacted, jobpool.OutreachStatusFailed:
		return status
	default:
		return ""
	}
}

func validJobPageSize(value int) bool {
	switch value {
	case 10, 20, 50, 100:
		return true
	default:
		return false
	}
}

func filterJobViews(jobs []webJobView, query jobListQuery) []webJobView {
	filtered := make([]webJobView, 0, len(jobs))
	for _, job := range jobs {
		if matchesJobFilter(job, query) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func matchesJobFilter(job webJobView, query jobListQuery) bool {
	return matchesJobSearch(job, query.Search) &&
		matchesPlatformStatus(job, query.PlatformStatus) &&
		matchesAssessmentStatus(job, query.AssessmentStatus) &&
		matchesAssessmentIncomplete(job, query.AssessmentIncomplete) &&
		matchesHumanReviewStatus(job, query.HumanReview) &&
		matchesOutreachStatus(job, query.OutreachStatus) &&
		matchesOutreachScope(job, query.OutreachUncontacted, query.OutreachContacted)
}

func matchesJobSearch(job webJobView, search string) bool {
	needle := strings.ToLower(search)
	return needle == "" || strings.Contains(strings.ToLower(job.JobTitle), needle) ||
		strings.Contains(strings.ToLower(job.CompanyName), needle)
}

func matchesPlatformStatus(job webJobView, expected jobpool.PlatformStatus) bool {
	return expected == "" || expected == job.PlatformStatus
}

func matchesAssessmentStatus(job webJobView, expected jobpool.AssessmentStatus) bool {
	return expected == "" || expected == job.AssessmentStatus
}

func matchesAssessmentIncomplete(job webJobView, incomplete bool) bool {
	if !incomplete {
		return true
	}
	switch job.AssessmentStatus {
	case jobpool.AssessmentStatusSuitable, jobpool.AssessmentStatusUnsuitable, jobpool.AssessmentStatusNeedsUserConfirmation:
		return false
	default:
		return true
	}
}

func matchesHumanReviewStatus(job webJobView, expected jobpool.HumanReviewStatus) bool {
	return expected == "" || expected == job.HumanReviewStatus
}

func matchesOutreachStatus(job webJobView, expected jobpool.OutreachStatus) bool {
	return expected == "" || expected == job.OutreachStatus
}

func matchesOutreachScope(job webJobView, uncontacted, contacted bool) bool {
	if uncontacted && job.OutreachStatus == jobpool.OutreachStatusContacted {
		return false
	}
	if contacted && job.OutreachStatus != jobpool.OutreachStatusContacted {
		return false
	}
	return true
}

func buildJobListState(query jobListQuery, jobs []webJobView) (*jobListState, []webJobView) {
	totalPages := (len(jobs) + query.PageSize - 1) / query.PageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if query.Page > totalPages {
		query.Page = totalPages
	}
	start := (query.Page - 1) * query.PageSize
	end := start + query.PageSize
	if start > len(jobs) {
		start = len(jobs)
	}
	if end > len(jobs) {
		end = len(jobs)
	}
	pageJobs := jobs[start:end]
	list := &jobListState{
		Search: query.Search, PlatformStatus: query.PlatformStatus,
		AssessmentStatus: query.AssessmentStatus, AssessmentIncomplete: query.AssessmentIncomplete,
		HumanReview: query.HumanReview, OutreachStatus: query.OutreachStatus,
		OutreachUncontacted: query.OutreachUncontacted, OutreachContacted: query.OutreachContacted,
		Page: query.Page, PageSize: query.PageSize, Total: len(jobs), TotalPages: totalPages,
		HasPrevious: query.Page > 1, HasNext: query.Page < totalPages,
	}
	if list.HasPrevious {
		list.PreviousURL = jobListURL(query, query.Page-1, query.PageSize)
	}
	if list.HasNext {
		list.NextURL = jobListURL(query, query.Page+1, query.PageSize)
	}
	for page := 1; page <= totalPages; page++ {
		list.Pages = append(list.Pages, jobPageLink{
			Number: page, URL: jobListURL(query, page, query.PageSize), Selected: page == query.Page,
		})
	}
	for _, pageSize := range []int{10, 20, 50, 100} {
		list.PageSizes = append(list.PageSizes, jobPageSize{
			Size: pageSize, URL: jobListURL(query, 1, pageSize), Selected: pageSize == query.PageSize,
		})
	}
	return list, pageJobs
}

func jobListURL(query jobListQuery, page, pageSize int) string {
	return "/jobs?" + query.values(page, pageSize).Encode()
}

func summarizeWorkflow(run *discovery.RunView, jobs []webJobView, query jobListQuery) *workflowSummary {
	summary := &workflowSummary{AssessmentTotal: len(jobs), OutreachTotal: len(jobs)}
	if run != nil {
		summary.DiscoveryCompleted = run.CompletedRanges
		summary.DiscoveryTotal = run.TotalRanges
		summary.DiscoveryPercent = progressPercent(summary.DiscoveryCompleted, summary.DiscoveryTotal)
	}
	for _, job := range jobs {
		switch job.AssessmentStatus {
		case jobpool.AssessmentStatusSuitable, jobpool.AssessmentStatusUnsuitable, jobpool.AssessmentStatusNeedsUserConfirmation:
			summary.AssessmentCompleted++
		}
		if job.OutreachStatus == jobpool.OutreachStatusContacted {
			summary.OutreachCompleted++
		}
	}
	summary.AssessmentPercent = progressPercent(summary.AssessmentCompleted, summary.AssessmentTotal)
	summary.OutreachPercent = progressPercent(summary.OutreachCompleted, summary.OutreachTotal)
	pageSize := query.PageSize
	if !validJobPageSize(pageSize) {
		pageSize = defaultJobListQuery().PageSize
	}
	incomplete := query
	incomplete.AssessmentStatus = ""
	incomplete.AssessmentIncomplete = true
	incomplete.OutreachUncontacted = false
	incomplete.OutreachContacted = false
	summary.AssessmentIncompleteURL = jobListURL(incomplete, 1, pageSize)
	uncontacted := query
	uncontacted.AssessmentIncomplete = false
	uncontacted.OutreachStatus = ""
	uncontacted.OutreachUncontacted = true
	uncontacted.OutreachContacted = false
	summary.OutreachUncontactedURL = jobListURL(uncontacted, 1, pageSize)
	contacted := query
	contacted.AssessmentIncomplete = false
	contacted.OutreachStatus = ""
	contacted.OutreachUncontacted = false
	contacted.OutreachContacted = true
	summary.OutreachContactedURL = jobListURL(contacted, 1, pageSize)
	return summary
}

func progressPercent(completed, total int) int {
	if total <= 0 || completed <= 0 {
		return 0
	}
	if completed >= total {
		return 100
	}
	return completed * 100 / total
}

func (h *handler) assessmentsState(ctx context.Context) (startupState, error) {
	optimization, err := h.dependencies.Assessment.GetPolicyOptimization(ctx)
	if err != nil {
		return startupState{}, err
	}
	settings, err := h.dependencies.Settings.Get(ctx)
	if err != nil {
		return startupState{}, err
	}
	return startupState{
		ActivePolicy: optimization.ActivePolicy, PolicyOptimization: optimization,
		Automation:   settings,
		RunlogHealth: h.dependencies.Runlog.Health(),
	}, nil
}

func (h *handler) generatePolicyDraft(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs            []int64 `json:"jobIds"`
		ValidationEnabled bool    `json:"validationEnabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	draft, err := h.dependencies.Assessment.GeneratePolicyDraft(r.Context(), request.JobIDs)
	if err != nil {
		h.writePolicyError(w, err)
		return
	}
	draft.ValidationEnabled = request.ValidationEnabled
	writeJSON(w, http.StatusOK, draft)
}

func (h *handler) validatePolicyDraft(w http.ResponseWriter, r *http.Request) {
	var draft assessment.PolicyDraft
	if !decodeJSON(w, r, &draft) {
		return
	}
	report, err := h.dependencies.Assessment.ValidatePolicyDraft(r.Context(), draft)
	if err != nil {
		h.writePolicyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *handler) adoptPolicy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Text            string `json:"text"`
		ChangeNote      string `json:"changeNote"`
		PolicyVersionID int64  `json:"policyVersionId"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	rules, err := assessment.ParsePolicyRules(request.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "policy_rules_required", "reason": "策略必须包含至少一条完整规则"})
		return
	}
	versionID, err := h.dependencies.Assessment.CreatePolicyVersionIfCurrent(r.Context(), rules, request.ChangeNote, request.PolicyVersionID)
	if err != nil {
		h.writePolicyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"policyVersionId": versionID})
}

func (h *handler) writePolicyError(w http.ResponseWriter, err error) {
	var rejection businessRejection
	if errors.As(err, &rejection) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code": rejection.RejectionCode(), "reason": rejection.RejectionReason(),
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "策略操作失败，请稍后重试"})
}

func (h *handler) configureAssessmentPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "鉴定设置内容无效", http.StatusBadRequest)
		return
	}
	processingLimit, err := strconv.Atoi(r.PostFormValue("assessmentProcessingLimit"))
	if err != nil {
		http.Error(w, "AI 同时鉴定数格式无效", http.StatusBadRequest)
		return
	}
	enabled := r.PostFormValue("automaticAssessmentEnabled") == "on"
	if err := h.dependencies.Settings.ConfigureAssessment(r.Context(), enabled, processingLimit); err != nil {
		var rejection businessRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.RejectionReason(), http.StatusBadRequest)
			return
		}
		http.Error(w, "保存鉴定设置失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/assessments", http.StatusSeeOther)
}

type outreachSettingsRequest struct {
	AutomaticOutreachEnabled bool                                            `json:"automaticOutreachEnabled"`
	GreetingText             string                                          `json:"greetingText"`
	TimeWindows              []automationsettings.OutreachTimeWindow         `json:"timeWindows"`
	Confirmation             automationsettings.OutreachSettingsConfirmation `json:"confirmation"`
}

func (h *handler) previewOutreachSettings(ctx context.Context, request outreachSettingsRequest) (automationsettings.OutreachChangeImpact, error) {
	return h.dependencies.Settings.PreviewOutreachConfiguration(
		ctx, request.AutomaticOutreachEnabled, request.GreetingText, request.TimeWindows,
	)
}

func (h *handler) saveOutreachSettings(ctx context.Context, request outreachSettingsRequest) error {
	return h.dependencies.Settings.ConfigureOutreachWithConfirmation(
		ctx, request.AutomaticOutreachEnabled, request.GreetingText, request.TimeWindows, request.Confirmation,
	)
}

func (h *handler) outreachEnableRequiresConfirmation(ctx context.Context, enabled bool) (bool, error) {
	if !enabled {
		return false, nil
	}
	settings, err := h.dependencies.Settings.Get(ctx)
	if err != nil {
		return false, err
	}
	return !settings.AutomaticOutreachEnabled, nil
}

func (h *handler) configureOutreach(w http.ResponseWriter, r *http.Request) {
	var request outreachSettingsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	impact, err := h.previewOutreachSettings(r.Context(), request)
	if err != nil {
		h.writeCommandResult(w, err)
		return
	}
	requiresConfirmation, err := h.outreachEnableRequiresConfirmation(r.Context(), request.AutomaticOutreachEnabled)
	if err != nil {
		h.writeCommandResult(w, err)
		return
	}
	if requiresConfirmation && !request.Confirmation.Confirmed {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    "outreach_confirmation_required",
			"reason":  "开启自动打招呼前必须确认当前可入队岗位、完整招呼语和时间规则",
			"preview": impact,
		})
		return
	}
	if err := h.saveOutreachSettings(r.Context(), request); err != nil {
		h.writeCommandResult(w, err)
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (h *handler) configureOutreachPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "自动打招呼设置内容无效", http.StatusBadRequest)
		return
	}
	request, err := parseOutreachSettingsForm(r)
	if err != nil {
		http.Error(w, "自动打招呼设置内容无效", http.StatusBadRequest)
		return
	}
	impact, err := h.previewOutreachSettings(r.Context(), request)
	if err != nil {
		var rejection businessRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.RejectionReason(), http.StatusBadRequest)
			return
		}
		http.Error(w, "读取自动打招呼影响失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	requiresConfirmation, err := h.outreachEnableRequiresConfirmation(r.Context(), request.AutomaticOutreachEnabled)
	if err != nil {
		http.Error(w, "读取自动打招呼设置失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	if requiresConfirmation && r.PostFormValue("confirmed") != "true" {
		h.renderOutreachEnableProposal(w, r, impact)
		return
	}

	request.Confirmation = automationsettings.OutreachSettingsConfirmation{
		EligibleJobCount: parseFormInt(r.PostFormValue("confirmedEligibleJobCount")),
		GreetingText:     r.PostFormValue("confirmedGreetingText"),
		TimeDescription:  r.PostFormValue("confirmedTimeDescription"),
		Confirmed:        r.PostFormValue("confirmed") == "true",
	}
	if err := h.saveOutreachSettings(r.Context(), request); err != nil {
		var rejection businessRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.RejectionReason(), http.StatusConflict)
			return
		}
		http.Error(w, "保存自动打招呼设置失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/outreach", http.StatusSeeOther)
}

func (h *handler) renderOutreachEnableProposal(w http.ResponseWriter, r *http.Request, impact automationsettings.OutreachChangeImpact) {
	state, err := h.outreachState(r.Context())
	if err != nil {
		http.Error(w, "无法读取当前业务状态", http.StatusInternalServerError)
		return
	}
	impact.AutomaticOutreachEnabled = true
	state.OutreachPreview = impact
	state.OutreachForm = impact
	state.OutreachProposal = &impact
	state.OutreachSettingsNote = "请确认开启自动打招呼：确认只授权之后新符合条件的岗位，不会停止后台处理。"
	h.executePage(w, http.StatusOK, pageData{Page: "outreach", PageTitle: "打招呼", State: state})
}

func parseOutreachSettingsForm(r *http.Request) (outreachSettingsRequest, error) {
	starts := r.PostForm["outreachTimeWindowStart"]
	ends := r.PostForm["outreachTimeWindowEnd"]
	if len(starts) != len(ends) {
		return outreachSettingsRequest{}, errors.New("outreach time window start and end counts differ")
	}
	windows := make([]automationsettings.OutreachTimeWindow, 0, len(starts))
	for index := range starts {
		if strings.TrimSpace(starts[index]) == "" && strings.TrimSpace(ends[index]) == "" {
			continue
		}
		windows = append(windows, automationsettings.OutreachTimeWindow{Start: starts[index], End: ends[index]})
	}
	if preset := r.PostFormValue("outreachWindowPreset"); preset != "" {
		window, ok := outreachWindowPreset(preset)
		if !ok {
			return outreachSettingsRequest{}, errors.New("unknown outreach time window preset")
		}
		windows = append(windows, window)
	}
	if r.PostFormValue("outreachWindowCustom") == "true" {
		windows = append(windows, automationsettings.OutreachTimeWindow{
			Start: r.PostFormValue("outreachWindowCustomStart"),
			End:   r.PostFormValue("outreachWindowCustomEnd"),
		})
	}
	return outreachSettingsRequest{
		AutomaticOutreachEnabled: r.PostFormValue("automaticOutreachEnabled") == "on",
		GreetingText:             r.PostFormValue("outreachGreetingText"),
		TimeWindows:              windows,
	}, nil
}

func outreachWindowPreset(name string) (automationsettings.OutreachTimeWindow, bool) {
	switch name {
	case "morning":
		return automationsettings.OutreachTimeWindow{Start: "09:00", End: "12:00"}, true
	case "afternoon":
		return automationsettings.OutreachTimeWindow{Start: "14:00", End: "18:00"}, true
	case "evening":
		return automationsettings.OutreachTimeWindow{Start: "19:00", End: "21:00"}, true
	default:
		return automationsettings.OutreachTimeWindow{}, false
	}
}

func parseFormInt(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func (h *handler) outreachState(ctx context.Context) (startupState, error) {
	settings, err := h.dependencies.Settings.Get(ctx)
	if err != nil {
		return startupState{}, err
	}
	availability, err := h.dependencies.Settings.QueueRealOutreachAvailability(ctx)
	if err != nil {
		return startupState{}, err
	}
	jobs, err := h.dependencies.Jobs.ListJobs(ctx)
	if err != nil {
		return startupState{}, err
	}
	preview, err := h.dependencies.Settings.PreviewOutreachChange(ctx, false)
	if err != nil {
		return startupState{}, err
	}
	form := preview
	form.OutreachTimeWindows = append([]automationsettings.OutreachTimeWindow(nil), preview.OutreachTimeWindows...)
	for len(form.OutreachTimeWindows) < 3 {
		form.OutreachTimeWindows = append(form.OutreachTimeWindows, automationsettings.OutreachTimeWindow{})
	}
	form.AutomaticOutreachEnabled = settings.AutomaticOutreachEnabled
	form.GreetingText = ""
	if settings.OutreachGreeting != nil {
		form.GreetingText = *settings.OutreachGreeting
	}
	return startupState{
		Automation: settings, Jobs: toWebJobViews(jobs, settings.OutreachGreeting != nil), EligibleOutreachCount: preview.EligibleJobCount,
		OutreachPreview: preview, OutreachForm: form,
		OutreachSettingsNote: "自动打招呼开关只控制之后新符合条件的岗位入队；关闭后，已经安排的岗位仍会继续。",
		Actions: firstUseActions{
			QueueRealOutreach: fromSettingsAvailability(availability),
		},
		RunlogHealth: h.dependencies.Runlog.Health(),
	}, nil
}

func (h *handler) resumeState(ctx context.Context) (startupState, error) {
	resume, err := h.dependencies.Resume.GetCurrent(ctx)
	if err != nil {
		return startupState{}, err
	}
	activeUse, err := h.dependencies.Discovery.GetActiveResumeUse(ctx)
	if err != nil {
		return startupState{}, err
	}
	return startupState{
		CurrentResume:   resume,
		RunlogHealth:    h.dependencies.Runlog.Health(),
		ActiveResumeUse: activeUse,
	}, nil
}

func (h *handler) getStartupState(ctx context.Context) (startupState, error) {
	resume, err := h.dependencies.Resume.GetCurrent(ctx)
	if err != nil {
		return startupState{}, err
	}
	policy, err := h.dependencies.Assessment.GetActivePolicy(ctx)
	if err != nil {
		return startupState{}, err
	}
	settings, err := h.dependencies.Settings.Get(ctx)
	if err != nil {
		return startupState{}, err
	}
	discoveryAvailability, err := h.dependencies.Discovery.StartAvailability(ctx)
	if err != nil {
		return startupState{}, err
	}
	outreachAvailability, err := h.dependencies.Settings.QueueRealOutreachAvailability(ctx)
	if err != nil {
		return startupState{}, err
	}
	activeUse, err := h.dependencies.Discovery.GetActiveResumeUse(ctx)
	if err != nil {
		return startupState{}, err
	}
	return startupState{
		CurrentResume: resume,
		ActivePolicy:  policy,
		Automation:    settings,
		Actions: firstUseActions{
			StartDiscovery:    fromDiscoveryAvailability(discoveryAvailability),
			QueueRealOutreach: fromSettingsAvailability(outreachAvailability),
		},
		RunlogHealth:    h.dependencies.Runlog.Health(),
		ActiveResumeUse: activeUse,
	}, nil
}

func fromDiscoveryAvailability(value discovery.ActionAvailability) actionAvailability {
	return actionAvailability{Allowed: value.Allowed, Code: value.Code, Reason: value.Reason}
}

func fromSettingsAvailability(value automationsettings.ActionAvailability) actionAvailability {
	return actionAvailability{Allowed: value.Allowed, Code: value.Code, Reason: value.Reason}
}

func (h *handler) renderPage(page, title string, query stateQuery) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := query(r.Context())
		if err != nil {
			http.Error(w, "无法读取当前业务状态", http.StatusInternalServerError)
			return
		}
		h.executePage(w, http.StatusOK, pageData{
			Page:      page,
			PageTitle: title,
			State:     state,
		})
	}
}

func (h *handler) executePage(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pageTemplate.ExecuteTemplate(w, "page.html", data)
}

func (h *handler) refreshResumePage(w http.ResponseWriter, r *http.Request) {
	result, refreshErr := h.dependencies.Resume.RefreshFromBoss(r.Context())
	state, stateErr := h.resumeState(r.Context())
	if stateErr != nil {
		http.Error(w, "无法读取当前业务状态", http.StatusInternalServerError)
		return
	}
	data := pageData{Page: "resume", PageTitle: "在线简历", State: state}
	if refreshErr == nil {
		message := "内容未变化，继续使用在线简历 v" + fmt.Sprint(result.Current.Version)
		if result.Status == onlineresume.RefreshCreated {
			message = "已保存在线简历 v" + fmt.Sprint(result.Current.Version)
		}
		data.ResumeFeedback = &resumeFeedback{Message: message}
		h.executePage(w, http.StatusOK, data)
		return
	}
	var rejection businessRejection
	if errors.As(refreshErr, &rejection) {
		data.ResumeFeedback = &resumeFeedback{Message: rejection.RejectionReason(), Error: true}
		h.executePage(w, http.StatusConflict, data)
		return
	}
	data.ResumeFeedback = &resumeFeedback{Message: "刷新在线简历失败，请稍后重试", Error: true}
	h.executePage(w, http.StatusInternalServerError, data)
}

func serveCSS(w http.ResponseWriter, _ *http.Request) {
	content, err := files.ReadFile("assets/app.css")
	if err != nil {
		http.Error(w, "无法读取样式", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(content)
}

func serveJS(w http.ResponseWriter, _ *http.Request) {
	content, err := files.ReadFile("assets/app.js")
	if err != nil {
		http.Error(w, "无法读取页面脚本", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(content)
}

func (h *handler) startupState(w http.ResponseWriter, r *http.Request) {
	state, err := h.getStartupState(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "无法读取当前业务状态"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *handler) runlogHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.dependencies.Runlog.Health())
}

func (h *handler) recheckRunlogAPI(w http.ResponseWriter, r *http.Request) {
	var decision runlog.RepairDecision
	if !decodeJSON(w, r, &decision) {
		return
	}
	writeJSON(w, http.StatusOK, h.dependencies.Runlog.Recheck(r.Context(), decision))
}

func (h *handler) recheckRunlogPage(w http.ResponseWriter, r *http.Request) {
	decision := runlog.RepairDecision{
		ConfirmQuarantine: r.URL.Query().Get("confirm-quarantine") == "true",
	}
	h.dependencies.Runlog.Recheck(r.Context(), decision)
	http.Redirect(w, r, runlogReturnPath(r.URL.Query().Get("return")), http.StatusSeeOther)
}

func runlogReturnPath(page string) string {
	switch page {
	case "assessments":
		return "/assessments"
	case "outreach":
		return "/outreach"
	case "resume":
		return "/resume"
	default:
		return "/jobs"
	}
}

func (h *handler) startDiscovery(w http.ResponseWriter, r *http.Request) {
	runID, err := h.dependencies.Discovery.Start(r.Context())
	if err != nil {
		h.writeCommandResult(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"discoveryRunId": runID})
}

func (h *handler) startDiscoveryPage(w http.ResponseWriter, r *http.Request) {
	_, err := h.dependencies.Discovery.Start(r.Context())
	if err == nil {
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
		return
	}
	var rejection businessRejection
	if errors.As(err, &rejection) {
		http.Error(w, rejection.RejectionReason(), http.StatusConflict)
		return
	}
	http.Error(w, "岗位发现失败，请查看运行状态", http.StatusInternalServerError)
}

type discoveryCommand func(context.Context, int64) error

func (h *handler) discoveryCommandAPI(command discoveryCommand) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID, ok := discoveryRunID(w, r)
		if !ok {
			return
		}
		h.writeCommandResult(w, command(r.Context(), runID))
	}
}

func (h *handler) discoveryCommandPage(command discoveryCommand) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID, ok := discoveryRunID(w, r)
		if !ok {
			return
		}
		err := command(r.Context(), runID)
		if err == nil {
			http.Redirect(w, r, "/jobs", http.StatusSeeOther)
			return
		}
		var rejection businessRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.RejectionReason(), http.StatusConflict)
			return
		}
		http.Error(w, "岗位发现操作失败，请稍后重试", http.StatusInternalServerError)
	}
}

func discoveryRunID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	runID, err := strconv.ParseInt(r.PathValue("runID"), 10, 64)
	if err != nil || runID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "岗位发现运行编号无效"})
		return 0, false
	}
	return runID, true
}

func (h *handler) queueReal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs       []int64                                     `json:"jobIds"`
		Confirmation automationsettings.RealOutreachConfirmation `json:"confirmation"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	result, err := h.dependencies.Settings.QueueRealOutreach(r.Context(), request.JobIDs, request.Confirmation)
	if err != nil {
		h.writeCommandResult(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) queueRealPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "真实打招呼内容无效", http.StatusBadRequest)
		return
	}
	jobIDs, err := parseOutreachJobIDs(r.PostForm["jobId"])
	if err != nil {
		http.Error(w, "岗位编号无效", http.StatusBadRequest)
		return
	}
	jobCount, err := strconv.Atoi(r.PostFormValue("jobCount"))
	if err != nil || jobCount < 0 {
		http.Error(w, "岗位数量无效", http.StatusBadRequest)
		return
	}
	result, err := h.dependencies.Settings.QueueRealOutreach(r.Context(), jobIDs, automationsettings.RealOutreachConfirmation{
		JobCount: jobCount, GreetingText: r.PostFormValue("greetingText"),
		TimeDescription: r.PostFormValue("timeDescription"), Confirmed: r.PostFormValue("confirmed") == "true",
	})
	if err != nil {
		var rejection businessRejection
		if errors.As(err, &rejection) {
			http.Error(w, rejection.RejectionReason(), http.StatusConflict)
			return
		}
		http.Error(w, "真实打招呼操作失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	if result.Succeeded == 0 && len(result.Skipped) > 0 {
		http.Error(w, result.Skipped[0].Reason, http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/outreach", http.StatusSeeOther)
}

func parseOutreachJobIDs(values []string) ([]int64, error) {
	jobIDs := make([]int64, 0, len(values))
	for _, value := range values {
		jobID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || jobID <= 0 {
			return nil, errors.New("invalid outreach job ID")
		}
		jobIDs = append(jobIDs, jobID)
	}
	return jobIDs, nil
}

func (h *handler) assessmentCommandAPI(command assessmentBatchCommand) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			JobIDs []int64 `json:"jobIds"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		result, err := command(r.Context(), request.JobIDs)
		if err != nil {
			h.writeCommandResult(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

type jobsBatchRequest struct {
	Action       string                                      `json:"action"`
	JobIDs       []int64                                     `json:"jobIds"`
	Decisions    []jobReviewDecisionRequest                  `json:"decisions"`
	Confirmation automationsettings.RealOutreachConfirmation `json:"confirmation"`
}

type jobReviewDecisionRequest struct {
	JobID          int64  `json:"jobId"`
	ExpectedJDHash string `json:"expectedJdHash"`
	Verdict        string `json:"verdict"`
	Note           string `json:"note"`
}

func (h *handler) batchJobsAPI(w http.ResponseWriter, r *http.Request) {
	var request jobsBatchRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	var (
		result jobpool.BatchActionResult
		err    error
	)
	switch request.Action {
	case "assessment":
		result, err = h.dependencies.Jobs.QueueAssessments(r.Context(), request.JobIDs)
	case "review":
		decisions, conversionErr := toReviewDecisions(request.Decisions)
		if conversionErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"reason": conversionErr.Error()})
			return
		}
		result, err = h.dependencies.Jobs.ReviewBatch(r.Context(), decisions)
	case "outreach":
		result, err = h.dependencies.Settings.QueueRealOutreach(r.Context(), request.JobIDs, request.Confirmation)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "批量操作类型无效"})
		return
	}
	if err != nil {
		h.writeBatchCommandResult(w, result, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func toReviewDecisions(requests []jobReviewDecisionRequest) ([]jobpool.ReviewDecision, error) {
	if len(requests) == 0 {
		return nil, errors.New("请选择要人工复核的岗位")
	}
	decisions := make([]jobpool.ReviewDecision, 0, len(requests))
	for _, request := range requests {
		decisions = append(decisions, jobpool.ReviewDecision{
			JobID: request.JobID, ExpectedJDHash: request.ExpectedJDHash,
			Verdict: jobpool.HumanVerdict(request.Verdict), Note: request.Note,
		})
	}
	return decisions, nil
}

func (h *handler) writeBatchCommandResult(w http.ResponseWriter, result jobpool.BatchActionResult, err error) {
	var rejection businessRejection
	if errors.As(err, &rejection) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": rejection.RejectionCode(), "reason": rejection.RejectionReason(),
			"succeeded": result.Succeeded, "skipped": result.Skipped,
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "批量操作失败，请稍后重试"})
}

func (h *handler) writeCommandResult(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var rejection businessRejection
	if errors.As(err, &rejection) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code":   rejection.RejectionCode(),
			"reason": rejection.RejectionReason(),
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "操作失败，请稍后重试"})
}

type businessRejection interface {
	error
	RejectionCode() string
	RejectionReason() string
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "请求内容无效"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "请求只能包含一个 JSON 对象"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
