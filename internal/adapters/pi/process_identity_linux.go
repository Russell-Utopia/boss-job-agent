//go:build linux

package pi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type systemProcessInspector struct{}

func (systemProcessInspector) Inspect(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("inspect Pi process: PID must be positive")
	}
	startTime, err := linuxProcessStartTime(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	executable, err := linuxProcessExecutable(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{StartTime: startTime, Executable: executable}, nil
}

func linuxProcessStartTime(pid int) (string, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	contents, err := os.ReadFile(path) //nolint:gosec // PID is converted from an integer and procfs is the OS identity source.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: inspect PID %d", errProcessNotFound, pid)
		}
		return "", fmt.Errorf("read process start time for PID %d: %w", pid, err)
	}
	closeParen := strings.LastIndexByte(string(contents), ')')
	if closeParen < 0 || closeParen+1 >= len(contents) {
		return "", fmt.Errorf("read process start time for PID %d: stat format is invalid", pid)
	}
	fields := strings.Fields(string(contents[closeParen+1:]))
	const startTimeFieldIndex = 19 // /proc/<pid>/stat field 22, after fields 3 through 21.
	if len(fields) <= startTimeFieldIndex {
		return "", fmt.Errorf("read process start time for PID %d: stat fields are incomplete", pid)
	}
	if _, err := strconv.ParseUint(fields[startTimeFieldIndex], 10, 64); err != nil {
		return "", fmt.Errorf("read process start time for PID %d: %w", pid, err)
	}
	return "ticks:" + fields[startTimeFieldIndex], nil
}

func linuxProcessExecutable(pid int) (string, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "exe")
	executable, err := os.Readlink(path) //nolint:gosec // PID is converted from an integer and procfs is the OS identity source.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: inspect PID %d", errProcessNotFound, pid)
		}
		return "", fmt.Errorf("read process executable for PID %d: %w", pid, err)
	}
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("read process executable for PID %d: path is not absolute", pid)
	}
	return filepath.Clean(executable), nil
}
