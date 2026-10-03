package authoritytest_test

import (
	"testing"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/authority/authoritytest"
)

// The in-memory reference Store must satisfy the full backend-neutral contract. It
// makes no durability claim, so Reopen is nil and the Reopen cases report
// NOT IMPLEMENTED (skipped) rather than passing.
func TestMemoryStoreConformance(t *testing.T) {
	authoritytest.Run(t, authoritytest.Harness{
		New: func(*testing.T) authority.Store { return authority.NewMemoryStore() },
	})
}
