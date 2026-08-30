//go:build !darwin && !linux

package sqlite

func ensureUpgradeSpace(string) error {
	return nil
}
