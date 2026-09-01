//go:build dragonfly || freebsd || netbsd || openbsd || solaris

package pi

import "fmt"

type systemProcessInspector struct{}

func (systemProcessInspector) Inspect(pid int) (ProcessIdentity, error) {
	return ProcessIdentity{}, fmt.Errorf(
		"inspect Pi process %d: high-precision process identity is unavailable on this operating system",
		pid,
	)
}
