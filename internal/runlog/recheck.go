package runlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultRecheckInterval = time.Minute
const shutdownDrainInterval = 100 * time.Millisecond

// RunRechecks periodically performs safe recovery until the context ends.
func (l *Log) RunRechecks(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRecheckInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !l.Health().Healthy {
				l.Recheck(ctx, RepairDecision{})
			}
		}
	}
}

// Drain waits for terminal evidence retained after a write failure. It is used
// during graceful shutdown so the process cannot report a clean exit while an
// already completed external attempt still lacks its terminal log record.
func (l *Log) Drain(ctx context.Context) error {
	for {
		if l.Health().PendingTerminalRecords == 0 {
			return nil
		}
		l.Recheck(ctx, RepairDecision{})
		if l.Health().PendingTerminalRecords == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrPendingTerminalRecords, ctx.Err())
		case <-time.After(shutdownDrainInterval):
		}
	}
}

type rotatedFile struct {
	path string
	time time.Time
}

func findRetentionConflicts(path string, now time.Time, maxBackups, maxAgeDays int) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	rotated := make([]rotatedFile, 0)
	conflictSet := make(map[string]struct{})
	for _, entry := range entries {
		if !rotatedLogName.MatchString(entry.Name()) {
			continue
		}
		file, conflict, err := inspectRotatedEntry(filepath.Dir(path), entry.Name())
		if err != nil {
			return nil, err
		}
		if conflict {
			conflictSet[file.path] = struct{}{}
			continue
		}
		rotated = append(rotated, file)
	}
	sort.Slice(rotated, func(i, j int) bool { return rotated[i].time.After(rotated[j].time) })
	oldestAllowed := now.Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
	for index, file := range rotated {
		if index >= maxBackups || file.time.Before(oldestAllowed) {
			conflictSet[file.path] = struct{}{}
		}
	}
	conflicts := make([]string, 0, len(conflictSet))
	for conflict := range conflictSet {
		conflicts = append(conflicts, conflict)
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

func inspectRotatedEntry(directory, name string) (rotatedFile, bool, error) {
	fullPath := filepath.Join(directory, name)
	info, err := os.Lstat(fullPath)
	if err != nil {
		return rotatedFile{}, false, err
	}
	if !info.Mode().IsRegular() {
		return rotatedFile{path: fullPath}, true, nil
	}
	timestampText := strings.TrimSuffix(strings.TrimPrefix(name, "boss-job-agent-"), ".jsonl")
	timestamp, err := time.Parse("2006-01-02T15-04-05.000", timestampText)
	if err != nil {
		return rotatedFile{}, false, fmt.Errorf("parse rotated log timestamp %s: %w", name, err)
	}
	return rotatedFile{path: fullPath, time: timestamp}, false, nil
}
