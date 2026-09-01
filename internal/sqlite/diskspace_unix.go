//go:build darwin || linux

package sqlite

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const minimumUpgradeHeadroom = 16 << 20

func ensureUpgradeSpace(path string) error {
	databaseInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect sqlite size before upgrade: %w", err)
	}
	required := uint64(databaseInfo.Size()*2 + minimumUpgradeHeadroom) //nolint:gosec // SQLite file sizes are non-negative.
	if walInfo, err := os.Stat(path + "-wal"); err == nil {
		required += uint64(walInfo.Size() * 2) //nolint:gosec // SQLite WAL file sizes are non-negative.
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect sqlite WAL size before upgrade: %w", err)
	}

	var status unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(path), &status); err != nil {
		return fmt.Errorf("inspect sqlite filesystem space: %w", err)
	}
	available, err := availableFilesystemBytes(status.Bavail, int64(status.Bsize))
	if err != nil {
		return fmt.Errorf("calculate sqlite filesystem space: %w", err)
	}
	if available < required {
		return fmt.Errorf(
			"insufficient space for sqlite upgrade: %d bytes available, %d required",
			available,
			required,
		)
	}
	return nil
}

func availableFilesystemBytes(availableBlocks uint64, blockSize int64) (uint64, error) {
	if blockSize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size: %d", blockSize)
	}

	unsignedBlockSize := uint64(blockSize)
	if availableBlocks > math.MaxUint64/unsignedBlockSize {
		return 0, fmt.Errorf(
			"filesystem byte count overflows uint64: %d blocks of %d bytes",
			availableBlocks,
			blockSize,
		)
	}
	return availableBlocks * unsignedBlockSize, nil
}
