//go:build linux

package admininit

import (
	"os"
	"path/filepath"
)

// SetTailTrace installs a hook that sees each step of the converge rule and
// the tail writer in order, and returns a function restoring the previous one.
func SetTailTrace(fn func(step string)) (restore func()) {
	prev := tailTrace
	tailTrace = fn
	return func() { tailTrace = prev }
}

// ReadTail reads keys/keyring, policy/state and epoch of adminDir through the
// strict readers. A missing file is returned as its read error.
func ReadTail(adminDir string) (Keyring, PolicyState, Epoch, error) {
	kb, err := os.ReadFile(filepath.Join(adminDir, KeysDirName, KeyringName)) // #nosec G304 -- test admin area.
	if err != nil {
		return Keyring{}, PolicyState{}, Epoch{}, err
	}
	k, err := parseKeyring(kb)
	if err != nil {
		return Keyring{}, PolicyState{}, Epoch{}, err
	}
	pb, err := os.ReadFile(filepath.Join(adminDir, PolicyDirName, PolicyStateName)) // #nosec G304 -- test admin area.
	if err != nil {
		return k, PolicyState{}, Epoch{}, err
	}
	ps, err := parsePolicyState(pb)
	if err != nil {
		return k, PolicyState{}, Epoch{}, err
	}
	eb, err := os.ReadFile(filepath.Join(adminDir, EpochName)) // #nosec G304 -- test admin area.
	if err != nil {
		return k, ps, Epoch{}, err
	}
	e, err := parseEpoch(eb)
	return k, ps, e, err
}

// PolicyDocumentInstallation exposes the strict policy-document reader.
func PolicyDocumentInstallation(doc []byte) (string, error) { return policyDocumentInstallation(doc) }
