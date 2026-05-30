package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GateResult is the JSON record written to docs/bench/ for each gate run.
type GateResult struct {
	Date              string   `json:"date"`
	SoriVersion       string   `json:"soriVersion"`
	Fixture           string   `json:"fixture"`
	ChunkSizeBytes    int64    `json:"chunkSizeBytes"`
	UploadConcurrency int      `json:"uploadConcurrency"`
	Metrics           Metrics  `json:"metrics"`
	Passed            bool     `json:"passed"`
	Violations        []string `json:"violations"`
}

// Check evaluates the 8 pass criteria against m.
// totalArtifactBytes is the full size of all files in the fixture.
// Returns (true, nil) if all criteria pass.
func Check(m Metrics, totalArtifactBytes int64) (bool, []string) {
	var violations []string

	// 1. No full-artifact temp file: M-06 must be O(metadata), not O(artifact size).
	//    Allow up to 10 MiB of temp headroom for OCI index/metadata files.
	const maxTempMiB = 10.0
	if m.PushPeakTempDiskMiB > maxTempMiB {
		violations = append(violations, fmt.Sprintf(
			"No-full-artifact-temp-file: pushPeakTempDiskMiB=%.1f exceeds %.1f MiB",
			m.PushPeakTempDiskMiB, maxTempMiB))
	}

	// 2. RSS ceiling: push peak RSS must stay below 256 MiB (uploadConcurrency=2).
	const maxRSSMiB = 256.0
	if m.PushPeakRSSMiB > maxRSSMiB {
		violations = append(violations, fmt.Sprintf(
			"RSS-ceiling: pushPeakRSSMiB=%.1f exceeds %.1f MiB",
			m.PushPeakRSSMiB, maxRSSMiB))
	}

	// 3. Second push uploads zero chunk bytes: enforced in TC-02 unit test via
	//    progress events.  Wall-clock comparison is unreliable at small scale
	//    (metadata overhead can dominate), so no timing assertion here.

	// 4. Reconstructed tree matches source: verified by Run calling verifyHashes.
	//    If Run returns nil, this criterion is satisfied — no additional check here.

	// 5. MaxChunkedLayers fires pre-push: M-09 = 0 on failure.
	//    This is a preflight constraint verified separately in TC-07.
	//    In a normal run, BlobsCreated must be > 0.
	if m.BlobsCreated == 0 {
		violations = append(violations, "BlobsCreated=0: manifest must contain at least one blob")
	}

	// 6. Benchmark results persisted: enforced by WriteResult caller.

	// 7. Partial update upload correctness: enforced in TC-03 via progress events.
	//    Timing comparison is unreliable at small fixture scale (Exists() overhead
	//    can exceed push time for tiny chunks), so no wall-clock assertion here.

	// 8. Source bytes read must equal fixture total bytes.
	if m.SourceBytesRead != totalArtifactBytes {
		violations = append(violations, fmt.Sprintf(
			"SourceBytesRead: got %d, want %d", m.SourceBytesRead, totalArtifactBytes))
	}

	return len(violations) == 0, violations
}

// WriteResult serialises result as JSON and writes it to
// dir/YYYY-MM-DD-<fixture>.json, creating dir if needed.
func WriteResult(dir string, result GateResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bench.WriteResult: mkdir %s: %w", dir, err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	name := date + "-" + result.Fixture + ".json"
	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("bench.WriteResult: marshal: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("bench.WriteResult: write %s: %w", path, err)
	}
	return nil
}

// RunAndGate runs the benchmark for cfg and writes the result to resultDir.
// Returns ErrGateFailed (wrapping violation details) if any criterion fails.
func RunAndGate(ctx context.Context, cfg RunConfig, resultDir string) (GateResult, error) {
	metrics, err := Run(ctx, cfg)
	if err != nil {
		return GateResult{}, fmt.Errorf("bench.RunAndGate: run: %w", err)
	}

	passed, violations := Check(metrics, cfg.Fixture.TotalBytes())
	result := GateResult{
		Date:              time.Now().UTC().Format("2006-01-02"),
		SoriVersion:       cfg.SoriVersion,
		Fixture:           cfg.Fixture.Name,
		ChunkSizeBytes:    cfg.ChunkSize,
		UploadConcurrency: 2,
		Metrics:           metrics,
		Passed:            passed,
		Violations:        violations,
	}
	if result.Violations == nil {
		result.Violations = []string{}
	}

	if err := WriteResult(resultDir, result); err != nil {
		return result, err
	}
	if !passed {
		return result, fmt.Errorf("bench gate failed for %s: %v", cfg.Fixture.Name, violations)
	}
	return result, nil
}
