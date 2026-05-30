// Package bench implements the P5-6 benchmark gate for the chunked CAS path.
//
// Each Fixture generates synthetic data matching a production-representative
// profile. Use Scale < 1.0 to reduce sizes for CI; Scale = 1.0 for the
// V1 gate run.
package bench

// FileSpec describes one file within a fixture dataset.
type FileSpec struct {
	Name    string
	Size    int64
	Entropy float64 // 0.0 = all-zero bytes, 1.0 = pseudorandom (high entropy)
}

// FixtureConfig describes a benchmark fixture dataset.
type FixtureConfig struct {
	// Name identifies the fixture in result JSON filenames.
	Name string
	// Files lists the files to generate.
	Files []FileSpec
}

// TotalBytes returns the sum of all file sizes in the fixture.
func (f FixtureConfig) TotalBytes() int64 {
	var n int64
	for _, fs := range f.Files {
		n += fs.Size
	}
	return n
}

// Scale returns a copy of the fixture with all file sizes multiplied by s.
// Minimum size per file is 1 byte.
func (f FixtureConfig) Scale(s float64) FixtureConfig {
	out := FixtureConfig{Name: f.Name, Files: make([]FileSpec, len(f.Files))}
	for i, spec := range f.Files {
		sz := int64(float64(spec.Size) * s)
		if sz < 1 {
			sz = 1
		}
		out.Files[i] = FileSpec{Name: spec.Name, Size: sz, Entropy: spec.Entropy}
	}
	return out
}

const (
	GiB int64 = 1 << 30
	MiB int64 = 1 << 20
)

// AllFixtures is the canonical set of 7 fixtures at full (1.0) scale.
var AllFixtures = []FixtureConfig{
	{
		Name: "synthetic-1GiB",
		Files: []FileSpec{
			{Name: "data.bin", Size: 1 * GiB, Entropy: 1.0},
		},
	},
	{
		Name: "synthetic-10GiB",
		Files: []FileSpec{
			{Name: "part0.bin", Size: 2 * GiB, Entropy: 1.0},
			{Name: "part1.bin", Size: 2 * GiB, Entropy: 1.0},
			{Name: "part2.bin", Size: 2 * GiB, Entropy: 1.0},
			{Name: "part3.bin", Size: 2 * GiB, Entropy: 1.0},
			{Name: "part4.bin", Size: 2 * GiB, Entropy: 1.0},
		},
	},
	{
		Name: "synthetic-50GiB",
		Files: func() []FileSpec {
			out := make([]FileSpec, 10)
			for i := range out {
				out[i] = FileSpec{Name: func(i int) string {
					return "part" + string(rune('0'+i)) + ".bin"
				}(i), Size: 5 * GiB, Entropy: 1.0}
			}
			return out
		}(),
	},
	{
		// Simulates a FASTA reference: one large file with low-entropy repeating
		// structure (bases A/T/G/C/N + newlines — set Entropy=0.2).
		Name: "genomics-fasta",
		Files: []FileSpec{
			{Name: "reference.fa", Size: 3 * GiB, Entropy: 0.2},
		},
	},
	{
		// BWA index profile: 5 binary files ranging 2–5 GiB (high entropy binary).
		Name: "genomics-bwa",
		Files: []FileSpec{
			{Name: "reference.fa.amb", Size: 2 * GiB, Entropy: 0.9},
			{Name: "reference.fa.ann", Size: 3 * GiB, Entropy: 0.9},
			{Name: "reference.fa.bwt", Size: 5 * GiB, Entropy: 0.9},
			{Name: "reference.fa.pac", Size: 2 * GiB, Entropy: 0.9},
			{Name: "reference.fa.sa", Size: 3 * GiB, Entropy: 0.9},
		},
	},
	{
		// STAR index profile: 10 binary files totaling ~40 GiB.
		Name: "genomics-star",
		Files: []FileSpec{
			{Name: "Genome", Size: 3 * GiB, Entropy: 0.9},
			{Name: "SA", Size: 5 * GiB, Entropy: 0.9},
			{Name: "SAindex", Size: 7 * GiB, Entropy: 0.9},
			{Name: "genomeParameters.txt", Size: 4 * KiB, Entropy: 0.3},
			{Name: "sjdbInfo.txt", Size: 1 * MiB, Entropy: 0.3},
			{Name: "sjdbList.fromGTF.out.tab", Size: 10 * MiB, Entropy: 0.5},
			{Name: "exonGeTrInfo.tab", Size: 8 * GiB, Entropy: 0.9},
			{Name: "exonInfo.tab", Size: 6 * GiB, Entropy: 0.9},
			{Name: "geneInfo.tab", Size: 5 * GiB, Entropy: 0.9},
			{Name: "transcriptInfo.tab", Size: 6 * GiB, Entropy: 0.9},
		},
	},
	{
		// Mixed: large binaries + small metadata files.
		Name: "genomics-mixed",
		Files: []FileSpec{
			{Name: "reference.fa", Size: 3 * GiB, Entropy: 0.2},
			{Name: "reference.fa.fai", Size: 512 * KiB, Entropy: 0.4},
			{Name: "reference.dict", Size: 256 * KiB, Entropy: 0.3},
			{Name: "annotation.json", Size: 64 * KiB, Entropy: 0.3},
			{Name: "index.bin", Size: 5 * GiB, Entropy: 0.9},
		},
	},
}

const KiB int64 = 1 << 10
