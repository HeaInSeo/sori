package acquisition_test

import (
	"testing"

	"github.com/HeaInSeo/sori/acquisition"
	"github.com/HeaInSeo/sori/acquisition/checkpointtest"
)

// TestMemoryCheckpointStoreConformance runs the logical tier only. The memory
// store has nothing to reopen, so the durability tier is reported as SKIP and
// this test is not evidence that any durable store exists.
func TestMemoryCheckpointStoreConformance(t *testing.T) {
	checkpointtest.Run(t, checkpointtest.Harness{
		New: func(*testing.T) acquisition.CheckpointStore { return acquisition.NewMemoryCheckpointStore() },
	})
}
