package webui

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"net/http"

	"github.com/Russell-Utopia/boss-job-agent/internal/application"
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
	app *application.Application
}

type pageData struct {
	Page      string
	PageTitle string
	State     application.StartupState
}

type stateQuery func(context.Context) (application.StartupState, error)

func New(app *application.Application) http.Handler {
	h := &handler{app: app}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /jobs", h.renderPage("jobs", "岗位工作台", h.app.JobsState))
	mux.HandleFunc("GET /assessments", h.renderPage("assessments", "岗位鉴定", h.app.AssessmentsState))
	mux.HandleFunc("GET /outreach", h.renderPage("outreach", "打招呼", h.app.OutreachState))
	mux.HandleFunc("GET /resume", h.renderPage("resume", "在线简历", h.app.ResumeState))
	mux.HandleFunc("GET /assets/app.css", serveCSS)
	mux.HandleFunc("GET /api/startup-state", h.startupState)
	mux.HandleFunc("POST /api/discovery-runs", h.startDiscovery)
	mux.HandleFunc("POST /api/outreach/simulation", h.queueSimulation)
	mux.HandleFunc("POST /api/outreach/real", h.queueReal)
	return mux
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
	state, err := h.app.StartupState(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "无法读取当前业务状态"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *handler) startDiscovery(w http.ResponseWriter, r *http.Request) {
	h.writeCommandResult(w, h.app.StartDiscovery(r.Context()))
}

func (h *handler) queueSimulation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs []int64 `json:"jobIds"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	h.writeCommandResult(w, h.app.QueueSimulationOutreach(r.Context(), request.JobIDs))
}

func (h *handler) queueReal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs       []int64                              `json:"jobIds"`
		Confirmation application.RealOutreachConfirmation `json:"confirmation"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	h.writeCommandResult(w, h.app.QueueRealOutreach(r.Context(), request.JobIDs, request.Confirmation))
}

func (h *handler) writeCommandResult(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rejection, ok := application.AsRejection(err); ok {
		writeJSON(w, http.StatusConflict, rejection)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"reason": "操作失败，请稍后重试"})
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
