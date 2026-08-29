package scenario

import (
	"sort"
	"strings"
	"sync"
)

type Filters struct {
	Query           string
	PlatformStatus  string
	AIConclusion    string
	HumanConclusion string
}

type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type JobView struct {
	ID                        string             `json:"id"`
	Title                     string             `json:"title"`
	Company                   string             `json:"company"`
	City                      string             `json:"city"`
	PlatformStatus            string             `json:"platformStatus"`
	PlatformStatusText        string             `json:"platformStatusText"`
	AIConclusion              string             `json:"aiConclusion"`
	AIConclusionText          string             `json:"aiConclusionText"`
	HumanConclusion           string             `json:"humanConclusion"`
	HumanConclusionText       string             `json:"humanConclusionText"`
	CurrentJudgementText      string             `json:"currentJudgementText"`
	OutreachStatusText        string             `json:"outreachStatusText"`
	QueueSimulation           ActionAvailability `json:"queueSimulation"`
	BecameUnavailableOnSubmit bool               `json:"becameUnavailableOnSubmit"`
}

type JobListView struct {
	Filters Filters   `json:"filters"`
	Jobs    []JobView `json:"jobs"`
	Total   int       `json:"total"`
}

type BatchItemResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

type BatchResult struct {
	AcceptedCount int               `json:"acceptedCount"`
	SkippedCount  int               `json:"skippedCount"`
	Items         []BatchItemResult `json:"items"`
}

type PolicyView struct {
	Name             string   `json:"name"`
	Rules            []string `json:"rules"`
	Text             string   `json:"text"`
	HumanReviewCount int      `json:"humanReviewCount"`
}

type PolicyDraft struct {
	Text             string `json:"text"`
	BasedOnPolicy    string `json:"basedOnPolicy"`
	HumanReviewCount int    `json:"humanReviewCount"`
}

type job struct {
	ID                  string
	Title               string
	Company             string
	City                string
	PlatformStatus      string
	AIConclusion        string
	HumanConclusion     string
	CurrentJudgement    string
	OutreachStatus      string
	StaleOnFirstSubmit  bool
	StaleAlreadyApplied bool
}

type Scenario struct {
	mu   sync.Mutex
	jobs []job
}

func New() *Scenario {
	s := &Scenario{}
	s.Reset()
	return s
}

func (s *Scenario) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = initialJobs()
}

func (s *Scenario) Jobs(filters Filters) JobListView {
	s.mu.Lock()
	defer s.mu.Unlock()

	filters.Query = strings.TrimSpace(filters.Query)
	views := make([]JobView, 0, len(s.jobs))
	for _, item := range s.jobs {
		if !matches(item, filters) {
			continue
		}
		views = append(views, viewOf(item))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return JobListView{Filters: filters, Jobs: views, Total: len(views)}
}

func (s *Scenario) QueueSimulation(ids []string) BatchResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		selected[id] = struct{}{}
	}

	// One pre-declared fixture changes after the page was rendered. Both UIs
	// therefore face the same stale-submit result without owning that rule.
	for i := range s.jobs {
		item := &s.jobs[i]
		if _, ok := selected[item.ID]; !ok || !item.StaleOnFirstSubmit || item.StaleAlreadyApplied {
			continue
		}
		item.OutreachStatus = "pending_simulation"
		item.StaleAlreadyApplied = true
		break
	}

	result := BatchResult{Items: make([]BatchItemResult, 0, len(ids))}
	for _, id := range ids {
		index := s.jobIndex(id)
		if index < 0 {
			result.SkippedCount++
			result.Items = append(result.Items, BatchItemResult{ID: id, Reason: "岗位已不在当前列表中"})
			continue
		}
		item := &s.jobs[index]
		availability := simulationAvailability(*item)
		entry := BatchItemResult{ID: item.ID, Title: item.Title}
		if !availability.Allowed {
			result.SkippedCount++
			entry.Reason = availability.Reason
			result.Items = append(result.Items, entry)
			continue
		}
		item.OutreachStatus = "pending_simulation"
		result.AcceptedCount++
		entry.Accepted = true
		result.Items = append(result.Items, entry)
	}
	return result
}

func (s *Scenario) Policy() PolicyView {
	rules := []string{
		"核心职责需要以后端 Go 开发为主。",
		"岗位地点必须与在线简历中的目标城市一致。",
		"职责描述过于宽泛、无法判断技术方向时，需要人工确认。",
		"明确要求长期高频出差时判为不适合。",
	}
	return PolicyView{
		Name:             "默认策略 v1",
		Rules:            rules,
		Text:             strings.Join(rules, "\n"),
		HumanReviewCount: 6,
	}
}

func (s *Scenario) GeneratePolicyDraft() PolicyDraft {
	return PolicyDraft{
		BasedOnPolicy:    "默认策略 v1",
		HumanReviewCount: 6,
		Text: strings.Join([]string{
			"核心职责需要以后端 Go 开发为主，允许少量平台工程工作。",
			"岗位地点必须与在线简历中的目标城市一致。",
			"职责描述过于宽泛时，优先检查是否给出明确的后端交付目标；仍无法判断才需要人工确认。",
			"明确要求长期高频出差时判为不适合。",
		}, "\n"),
	}
}

func (s *Scenario) jobIndex(id string) int {
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			return i
		}
	}
	return -1
}

func matches(item job, filters Filters) bool {
	query := strings.ToLower(filters.Query)
	if query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Company), query) {
		return false
	}
	if filters.PlatformStatus != "" && item.PlatformStatus != filters.PlatformStatus {
		return false
	}
	if filters.AIConclusion != "" && item.AIConclusion != filters.AIConclusion {
		return false
	}
	if filters.HumanConclusion != "" && item.HumanConclusion != filters.HumanConclusion {
		return false
	}
	return true
}

func viewOf(item job) JobView {
	return JobView{
		ID:                        item.ID,
		Title:                     item.Title,
		Company:                   item.Company,
		City:                      item.City,
		PlatformStatus:            item.PlatformStatus,
		PlatformStatusText:        platformStatusText(item.PlatformStatus),
		AIConclusion:              item.AIConclusion,
		AIConclusionText:          aiConclusionText(item.AIConclusion),
		HumanConclusion:           item.HumanConclusion,
		HumanConclusionText:       humanConclusionText(item.HumanConclusion),
		CurrentJudgementText:      currentJudgementText(item.CurrentJudgement),
		OutreachStatusText:        outreachStatusText(item.OutreachStatus),
		QueueSimulation:           simulationAvailability(item),
		BecameUnavailableOnSubmit: item.StaleOnFirstSubmit && !item.StaleAlreadyApplied,
	}
}

func simulationAvailability(item job) ActionAvailability {
	switch {
	case item.PlatformStatus == "closed":
		return ActionAvailability{Reason: "岗位已关闭，不能加入模拟队列"}
	case item.OutreachStatus == "contacted":
		return ActionAvailability{Reason: "岗位已经完成首次沟通"}
	case item.OutreachStatus == "pending_simulation":
		return ActionAvailability{Reason: "岗位已在模拟队列中"}
	case item.OutreachStatus == "simulated":
		return ActionAvailability{Reason: "岗位已经完成过模拟沟通"}
	case item.CurrentJudgement != "suitable":
		return ActionAvailability{Reason: "岗位当前判断不是“适合”"}
	default:
		return ActionAvailability{Allowed: true}
	}
}

func platformStatusText(value string) string {
	if value == "open" {
		return "可沟通"
	}
	return "已关闭"
}

func aiConclusionText(value string) string {
	switch value {
	case "suitable":
		return "适合"
	case "unsuitable":
		return "不适合"
	case "needs_user_confirmation":
		return "需要人工确认"
	default:
		return "尚无结论"
	}
}

func humanConclusionText(value string) string {
	switch value {
	case "suitable":
		return "已复核·适合"
	case "unsuitable":
		return "已复核·不适合"
	case "needs_rereview":
		return "待重新复核"
	default:
		return "未复核"
	}
}

func currentJudgementText(value string) string {
	switch value {
	case "suitable":
		return "适合"
	case "unsuitable":
		return "不适合"
	default:
		return "等待人工复核"
	}
}

func outreachStatusText(value string) string {
	switch value {
	case "pending_simulation":
		return "等待模拟"
	case "simulated":
		return "已完成模拟"
	case "contacted":
		return "已沟通"
	default:
		return "尚未安排"
	}
}

func initialJobs() []job {
	return []job{
		{ID: "J-1001", Title: "Go 后端开发", Company: "星河科技", City: "福州", PlatformStatus: "open", AIConclusion: "unsuitable", HumanConclusion: "suitable", CurrentJudgement: "suitable", OutreachStatus: "not_queued"},
		{ID: "J-1002", Title: "Go 平台工程师", Company: "云杉网络", City: "福州", PlatformStatus: "open", AIConclusion: "needs_user_confirmation", HumanConclusion: "suitable", CurrentJudgement: "suitable", OutreachStatus: "not_queued", StaleOnFirstSubmit: true},
		{ID: "J-1003", Title: "Backend Engineer", Company: "远岚数据", City: "长沙", PlatformStatus: "open", AIConclusion: "suitable", HumanConclusion: "unreviewed", CurrentJudgement: "suitable", OutreachStatus: "not_queued"},
		{ID: "J-1004", Title: "Go 开发工程师", Company: "霁光科技", City: "福州", PlatformStatus: "closed", AIConclusion: "suitable", HumanConclusion: "suitable", CurrentJudgement: "suitable", OutreachStatus: "not_queued"},
		{ID: "J-1005", Title: "Go 工程师", Company: "岩海云", City: "长沙", PlatformStatus: "open", AIConclusion: "unsuitable", HumanConclusion: "unsuitable", CurrentJudgement: "unsuitable", OutreachStatus: "not_queued"},
		{ID: "J-1006", Title: "Go 后端研发", Company: "北辰智能", City: "福州", PlatformStatus: "open", AIConclusion: "suitable", HumanConclusion: "suitable", CurrentJudgement: "suitable", OutreachStatus: "contacted"},
		{ID: "J-1007", Title: "Go 研发工程师", Company: "河图互联", City: "长沙", PlatformStatus: "open", AIConclusion: "suitable", HumanConclusion: "needs_rereview", CurrentJudgement: "needs_review", OutreachStatus: "not_queued"},
		{ID: "J-1008", Title: "Java 后端开发", Company: "南风软件", City: "福州", PlatformStatus: "open", AIConclusion: "suitable", HumanConclusion: "unreviewed", CurrentJudgement: "suitable", OutreachStatus: "simulated"},
	}
}
