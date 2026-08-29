package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/throwaway-prototypes/web-stack-comparison/scenario"
)

type server struct {
	root     string
	scenario *scenario.Scenario
	page     *template.Template
}

type pageData struct {
	Page        string
	Jobs        scenario.JobListView
	Policy      scenario.PolicyView
	Draft       *scenario.PolicyDraft
	BatchResult *scenario.BatchResult
}

func main() {
	address := flag.String("addr", "127.0.0.1:8091", "prototype listen address")
	root := flag.String("root", ".", "prototype directory")
	flag.Parse()

	handler, err := newHandler(*root)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Web 技术栈对比原型：http://%s/prototype/go/jobs", *address)
	log.Printf("React 对比入口：http://%s/prototype/react/", *address)
	log.Fatal(http.ListenAndServe(*address, handler))
}

func newHandler(root string) (http.Handler, error) {
	page, err := template.ParseFiles(filepath.Join(root, "templates", "page.gohtml"))
	if err != nil {
		return nil, fmt.Errorf("parse prototype template: %w", err)
	}
	s := &server{root: root, scenario: scenario.New(), page: page}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /prototype/go", redirect("/prototype/go/jobs"))
	mux.HandleFunc("GET /prototype/go/jobs", s.goJobs)
	mux.HandleFunc("POST /prototype/go/jobs/queue-simulation", s.goQueueSimulation)
	mux.HandleFunc("GET /prototype/go/assessments", s.goAssessments)
	mux.HandleFunc("POST /prototype/go/assessments/generate", s.goGenerateDraft)
	mux.HandleFunc("POST /prototype/go/reset", s.goReset)
	mux.HandleFunc("GET /prototype/api/jobs", s.apiJobs)
	mux.HandleFunc("GET /prototype/api/policy", s.apiPolicy)
	mux.HandleFunc("POST /prototype/api/policy-draft", s.apiPolicyDraft)
	mux.HandleFunc("POST /prototype/api/simulation", s.apiQueueSimulation)
	mux.HandleFunc("POST /prototype/api/reset", s.apiReset)
	mux.Handle("GET /prototype/assets/", http.StripPrefix("/prototype/assets/", http.FileServer(http.Dir(filepath.Join(root, "assets")))))
	mux.HandleFunc("GET /prototype/react", redirect("/prototype/react/"))
	mux.Handle("GET /prototype/react/", s.reactFiles())
	return mux, nil
}

func redirect(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

func (s *server) goJobs(w http.ResponseWriter, r *http.Request) {
	s.render(w, pageData{Page: "jobs", Jobs: s.scenario.Jobs(filtersFromRequest(r)), Policy: s.scenario.Policy()})
}

func (s *server) goQueueSimulation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "无法读取所选岗位", http.StatusBadRequest)
		return
	}
	filters := scenario.Filters{
		Query:           r.FormValue("query"),
		PlatformStatus:  r.FormValue("platformStatus"),
		AIConclusion:    r.FormValue("aiConclusion"),
		HumanConclusion: r.FormValue("humanConclusion"),
	}
	result := s.scenario.QueueSimulation(r.Form["jobIds"])
	s.render(w, pageData{
		Page:        "jobs",
		Jobs:        s.scenario.Jobs(filters),
		Policy:      s.scenario.Policy(),
		BatchResult: &result,
	})
}

func (s *server) goAssessments(w http.ResponseWriter, _ *http.Request) {
	s.render(w, pageData{Page: "assessments", Policy: s.scenario.Policy()})
}

func (s *server) goGenerateDraft(w http.ResponseWriter, _ *http.Request) {
	draft := s.scenario.GeneratePolicyDraft()
	s.render(w, pageData{Page: "assessments", Policy: s.scenario.Policy(), Draft: &draft})
}

func (s *server) goReset(w http.ResponseWriter, r *http.Request) {
	s.scenario.Reset()
	target := r.FormValue("returnTo")
	if !strings.HasPrefix(target, "/prototype/go/") {
		target = "/prototype/go/jobs"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *server) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.Execute(w, data); err != nil {
		http.Error(w, "原型页面渲染失败", http.StatusInternalServerError)
	}
}

func (s *server) apiJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.scenario.Jobs(filtersFromRequest(r)))
}

func (s *server) apiPolicy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.scenario.Policy())
}

func (s *server) apiPolicyDraft(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.scenario.GeneratePolicyDraft())
}

func (s *server) apiQueueSimulation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		JobIDs []string `json:"jobIds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "请求内容无效"})
		return
	}
	writeJSON(w, http.StatusOK, s.scenario.QueueSimulation(request.JobIDs))
}

func (s *server) apiReset(w http.ResponseWriter, _ *http.Request) {
	s.scenario.Reset()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) reactFiles() http.Handler {
	root := filepath.Join(s.root, "react", "dist")
	files := http.FileServer(http.Dir(root))
	return http.StripPrefix("/prototype/react/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	}))
}

func filtersFromRequest(r *http.Request) scenario.Filters {
	return scenario.Filters{
		Query:           r.URL.Query().Get("query"),
		PlatformStatus:  r.URL.Query().Get("platformStatus"),
		AIConclusion:    r.URL.Query().Get("aiConclusion"),
		HumanConclusion: r.URL.Query().Get("humanConclusion"),
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
