package chunked

import (
	"errors"
	"testing"
)

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid simple", "hg38.fa", false},
		{"valid nested", "subdir/file.bin", false},
		{"valid deep", "a/b/c/file.txt", false},
		{"empty", "", true},
		{"absolute", "/etc/passwd", true},
		{"traversal component", "../escape", true},
		{"traversal mid-path", "a/../b", true},
		{"empty segment leading slash would be abs", "/a", true},
		{"double slash", "a//b", true},
		{"trailing slash becomes empty segment", "a/b/", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePath(tc.path)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidatePath(%q) expected error, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidatePath(%q) unexpected error: %v", tc.path, err)
			}
			if tc.wantErr && !errors.Is(err, ErrPathValidation) {
				t.Fatalf("ValidatePath(%q) error %v is not ErrPathValidation", tc.path, err)
			}
		})
	}
}

func TestValidatePaths_Duplicate(t *testing.T) {
	idx := &ChunkIndex{
		Files: []ChunkIndexFile{
			{Path: "a.txt"},
			{Path: "b.txt"},
			{Path: "a.txt"},
		},
	}
	err := ValidatePaths(idx)
	if err == nil {
		t.Fatal("expected duplicate path error, got nil")
	}
	if !errors.Is(err, ErrPathValidation) {
		t.Fatalf("expected ErrPathValidation, got %v", err)
	}
}

func TestValidatePaths_Clean(t *testing.T) {
	idx := &ChunkIndex{
		Files: []ChunkIndexFile{
			{Path: "hg38.fa"},
			{Path: "hg38.fa.fai"},
			{Path: "subdir/file.bin"},
		},
	}
	if err := ValidatePaths(idx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMetadataLayerCount(t *testing.T) {
	tests := []struct {
		hasMeta       bool
		hasConfigBlob bool
		want          int
	}{
		{false, false, 1},
		{true, false, 2},
		{false, true, 2},
		{true, true, 3},
	}
	for _, tc := range tests {
		got := MetadataLayerCount(tc.hasMeta, tc.hasConfigBlob)
		if got != tc.want {
			t.Errorf("MetadataLayerCount(%v,%v) = %d, want %d",
				tc.hasMeta, tc.hasConfigBlob, got, tc.want)
		}
	}
}

func TestEstimatedChunkCount(t *testing.T) {
	const gib = int64(1 << 30)

	tests := []struct {
		name      string
		fileSizes []int64
		chunkSize int64
		want      int64
	}{
		{
			name:      "single small file",
			fileSizes: []int64{1024},
			chunkSize: gib,
			want:      1,
		},
		{
			name:      "single exact chunk",
			fileSizes: []int64{gib},
			chunkSize: gib,
			want:      1,
		},
		{
			name:      "single file three full chunks plus partial",
			fileSizes: []int64{3*gib + 500},
			chunkSize: gib,
			want:      4,
		},
		{
			name:      "five files each smaller than chunk",
			fileSizes: []int64{100, 200, 300, 400, 500},
			chunkSize: gib,
			want:      5, // each file = 1 chunk regardless of size
		},
		{
			name:      "empty file",
			fileSizes: []int64{0},
			chunkSize: gib,
			want:      1, // empty file still produces one entry
		},
		{
			name:      "mixed small and large",
			fileSizes: []int64{gib * 2, 512, gib*3 + 1},
			chunkSize: gib,
			want:      2 + 1 + 4, // 2 + 1 + 4
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimatedChunkCount(tc.fileSizes, tc.chunkSize)
			if got != tc.want {
				t.Errorf("EstimatedChunkCount(%v, %d) = %d, want %d",
					tc.fileSizes, tc.chunkSize, got, tc.want)
			}
		})
	}
}

func TestMaxChunkedLayersBudget(t *testing.T) {
	// Worst case: both optional layers present → 900 - 3 = 897 chunk layers.
	mlc := MetadataLayerCount(true, true)
	maxChunks := MaxChunkedLayers - mlc
	if maxChunks != 897 {
		t.Errorf("worst-case chunk budget = %d, want 897", maxChunks)
	}

	// Best case: no optional layers → 900 - 1 = 899 chunk layers.
	mlc = MetadataLayerCount(false, false)
	maxChunks = MaxChunkedLayers - mlc
	if maxChunks != 899 {
		t.Errorf("best-case chunk budget = %d, want 899", maxChunks)
	}
}

func TestValidateChunkSize(t *testing.T) {
	if err := ValidateChunkSize(DefaultChunkSize); err != nil {
		t.Fatalf("DefaultChunkSize should be valid: %v", err)
	}
	if err := ValidateChunkSize(MinChunkSize); err != nil {
		t.Fatalf("MinChunkSize should be valid: %v", err)
	}
	if err := ValidateChunkSize(MaxChunkSize); err != nil {
		t.Fatalf("MaxChunkSize should be valid: %v", err)
	}
	if err := ValidateChunkSize(MinChunkSize - 1); err == nil {
		t.Fatal("below MinChunkSize should be invalid")
	}
	if err := ValidateChunkSize(MaxChunkSize + 1); err == nil {
		t.Fatal("above MaxChunkSize should be invalid")
	}
}
