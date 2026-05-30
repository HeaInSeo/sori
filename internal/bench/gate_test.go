//go:build bench

package bench_test

import (
	"context"
	"os"
	"testing"

	"github.com/HeaInSeo/sori/chunked"
	"github.com/HeaInSeo/sori/internal/bench"
)

// TestGate_SmallScale runs all 7 fixtures at 0.001× scale (~1 MiB per GiB)
// to verify the gate framework is wired correctly without large disk usage.
func TestGate_SmallScale(t *testing.T) {
	const scale = 0.001
	for _, fixture := range bench.AllFixtures {
		fixture := fixture.Scale(scale)
		t.Run(fixture.Name, func(t *testing.T) {
			storePath := t.TempDir()
			srcPath := t.TempDir()
			destPath := t.TempDir()
			resultDir := t.TempDir()

			result, err := bench.RunAndGate(context.Background(), bench.RunConfig{
				Fixture:     fixture,
				ChunkSize:   chunked.MinChunkSize,
				StorePath:   storePath,
				SrcPath:     srcPath,
				DestPath:    destPath,
				SoriVersion: "v0.6.0-dev",
			}, resultDir)
			if err != nil {
				t.Fatalf("RunAndGate: %v", err)
			}
			if !result.Passed {
				t.Errorf("gate violations: %v", result.Violations)
			}

			// Verify result JSON was written.
			entries, err := os.ReadDir(resultDir)
			if err != nil || len(entries) == 0 {
				t.Error("expected result JSON in resultDir, found none")
			}
		})
	}
}

// BenchmarkGate_Synthetic1GiB runs the full-scale synthetic-1GiB fixture.
// Run with: go test -tags bench -bench=BenchmarkGate_Synthetic1GiB -benchtime=1x
func BenchmarkGate_Synthetic1GiB(b *testing.B) {
	runBenchFixture(b, bench.AllFixtures[0])
}

func BenchmarkGate_Synthetic10GiB(b *testing.B) {
	runBenchFixture(b, bench.AllFixtures[1])
}

func BenchmarkGate_Synthetic50GiB(b *testing.B) {
	runBenchFixture(b, bench.AllFixtures[2])
}

func BenchmarkGate_GenomicsBWA(b *testing.B) {
	runBenchFixture(b, bench.AllFixtures[4])
}

func BenchmarkGate_GenomicsSTAR(b *testing.B) {
	runBenchFixture(b, bench.AllFixtures[5])
}

func runBenchFixture(b *testing.B, fixture bench.FixtureConfig) {
	b.Helper()
	resultDir := "../../../../docs/bench"

	storePath, _ := os.MkdirTemp("", "sori-bench-store-*")
	srcPath, _ := os.MkdirTemp("", "sori-bench-src-*")
	destPath, _ := os.MkdirTemp("", "sori-bench-dest-*")
	b.Cleanup(func() {
		os.RemoveAll(storePath)
		os.RemoveAll(srcPath)
		os.RemoveAll(destPath)
	})

	b.ResetTimer()
	result, err := bench.RunAndGate(context.Background(), bench.RunConfig{
		Fixture:     fixture,
		ChunkSize:   chunked.DefaultChunkSize,
		StorePath:   storePath,
		SrcPath:     srcPath,
		DestPath:    destPath,
		SoriVersion: "v0.6.0-dev",
	}, resultDir)
	b.StopTimer()

	if err != nil {
		b.Fatalf("RunAndGate: %v", err)
	}
	b.ReportMetric(result.Metrics.FirstPushSeconds, "push_s")
	b.ReportMetric(result.Metrics.FetchSeconds, "fetch_s")
	b.ReportMetric(result.Metrics.PushPeakRSSMiB, "rss_MiB")
}
