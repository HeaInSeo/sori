package admininit

import "testing"

// TestSameName checks the absent-name rule on every OS. Codex r4320419157: with
// case folding alone, a composed "café.db" and a decomposed "café.db" both
// preflighted as PRISTINE, and on a normalization-insensitive volume the
// authority schema was written into the file the job ledger then failed to use.
func TestSameName(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"jobs.db", "jobs.db", true},
		{"jobs.db", "JOBS.db", true},
		{"café.db", "café.db", true},
		{"café.db", "café.db", true},
		{"CAFÉ.db", "café.db", true},
		{"café.db-wal", "café.db-wal", true},
		{"가.db", "가.db", true}, // Hangul syllable and its conjoining jamo
		{"café.db", "cafe.db", false},
		{"jobs.db", "authority.db", false},
		{"ﬁle.db", "file.db", false}, // compatibility (NFKC) equivalence is not joined
	}
	for _, tc := range cases {
		if got := sameName(tc.a, tc.b); got != tc.want {
			t.Errorf("sameName(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
