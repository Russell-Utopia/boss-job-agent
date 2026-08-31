package runlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestProductionRotationPolicyIsFixed(t *testing.T) {
	t.Parallel()

	logs := Open(filepath.Join(t.TempDir(), logFilename))
	t.Cleanup(func() { _ = logs.Close() })
	logs.mu.RLock()
	writer, ok := logs.writer.(*lumberjack.Logger)
	logs.mu.RUnlock()
	if !ok {
		t.Fatalf("writer type = %T, want lumberjack", logs.writer)
	}
	if writer.MaxSize != 10 || writer.MaxBackups != 9 || writer.MaxAge != 30 || writer.LocalTime || writer.Compress {
		t.Errorf("rotation policy = %#v, want 10 MB, 9 backups, 30 days, UTC, uncompressed", writer)
	}
}

func TestConcurrentAttemptsRemainValidJSONAcrossSizeRotation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), logFilename)
	logs := open(path, options{maxSizeMB: 1, maxBackups: 9, maxAgeDays: 30})
	writeConcurrentAttempts(t, logs)
	closeRunlog(t, logs)
	assertRotatedFiles(t, path)
}

func TestRealWriterKeepsAtMostNineBackupsDuringNormalRotation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), logFilename)
	logs := open(path, options{maxSizeMB: 1, maxBackups: 9, maxAgeDays: 30})
	writePaddingRecords(t, logs, 140)
	closeRunlog(t, logs)

	files := waitForLogFileCount(t, path, 10)
	if len(files) != 10 {
		t.Fatalf("log files after more than nine rotations = %d, want current plus nine backups: %v", len(files), files)
	}
}

func TestRealWriterDeletesBackupOlderThanThirtyDaysDuringNormalRotation(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, logFilename)
	logs := open(path, options{maxSizeMB: 1, maxBackups: 9, maxAgeDays: 30})
	t.Cleanup(func() { _ = logs.Close() })
	oldBackup := filepath.Join(directory, "boss-job-agent-"+
		time.Now().UTC().Add(-31*24*time.Hour).Format("2006-01-02T15-04-05.000")+".jsonl")
	if err := os.WriteFile(oldBackup, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write old backup: %v", err)
	}
	writePaddingRecords(t, logs, 12)

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(oldBackup)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("old backup still exists after real writer cleanup: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writePaddingRecords(t *testing.T, logs *Log, count int) {
	t.Helper()
	padding := strings.Repeat("x", 100*1024)
	for index := 0; index < count; index++ {
		record := slog.NewRecord(time.Now().UTC(), slog.LevelInfo, "rotation test", 0)
		record.AddAttrs(
			slog.Int("schema_version", 1),
			slog.String("event", "rotation_test"),
			slog.Int("index", index),
			slog.String("padding", padding),
		)
		logs.mu.Lock()
		err := logs.handleLocked(t.Context(), record)
		logs.mu.Unlock()
		if err != nil {
			t.Fatalf("write padding record %d: %v", index, err)
		}
	}
}

func waitForLogFileCount(t *testing.T, path string, wantAtMost int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		files, incomplete := findLogFiles(path)
		if len(incomplete) != 0 {
			t.Fatalf("incomplete log files = %v", incomplete)
		}
		if len(files) <= wantAtMost {
			return files
		}
		if time.Now().After(deadline) {
			return files
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeConcurrentAttempts(t *testing.T, logs *Log) {
	t.Helper()
	const goroutines = 8
	const attemptsPerGoroutine = 100
	writeErrors := make(chan error, goroutines*attemptsPerGoroutine)
	var group sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			writeAttemptBatch(t.Context(), logs, worker, attemptsPerGoroutine, writeErrors)
		}(worker)
	}
	group.Wait()
	close(writeErrors)
	for err := range writeErrors {
		t.Errorf("write concurrent attempt: %v", err)
	}
}

func writeAttemptBatch(ctx context.Context, logs *Log, worker, count int, writeErrors chan<- error) {
	for index := 0; index < count; index++ {
		attemptNo := int64(worker*count + index + 1)
		trace, err := logs.Start(ctx, testDiscoveryAttempt(attemptNo))
		if err != nil {
			writeErrors <- err
			continue
		}
		err = logs.Finish(ctx, trace, AttemptResult{
			Outcome:       OutcomeFailed,
			ErrorCategory: ErrorCategoryTransient,
			Err:           errors.New(strings.Repeat(fmt.Sprintf("worker-%d-", worker), 700)),
		})
		if err != nil {
			writeErrors <- err
		}
	}
}

func assertRotatedFiles(t *testing.T, path string) {
	t.Helper()
	files, incomplete := findLogFiles(path)
	if len(incomplete) != 0 {
		t.Fatalf("incomplete rotated files = %v", incomplete)
	}
	if len(files) < 2 {
		t.Fatalf("log files = %v, want size rotation", files)
	}
	if len(files) > 10 {
		t.Fatalf("log files = %d, want current plus at most nine backups", len(files))
	}
	for _, file := range files {
		assertFileContainsOnlyJSONObjects(t, file)
	}
}

func TestRetentionOverflowRequiresConfirmationBeforeIsolation(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, logFilename)
	now := time.Now().UTC()
	for index := 0; index < 10; index++ {
		name := "boss-job-agent-" + now.Add(-time.Duration(index)*time.Hour).Format("2006-01-02T15-04-05.000") + ".jsonl"
		if err := os.WriteFile(filepath.Join(directory, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write rotated log %d: %v", index, err)
		}
	}
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })
	if health := logs.Health(); health.Healthy || !health.ConfirmationRequired {
		t.Fatalf("retention overflow health = %#v, want confirmation required", health)
	}
	if health := logs.Recheck(t.Context(), RepairDecision{ConfirmQuarantine: true}); !health.Healthy {
		t.Fatalf("confirmed retention repair health = %#v, want healthy", health)
	}
	files, incomplete := findLogFiles(path)
	if len(incomplete) != 0 {
		t.Fatalf("incomplete files after retention repair = %v", incomplete)
	}
	if len(files) != 10 {
		t.Fatalf("retained files = %d, want current plus nine backups", len(files))
	}
}

func TestBackupOlderThanThirtyDaysRequiresConfirmationBeforeIsolation(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, logFilename)
	name := "boss-job-agent-" + time.Now().UTC().Add(-31*24*time.Hour).Format("2006-01-02T15-04-05.000") + ".jsonl"
	if err := os.WriteFile(filepath.Join(directory, name), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write expired backup: %v", err)
	}
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })
	if health := logs.Health(); health.Healthy || !health.ConfirmationRequired {
		t.Fatalf("expired backup health = %#v, want confirmation required", health)
	}
	if health := logs.Recheck(t.Context(), RepairDecision{ConfirmQuarantine: true}); !health.Healthy {
		t.Fatalf("confirmed expired backup repair health = %#v, want healthy", health)
	}
}

func assertFileContainsOnlyJSONObjects(t *testing.T, path string) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open rotated JSONL root: %v", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		t.Fatalf("open rotated JSONL %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		var object map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &object); err != nil {
			t.Fatalf("decode %s line %d: %v", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
}
