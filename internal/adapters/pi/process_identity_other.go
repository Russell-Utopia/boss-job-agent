//go:build aix || js || plan9 || wasip1 || windows

package pi

import "fmt"

type systemProcessInspector struct{}

func (systemProcessInspector) Inspect(pid int) (ProcessIdentity, error) {
	return ProcessIdentity{}, fmt.Errorf("inspect Pi process %d: operating-system process identity is unavailable", pid)
}
