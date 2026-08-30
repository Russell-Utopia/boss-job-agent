package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
)

const applicationDirectoryName = "boss-job-agent"

// DefaultPath returns the formal database location outside the application
// installation and current working directories.
func DefaultPath() (string, error) {
	configurationDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	return filepath.Join(
		configurationDirectory,
		applicationDirectoryName,
		"data",
		"boss-job-agent.db",
	), nil
}

func backupDirectory(databasePath string) string {
	databaseDirectory := filepath.Dir(databasePath)
	if filepath.Base(databaseDirectory) == "data" {
		return filepath.Join(filepath.Dir(databaseDirectory), "backups")
	}
	return filepath.Join(databaseDirectory, "backups")
}
