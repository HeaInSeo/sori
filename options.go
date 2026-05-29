package sori

// PackageOptions controls the preferred core packaging path.
//
// This option surface is part of the stable core candidate contract.
type PackageOptions struct {
	ConfigBlob        []byte
	RequireConfigBlob bool
}

// PushOptions controls the preferred core push path.
//
// This option surface is part of the stable core candidate contract.
type PushOptions struct {
	Target RemoteTarget
}

// FetchOptions controls the preferred core fetch path.
//
// This option surface is part of the stable core candidate contract.
type FetchOptions struct {
	Concurrency             int
	RequireEmptyDestination bool
	// AtomicOverwrite enables the 3-phase overwrite path:
	//   Phase 1 — extract to a staging sibling of destRoot
	//   Phase 2 — rename existing destRoot to a backup sibling (if present)
	//   Phase 3 — rename staging to destRoot (atomic commit)
	//   Cleanup — remove backup (best-effort; warning logged on failure)
	//
	// On Phase 3 failure a best-effort rollback renames the backup back to
	// destRoot.  If that rollback also fails, destRoot may be absent; the
	// error message includes the staging and backup paths for manual recovery.
	//
	// AtomicOverwrite and RequireEmptyDestination are mutually exclusive;
	// setting both returns ErrValidation.
	AtomicOverwrite bool
}

// ReferrerOptions controls the experimental referrer helpers.
//
// Experimental: this option surface belongs to the referrer API and is not yet
// part of the frozen core contract.
type ReferrerOptions struct {
	Target RemoteTarget
}
