package acquisition_test

import (
	"testing"

	"github.com/HeaInSeo/sori/acquisition"
	"github.com/HeaInSeo/sori/acquisition/sqlitecheckpoint"
)

func openDurable(t *testing.T, path string) acquisition.DurableCheckpointStore {
	t.Helper()
	s, err := sqlitecheckpoint.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// W01.2 E021: the existing staged-unavailable and digest-mismatch policies hold when
// the resume reads the checkpoint back from the SQLite file after a reopen.
func TestDurableStagedResumeSQLite(t *testing.T) {
	acquisition.RunDurableStagedResumeForTest(t, openDurable)
}
