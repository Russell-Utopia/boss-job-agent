//go:build darwin

package pi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type systemProcessInspector struct{}

func (systemProcessInspector) Inspect(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("inspect Pi process: PID must be positive")
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcessIdentity{}, classifyDarwinProcessError(pid, err)
	}
	if int(process.Proc.P_pid) != pid {
		return ProcessIdentity{}, fmt.Errorf("%w: inspect PID %d", errProcessNotFound, pid)
	}
	if process.Proc.P_stat == 'Z' {
		return ProcessIdentity{}, fmt.Errorf("%w: PID %d is a zombie", errProcessNotFound, pid)
	}
	command, err := darwinProcessCommand(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	executable, err := processExecutable(command)
	if err != nil {
		return ProcessIdentity{}, err
	}
	startTime := fmt.Sprintf("timeval:%d:%d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec)
	return ProcessIdentity{StartTime: startTime, Executable: executable}, nil
}

func darwinProcessCommand(pid int) (string, error) {
	// ps only reads process metadata; it never sends a signal or changes a process.
	output, err := exec.CommandContext( //nolint:gosec // PID is converted from an integer and ps is a fixed executable.
		context.Background(), "ps", "-p", strconv.Itoa(pid), "-o", "command=",
	).Output()
	if err != nil {
		return "", classifyDarwinProcessError(pid, fmt.Errorf("inspect executable with ps: %w", err))
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("inspect PID %d executable with ps: command is missing", pid)
	}
	return value, nil
}

func classifyDarwinProcessError(pid int, cause error) error {
	if !errors.Is(cause, unix.ESRCH) && !errors.Is(cause, unix.ENOENT) {
		processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
		if err != nil {
			return fmt.Errorf("inspect PID %d process list: %w", pid, errors.Join(err, cause))
		}
		for _, process := range processes {
			if int(process.Proc.P_pid) == pid {
				return fmt.Errorf("inspect PID %d: %w", pid, cause)
			}
		}
	}
	return fmt.Errorf("%w: inspect PID %d", errProcessNotFound, pid)
}

func processExecutable(command string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", fmt.Errorf("inspect Pi process: executable is missing")
	}
	executable := strings.Trim(fields[0], "()")
	if !filepath.IsAbs(executable) {
		resolved, err := exec.LookPath(executable)
		if err != nil {
			return "", fmt.Errorf("resolve Pi executable %q: %w", executable, err)
		}
		executable = resolved
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Pi executable path %q: %w", executable, err)
	}
	return filepath.Clean(canonical), nil
}
