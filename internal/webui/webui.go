package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

//go:embed templates/*.html assets/*.css
var files embed.FS

var pageTemplate = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"enabledText": func(enabled bool) string {
		if enabled {
			return "已开启"
		}
		return "已关闭"
	},
}).ParseFS(files, "templates/page.html"))

type handler struct {
	dependencies Dependencies
}

type Dependencies struct {
	Resume     *onlineresume.Versions
	Discovery  *discovery.Service
	Assessment *assessment.Service
	Settings   *automationsettings.Settings
	Runlog     *runlog.Log
}

type pageData struct {
	Page      string
	PageTitle string
	State     startupState
}

type startupState struct {
	CurrentResume *onlineresume.Version   `json:"currentResume"`
	ActivePolicy  assessment.Policy       `json:"activePolicy"`
	Automation    automationsettings.View `json:"automation"`
	Actions       firstUseActions         `json:"actions"`
	RunlogHealth  runlog.Health           `json:"runlogHealth"`
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

type stateQuery func(context.Context) (startupState, error)

func New(dependencies Dependencies) http.Handler {
	h := &handler{dependencies: dependencies}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /jobs", h.renderPage("jobs", "岗位工作台", h.jobsState))
	mux.HandleFunc("GET /assessments", h.renderPage("assessments", "岗位鉴定", h.assessmentsState))
	mux.HandleFunc("GET /outreach", h.renderPage("outreach", "打招呼", h.outreachState))
	mux.HandleFunc("GET /resume", h.renderPage("resume", "在线简历", h.resumeState))
	mux.HandleFunc("GET /assets/app.css", serveCSS)
	mux.HandleFunc("GET /api/startup-state", h.startupState)
	mux.HandleFunc("GET /api/runlog/health", h.runlogHealth)
	mux.HandleFunc("POST /api/runlog/recheck", h.recheckRunlogAPI)
	mux.HandleFunc("POST /runlog/recheck", h.recheckRunlogPage)
	mux.HandleFunc("POST /api/discovery-runs", h.startDiscovery)
	mux.HandleFunc("POST /api/outreach/real", h.queueReal)
	return mux
}

func (h *handler) jobsState(ctx context.Context) (startupState, error) {
	resume, err := h.dependencies.Resume.GetCurrent(ctx)
	if err != nil {
		return startupState{}, err
	}
	availability, err := h.dependencies.Discovery.StartAvailability(ctx)
	if err != nil {
		return startupState{}, err
	}
	return startupState{
		CurrentResume: resume,
		Actions: firstUseActions{
			StartDiscovery: fromDiscoveryAvailability(availability),
		},
		RunlogHealth: h.dependencies.Runlog.Health(),
	}, nil
}

func (h *handler) assessmentsState(ctx context.Context) (startupState, error) {
	policy, err := h.dependencies.Assessment.GetActivePolicy(ctx)
	if err != nil {
		return startupState{}, err
	}
	settings, err := h.dependencies.Settings.Get(ctx)
	if err != nil {
		return startupState{}, err
	}
	return startupState{
		ActivePolicy: policy,
		Automation:   settings,
		RunlogHealth: h.dependencies.Runlog.Health(),
	}, nil
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
	return startupState{
		Automation: settings,
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
	return startupState{CurrentResume: resume, RunlogHealth: h.dependencies.Runlog.Health()}, nil
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
	return startupState{
		CurrentResume: resume,
		ActivePolicy:  policy,
		Automation:    settings,
		Actions: firstUseActions{
			StartDiscovery:    fromDiscoveryAvailability(discoveryAvailability),
			QueueRealOutreach: fromSettingsAvailability(outreachAvailability),
		},
		RunlogHealth: h.dependencies.Runlog.Health(),
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTemplate.ExecuteTemplate(w, "page.html", pageData{
			Page:      page,
			PageTitle: title,
			State:     state,
		}); err != nil {
			return
		}
	}
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
	h.writeCommandResult(w, h.dependencies.Discovery.Start(r.Context()))
}

func (h *handler) queueReal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs       []int64                                     `json:"jobIds"`
		Confirmation automationsettings.RealOutreachConfirmation `json:"confirmation"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	h.writeCommandResult(w, h.dependencies.Settings.QueueRealOutreach(r.Context(), request.JobIDs, request.Confirmation))
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
