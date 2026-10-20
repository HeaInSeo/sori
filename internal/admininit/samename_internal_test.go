package admininit

import "testing"

// TestSameName checks the absent-name rule on every OS. Codex r4320419157: with
// case folding alone, a composed "café.db" and a decomposed "café.db" both
// preflighted as PRISTINE, and on a normalization-insensitive volume the
// authority schema was written into the file the job ledger then failed to use.
// Codex r4321440348 and r4321440350: the same happened for Win32 trailing dot and
// space aliases and for names that only full Unicode case folding joins.
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
		// Win32 drops trailing dots and spaces when it opens a path.
		{"jobs.db", "jobs.db.", true},
		{"jobs.db", "jobs.db ", true},
		{"jobs.db", "JOBS.DB. .", true},
		{"jobs.db-wal", "jobs.db-wal.", true},
		{"jobs.db", ".jobs.db", false}, // leading dots are kept
		{"jobs.db", " jobs.db", false}, // leading spaces are kept
		{"jobs.db", "jobs .db", false},
		// Full case folding joins what simple folding (strings.EqualFold) keeps apart.
		{"ß.db", "ss.db", true},
		{"straße.db", "STRASSE.db", true},
		{"ǰ.db", "ǰ.db", true}, // U+01F0 folds to "j" plus U+030C
		{"ß.db", "s.db", false},
		{"ﬁle.db", "file.db", true}, // full case folding maps U+FB01 to "fi"
		{"①.db", "1.db", false},     // compatibility (NFKC) equivalence alone is not joined
	}
	for _, tc := range cases {
		if got := sameName(tc.a, tc.b); got != tc.want {
			t.Errorf("sameName(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
