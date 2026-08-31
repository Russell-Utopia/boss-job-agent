package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const logFilename = "boss-job-agent.jsonl"

// DefaultPath resolves the persistent per-user log path without falling back
// to the working directory or a temporary directory.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home for runlog: %w", err)
	}
	return defaultPath(runtime.GOOS, home, os.Getenv("XDG_STATE_HOME"), os.Getenv("LOCALAPPDATA"))
}

func defaultPath(goos, home, xdgStateHome, localAppData string) (string, error) {
	switch goos {
	case "darwin":
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("user home must be absolute")
		}
		return filepath.Join(home, "Library", "Logs", "boss-job-agent", logFilename), nil
	case "windows":
		if !isWindowsAbsolute(localAppData) {
			return "", fmt.Errorf("LOCALAPPDATA must be absolute")
		}
		return filepath.Join(localAppData, "boss-job-agent", "Logs", logFilename), nil
	default:
		stateRoot := xdgStateHome
		if stateRoot == "" {
			if !filepath.IsAbs(home) {
				return "", fmt.Errorf("user home must be absolute")
			}
			stateRoot = filepath.Join(home, ".local", "state")
		} else if !filepath.IsAbs(stateRoot) {
			return "", fmt.Errorf("XDG_STATE_HOME must be absolute")
		}
		return filepath.Join(stateRoot, "boss-job-agent", "logs", logFilename), nil
	}
}

func isWindowsAbsolute(path string) bool {
	if strings.HasPrefix(path, `\\`) {
		return true
	}
	if len(path) < 3 {
		return false
	}
	letter := path[0]
	return ((letter >= 'A' && letter <= 'Z') || (letter >= 'a' && letter <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}
