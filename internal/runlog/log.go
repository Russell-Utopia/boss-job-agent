package runlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	healthCodeHealthy     = "healthy"
	healthCodeUnavailable = "log_unavailable"
	healthCodeQuarantine  = "log_quarantine_required"
	productionMaxSizeMB   = 10
	productionMaxBackups  = 9
	productionMaxAgeDays  = 30
)

var (
	ErrUnavailable            = errors.New("runlog is unavailable")
	ErrPendingTerminalRecords = errors.New("runlog has unpersisted terminal records")
)

// Log owns the persistent JSONL run history and its health state.
type Log struct {
	mu                     sync.RWMutex
	path                   string
	writer                 io.WriteCloser
	handler                slog.Handler
	health                 Health
	now                    func() time.Time
	stderr                 io.Writer
	options                options
	resolvePath            func() (string, error)
	pendingTerminalRecords []slog.Record
}

type options struct {
	maxSizeMB  int
	maxBackups int
	maxAgeDays int
}

// Open creates a recoverable runlog. Path failures are represented by Health
// so the rest of the local Web application can continue to start.
func Open(path string) *Log {
	return open(path, options{
		maxSizeMB:  productionMaxSizeMB,
		maxBackups: productionMaxBackups,
		maxAgeDays: productionMaxAgeDays,
	})
}

// OpenDefault resolves the platform-specific path on every recheck. A path
// resolution failure therefore becomes recoverable health state instead of an
// application startup error, without falling back to a less durable location.
func OpenDefault() *Log {
	return openResolved(DefaultPath, options{
		maxSizeMB:  productionMaxSizeMB,
		maxBackups: productionMaxBackups,
		maxAgeDays: productionMaxAgeDays,
	})
}

func open(path string, configuration options) *Log {
	return openResolved(func() (string, error) { return path, nil }, configuration)
}

func openResolved(resolvePath func() (string, error), configuration options) *Log {
	logs := &Log{
		now:         time.Now,
		stderr:      os.Stderr,
		options:     configuration,
		resolvePath: resolvePath,
	}
	logs.Recheck(context.Background(), RepairDecision{})
	return logs
}

// Health returns a snapshot safe for concurrent Web and worker access.
func (l *Log) Health() Health {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.health
}

type RepairDecision struct {
	ConfirmQuarantine bool `json:"confirmQuarantine"`
}

// Recheck immediately validates the durable log and performs only safe
// repairs unless the caller explicitly confirms quarantining conflicting data.
func (l *Log) Recheck(ctx context.Context, decision RepairDecision) Health {
	if err := ctx.Err(); err != nil {
		return l.Health()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recheckLocked(decision)
	return l.health
}

func (l *Log) recheckLocked(decision RepairDecision) {
	checkedAt := l.now().UTC()
	wasHealthy := l.health.Healthy
	l.closeWriterLocked()
	path, err := l.resolvePath()
	if err != nil {
		l.degradeLocked(checkedAt, healthCodeUnavailable, fmt.Sprintf("解析运行日志路径失败：%v", err), false)
		return
	}
	l.path = path
	if !l.prepareStorageLocked(decision, checkedAt) {
		return
	}
	l.openWriterLocked()
	if !l.replayTerminalRecordsLocked() {
		return
	}
	if !l.writeReadyRecordLocked(checkedAt, wasHealthy) {
		return
	}
	l.setHealthLocked(Health{
		Healthy:   true,
		Code:      healthCodeHealthy,
		Message:   "运行日志正常",
		CheckedAt: checkedAt,
	})
}

func (l *Log) replayTerminalRecordsLocked() bool {
	for len(l.pendingTerminalRecords) > 0 {
		if err := l.handleLocked(context.Background(), l.pendingTerminalRecords[0]); err != nil {
			return false
		}
		l.pendingTerminalRecords = l.pendingTerminalRecords[1:]
		l.health.PendingTerminalRecords = len(l.pendingTerminalRecords)
	}
	return true
}

func (l *Log) queueTerminalRecordLocked(record slog.Record) {
	l.pendingTerminalRecords = append(l.pendingTerminalRecords, record.Clone())
	l.health.PendingTerminalRecords = len(l.pendingTerminalRecords)
}

func (l *Log) closeWriterLocked() {
	if l.writer != nil {
		_ = l.writer.Close()
		l.writer = nil
		l.handler = nil
	}
}

func (l *Log) prepareStorageLocked(decision RepairDecision, checkedAt time.Time) bool {
	if err := ensurePrivatePath(l.path, decision, checkedAt); err != nil {
		l.reportPathPreparationErrorLocked(checkedAt, err)
		return false
	}
	retentionConflicts, err := findRetentionConflicts(l.path, checkedAt, l.options.maxBackups, l.options.maxAgeDays)
	if err != nil {
		l.degradeLocked(checkedAt, healthCodeUnavailable, fmt.Sprintf("检查日志保留边界失败：%v", err), false)
		return false
	}
	return l.resolveRetentionConflictsLocked(retentionConflicts, decision, checkedAt)
}

func (l *Log) reportPathPreparationErrorLocked(checkedAt time.Time, err error) {
	var confirmation *confirmationError
	if errors.As(err, &confirmation) {
		l.degradeLocked(checkedAt, healthCodeQuarantine, fmt.Sprintf("运行日志路径包含需要隔离的数据：%s", confirmation.path), true)
		return
	}
	l.degradeLocked(checkedAt, healthCodeUnavailable, fmt.Sprintf("运行日志不可写：%v", err), false)
}

func (l *Log) resolveRetentionConflictsLocked(conflicts []string, decision RepairDecision, checkedAt time.Time) bool {
	if len(conflicts) == 0 {
		return true
	}
	if !decision.ConfirmQuarantine {
		l.degradeLocked(checkedAt, healthCodeQuarantine, "运行日志超过保留边界，需要确认后隔离超出部分", true)
		return false
	}
	for _, conflict := range conflicts {
		if _, err := quarantinePath(conflict, checkedAt); err != nil {
			l.degradeLocked(checkedAt, healthCodeUnavailable, fmt.Sprintf("隔离超出保留边界的日志失败：%v", err), false)
			return false
		}
	}
	return true
}

func (l *Log) openWriterLocked() {
	writer := &lumberjack.Logger{
		Filename:   l.path,
		MaxSize:    l.options.maxSizeMB,
		MaxBackups: l.options.maxBackups,
		MaxAge:     l.options.maxAgeDays,
		LocalTime:  false,
		Compress:   false,
	}
	l.writer = writer
	l.handler = slog.NewJSONHandler(writer, &slog.HandlerOptions{ReplaceAttr: utcTimeAttribute})
}

func (l *Log) writeReadyRecordLocked(checkedAt time.Time, wasHealthy bool) bool {
	event := "runlog_started"
	message := "runlog ready"
	if !wasHealthy && l.health.Code != "" {
		event = "runlog_recovered"
		message = "runlog recovered"
	}
	startup := slog.NewRecord(checkedAt, slog.LevelInfo, message, 0)
	startup.AddAttrs(
		slog.Int("schema_version", 1),
		slog.String("event", event),
	)
	if err := l.handleLocked(context.Background(), startup); err != nil {
		return false
	}
	return true
}

type confirmationError struct {
	path string
}

func (e *confirmationError) Error() string {
	return "quarantine confirmation required for " + e.path
}

func ensurePrivatePath(path string, decision RepairDecision, now time.Time) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("日志路径必须是绝对路径")
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory, decision, now); err != nil {
		return err
	}
	return ensurePrivateLogFile(path, decision, now)
}

func ensurePrivateDirectory(directory string, decision RepairDecision, now time.Time) error {
	if err := preparePathKind(directory, true, decision, now); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建日志目录: %w", err)
	}
	if runtime.GOOS != "windows" {
		//nolint:gosec // A private directory needs the owner execute bit; 0600 would make it unusable.
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("收紧日志目录权限: %w", err)
		}
	}
	return nil
}

func ensurePrivateLogFile(path string, decision RepairDecision, now time.Time) error {
	if err := preparePathKind(path, false, decision, now); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("打开日志目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("收紧日志文件权限: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭日志预检文件: %w", err)
	}
	return nil
}

func preparePathKind(path string, wantDirectory bool, decision RepairDecision, now time.Time) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查日志路径 %s: %w", path, err)
	}
	valid := info.Mode()&os.ModeSymlink == 0
	if wantDirectory {
		valid = valid && info.IsDir()
	} else {
		valid = valid && info.Mode().IsRegular()
	}
	if valid {
		return nil
	}
	if !decision.ConfirmQuarantine {
		return &confirmationError{path: path}
	}
	if _, err := quarantinePath(path, now); err != nil {
		return fmt.Errorf("隔离冲突日志路径 %s: %w", path, err)
	}
	return nil
}

func quarantinePath(path string, now time.Time) (string, error) {
	base := path + ".quarantine-" + now.UTC().Format("20060102T150405.000000000Z")
	target := base
	for suffix := 1; ; suffix++ {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		target = fmt.Sprintf("%s-%d", base, suffix)
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
}

func utcTimeAttribute(_ []string, attribute slog.Attr) slog.Attr {
	if attribute.Key == slog.TimeKey && attribute.Value.Kind() == slog.KindTime {
		return slog.Time(slog.TimeKey, attribute.Value.Time().UTC())
	}
	return attribute
}

func (l *Log) handleLocked(ctx context.Context, record slog.Record) error {
	if l.handler == nil {
		return ErrUnavailable
	}
	if err := l.handler.Handle(ctx, record); err != nil {
		checkedAt := l.now().UTC()
		l.degradeLocked(checkedAt, healthCodeUnavailable, fmt.Sprintf("运行日志写入失败：%v", err), false)
		_, _ = fmt.Fprintf(l.stderr, "boss-job-agent: runlog degraded: %v\n", err)
		return fmt.Errorf("persist runlog record: %w", err)
	}
	return nil
}

func (l *Log) degradeLocked(checkedAt time.Time, code, message string, confirmationRequired bool) {
	l.setHealthLocked(Health{
		Code:                 code,
		Message:              message,
		CheckedAt:            checkedAt,
		ConfirmationRequired: confirmationRequired,
	})
}

func (l *Log) setHealthLocked(health Health) {
	health.PendingTerminalRecords = len(l.pendingTerminalRecords)
	l.health = health
}

// Close releases the active log file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pendingTerminalRecords) > 0 {
		l.recheckLocked(RepairDecision{})
	}
	var closeErr error
	if l.writer != nil {
		if err := l.writer.Close(); err != nil {
			closeErr = fmt.Errorf("close runlog: %w", err)
		}
		l.writer = nil
		l.handler = nil
	}
	var pendingErr error
	if len(l.pendingTerminalRecords) > 0 {
		pendingErr = fmt.Errorf("%w: %d", ErrPendingTerminalRecords, len(l.pendingTerminalRecords))
	}
	return errors.Join(closeErr, pendingErr)
}
