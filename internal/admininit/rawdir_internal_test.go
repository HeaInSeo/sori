package admininit

import "testing"

// TestRawDirOf checks the raw parent with the Windows separator rule (both '/'
// and '\') and the Unix rule. Codex r4319772336: with only '\' recognized,
// "C:/data/jobs.db" had the parent "." and "C:\data\JOBS.db" had "C:\data\",
// so sameFile compared two different directories and missed the alias.
func TestRawDirOf(t *testing.T) {
	windows := func(c uint8) bool { return c == '/' || c == '\\' }
	unix := func(c uint8) bool { return c == '/' }
	cases := []struct {
		path, volume string
		isSep        func(uint8) bool
		want         string
	}{
		{`C:/data/jobs.db`, `C:`, windows, `C:/data/`},
		{`C:\data\JOBS.db`, `C:`, windows, `C:\data\`},
		{`C:\data/jobs.db`, `C:`, windows, `C:\data/`},
		{`C:/data\jobs.db`, `C:`, windows, `C:/data\`},
		{`C:\link\..\data/jobs.db`, `C:`, windows, `C:\link\..\data/`},
		{`C:jobs.db`, `C:`, windows, `C:.`},
		{`\\host\share\jobs.db`, `\\host\share`, windows, `\\host\share\`},
		{`jobs.db`, ``, windows, `.`},
		{`/data/link/../jobs.db`, ``, unix, `/data/link/../`},
		{`/data/a\b.db`, ``, unix, `/data/`},
		{`jobs.db`, ``, unix, `.`},
	}
	for _, tc := range cases {
		if got := rawDirOf(tc.path, tc.volume, tc.isSep); got != tc.want {
			t.Errorf("rawDirOf(%q, %q) = %q; want %q", tc.path, tc.volume, got, tc.want)
		}
	}
}
