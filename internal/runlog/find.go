package runlog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxJSONLLineBytes = 256 * 1024

var rotatedLogName = regexp.MustCompile(`^boss-job-agent-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3}\.jsonl$`)

type Query struct {
	TraceID        string
	Flow           Flow
	Operation      Operation
	DiscoveryRunID int64
	PlatformJobID  string
	AttemptNo      int64
	SearchRole     string
	SearchCity     string
	PageNo         int
}

type Report struct {
	Events          []json.RawMessage `json:"events"`
	IncompleteFiles []string          `json:"incompleteFiles,omitempty"`
	RetentionNotice string            `json:"retentionNotice"`
}

type IncompleteError struct {
	Files []string
}

func (e *IncompleteError) Error() string {
	return "日志查询不完整：" + strings.Join(e.Files, ", ")
}

type storedEvent struct {
	Time           string    `json:"time"`
	SchemaVersion  int       `json:"schema_version"`
	TraceID        string    `json:"trace_id"`
	Flow           Flow      `json:"flow"`
	Operation      Operation `json:"operation"`
	DiscoveryRunID int64     `json:"discovery_run_id"`
	PlatformJobID  string    `json:"platform_job_id"`
	AttemptNo      int64     `json:"attempt_no"`
	SearchRole     string    `json:"search_role"`
	SearchCity     string    `json:"search_city"`
	PageNo         int       `json:"page_no"`
}

type locatedEvent struct {
	time time.Time
	file string
	line int
	raw  json.RawMessage
}

// Find streams only the current and lumberjack-rotated JSONL files and applies
// exact typed field matches. Valid matches are preserved even when another file
// makes the overall report incomplete.
func Find(ctx context.Context, path string, query Query) (Report, error) {
	report := Report{RetentionNotice: "仅查询当前保留的运行日志；没有命中不表示历史上从未发生。"}
	if path == "" || !filepath.IsAbs(path) {
		return report, fmt.Errorf("日志路径必须是绝对路径")
	}
	if query == (Query{}) {
		return report, fmt.Errorf("日志查询至少需要一个精确字段")
	}
	files, incomplete := findLogFiles(path)
	matches, scanIncomplete, err := collectMatches(ctx, files, query)
	if err != nil {
		return report, err
	}
	incomplete = append(incomplete, scanIncomplete...)
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].time.Equal(matches[j].time) {
			return matches[i].time.Before(matches[j].time)
		}
		if matches[i].file != matches[j].file {
			return matches[i].file < matches[j].file
		}
		return matches[i].line < matches[j].line
	})
	for _, match := range matches {
		report.Events = append(report.Events, match.raw)
	}
	incomplete = uniqueSorted(incomplete)
	report.IncompleteFiles = incomplete
	if len(incomplete) > 0 {
		return report, &IncompleteError{Files: incomplete}
	}
	return report, nil
}

func collectMatches(ctx context.Context, files []string, query Query) ([]locatedEvent, []string, error) {
	matches := make([]locatedEvent, 0)
	incomplete := make([]string, 0)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		fileMatches, complete := findInFile(ctx, file, query)
		matches = append(matches, fileMatches...)
		if !complete {
			incomplete = append(incomplete, file)
		}
	}
	return matches, incomplete, nil
}

func findLogFiles(path string) ([]string, []string) {
	directory := filepath.Dir(path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, []string{directory}
	}
	files := make([]string, 0, len(entries))
	incomplete := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if name != logFilename && !rotatedLogName.MatchString(name) {
			continue
		}
		fullPath := filepath.Join(directory, name)
		info, err := os.Lstat(fullPath)
		if err != nil || !info.Mode().IsRegular() {
			incomplete = append(incomplete, fullPath)
			continue
		}
		files = append(files, fullPath)
	}
	sort.Strings(files)
	return files, incomplete
}

func findInFile(ctx context.Context, path string, query Query) ([]locatedEvent, bool) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, false
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineBytes)
	matches := make([]locatedEvent, 0)
	complete := true
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if err := ctx.Err(); err != nil {
			return matches, false
		}
		line := append([]byte(nil), scanner.Bytes()...)
		var event storedEvent
		if err := json.Unmarshal(line, &event); err != nil || event.SchemaVersion != 1 {
			complete = false
			continue
		}
		eventTime, err := time.Parse(time.RFC3339Nano, event.Time)
		if err != nil {
			complete = false
			continue
		}
		if queryMatches(query, event) {
			matches = append(matches, locatedEvent{
				time: eventTime,
				file: path,
				line: lineNumber,
				raw:  json.RawMessage(line),
			})
		}
	}
	if scanner.Err() != nil {
		complete = false
	}
	return matches, complete
}

func queryMatches(query Query, event storedEvent) bool {
	return queryCoreMatches(query, event) && queryIdentityMatches(query, event) && querySearchMatches(query, event)
}

func queryCoreMatches(query Query, event storedEvent) bool {
	return (query.TraceID == "" || query.TraceID == event.TraceID) &&
		(query.Flow == "" || query.Flow == event.Flow) &&
		(query.Operation == "" || query.Operation == event.Operation)
}

func queryIdentityMatches(query Query, event storedEvent) bool {
	return (query.DiscoveryRunID == 0 || query.DiscoveryRunID == event.DiscoveryRunID) &&
		(query.PlatformJobID == "" || query.PlatformJobID == event.PlatformJobID) &&
		(query.AttemptNo == 0 || query.AttemptNo == event.AttemptNo)
}

func querySearchMatches(query Query, event storedEvent) bool {
	return (query.SearchRole == "" || query.SearchRole == event.SearchRole) &&
		(query.SearchCity == "" || query.SearchCity == event.SearchCity) &&
		(query.PageNo == 0 || query.PageNo == event.PageNo)
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
