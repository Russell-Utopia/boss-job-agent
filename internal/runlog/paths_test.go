package runlog

import (
	"path/filepath"
	"testing"
)

func TestDefaultPathUsesPersistentPerUserLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		goos         string
		home         string
		xdgStateHome string
		localAppData string
		want         string
	}{
		{
			name: "macOS Library Logs",
			goos: "darwin",
			home: "/Users/example",
			want: "/Users/example/Library/Logs/boss-job-agent/boss-job-agent.jsonl",
		},
		{
			name:         "Linux absolute XDG state",
			goos:         "linux",
			home:         "/home/example",
			xdgStateHome: "/state/example",
			want:         "/state/example/boss-job-agent/logs/boss-job-agent.jsonl",
		},
		{
			name: "Linux XDG default",
			goos: "linux",
			home: "/home/example",
			want: "/home/example/.local/state/boss-job-agent/logs/boss-job-agent.jsonl",
		},
		{
			name:         "Windows LocalAppData",
			goos:         "windows",
			home:         `C:\\Users\\example`,
			localAppData: `C:\\Users\\example\\AppData\\Local`,
			want:         filepath.Join(`C:\\Users\\example\\AppData\\Local`, "boss-job-agent", "Logs", "boss-job-agent.jsonl"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := defaultPath(test.goos, test.home, test.xdgStateHome, test.localAppData)
			if err != nil {
				t.Fatalf("default path: %v", err)
			}
			if got != filepath.Clean(test.want) {
				t.Errorf("path = %q, want %q", got, filepath.Clean(test.want))
			}
		})
	}
}

func TestDefaultPathRejectsRelativeXDGStateHome(t *testing.T) {
	t.Parallel()

	if _, err := defaultPath("linux", "/home/example", "relative/state", ""); err == nil {
		t.Fatal("relative XDG_STATE_HOME was accepted")
	}
}

func TestDefaultPathRejectsRelativeWindowsLocalAppData(t *testing.T) {
	t.Parallel()

	if _, err := defaultPath("windows", `C:\\Users\\example`, ``, `relative\\AppData`); err == nil {
		t.Fatal("relative LOCALAPPDATA was accepted")
	}
}
