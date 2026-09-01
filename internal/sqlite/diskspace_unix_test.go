//go:build darwin || linux

package sqlite

import (
	"math"
	"testing"
)

func TestAvailableFilesystemBytes(t *testing.T) {
	t.Parallel()

	got, err := availableFilesystemBytes(3, 4096)
	if err != nil {
		t.Fatalf("calculate available filesystem bytes: %v", err)
	}
	if got != 12_288 {
		t.Fatalf("available filesystem bytes = %d, want 12288", got)
	}
}

func TestAvailableFilesystemBytesRejectsInvalidBlockSize(t *testing.T) {
	t.Parallel()

	for _, blockSize := range []int64{0, -1} {
		if _, err := availableFilesystemBytes(1, blockSize); err == nil {
			t.Errorf("block size %d was accepted", blockSize)
		}
	}
}

func TestAvailableFilesystemBytesRejectsOverflow(t *testing.T) {
	t.Parallel()

	if _, err := availableFilesystemBytes(math.MaxUint64, 2); err == nil {
		t.Fatal("overflowing filesystem byte count was accepted")
	}
}
