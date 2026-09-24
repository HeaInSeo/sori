package authority

import (
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

// AcceptProfile identifies the producer path of an acceptance request so a stricter,
// path-specific gate can be applied without tightening the base I1M contract
// (SORI-I4A). Internal only; not a public schema.
type AcceptProfile int

const (
	// ProfileUnspecified is the I1M-compatible default: only the base origin
	// provenance contract applies.
	ProfileUnspecified AcceptProfile = iota
	// ProfileI4AOCIDigest marks an ExternalImport produced by the I4A OCI
	// pull-by-digest acquisition path. It additionally REQUIRES
	// Provenance.ObservedVersion and a well-formed Provenance.ObservedChecksum
	// (the pinned manifest digest, i.e. the frozen subject proof).
	ProfileI4AOCIDigest
)

func (p AcceptProfile) String() string {
	switch p {
	case ProfileUnspecified:
		return "unspecified"
	case ProfileI4AOCIDigest:
		return "i4a_oci_digest"
	default:
		return "invalid"
	}
}

// validateProfile applies the profile-specific acceptance gate. It runs after the
// base validation and only ever adds requirements; ProfileUnspecified adds none.
func validateProfile(profile AcceptProfile, m SemanticManifest) error {
	switch profile {
	case ProfileUnspecified:
		return nil
	case ProfileI4AOCIDigest:
		if m.Origin != OriginExternalImport {
			return fmt.Errorf("%w: profile %s requires an external import origin", ErrInvalidManifest, profile)
		}
		if strings.TrimSpace(m.Provenance.ObservedVersion) == "" {
			return fmt.Errorf("%w: profile %s requires an observed version", ErrInvalidManifest, profile)
		}
		if strings.TrimSpace(m.Provenance.ObservedChecksum) == "" {
			return fmt.Errorf("%w: profile %s requires an observed checksum (frozen subject digest)", ErrInvalidManifest, profile)
		}
		if _, err := digest.Parse(m.Provenance.ObservedChecksum); err != nil {
			return fmt.Errorf("%w: profile %s observed checksum is not a valid digest: %v", ErrInvalidManifest, profile, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown acceptance profile %d", ErrInvalidManifest, int(profile))
	}
}
