# Catalog Projection Example

This document describes the end-to-end flow from dataset publication to pipeline editor
binding, using a concrete genomics scenario.  This is the minimal catalog projection
that sori's open core must support.

---

## Scenario: BWA Alignment Node

A bioinformatics pipeline editor shows a BWA alignment node.  The node requires a
reference genome input:

```
inputType:  reference_genome
format:     bwa-index
organism:   Homo sapiens
reference:  GRCh38
```

The user needs to select a pre-indexed dataset from the catalog.

---

## Step 1: Publish Dataset with Metadata

The data producer publishes a BWA index using the sori chunked CAS format:

```go
meta := chunked.DatasetMetadata{
    SchemaVersion: "sori.dataset.metadata.v1",
    Kind:          "reference-index",
    DisplayName:   "GRCh38 BWA Index",
    Description:   "Human reference genome GRCh38 indexed for BWA/BWA-MEM2",
    Organism: chunked.Organism{
        Name:       "Homo sapiens",
        TaxonomyID: "9606",
    },
    Reference: chunked.DatasetReference{
        Name:    "GRCh38",
        Version: "p14",
        Aliases: []string{"hg38", "GCA_000001405.15"},
    },
    DataTypes:       []string{"reference-index"},
    FileFormats:     []string{"bwa-index"},
    CompatibleTools: []string{"bwa", "bwa-mem2"},
    CompatibleNodeTypes:  []string{"alignment"},
    CompatibleInputTypes: []string{"reference_genome", "bwa_index"},
    CompatibleInputs: []chunked.CompatibleInput{
        {
            InputType:       "reference_genome",
            Format:          "bwa-index",
            CompatibleTools: []string{"bwa", "bwa-mem2"},
            Organism:        "Homo sapiens",
            Reference:       "GRCh38",
        },
    },
    SizeBytes:        15_032_385_536, // ~14 GiB
    Source:           "https://www.ncbi.nlm.nih.gov/assembly/GCF_000001405.40",
    License:          "public-domain",
    Tags:             []string{"hg38", "GRCh38", "bwa", "alignment"},
    CreatedAt:        "2026-05-30T00:00:00Z",
    CreatedBy:        "genomics-ops",
    ValidationStatus: "validated",
    ArtifactRef:      "registry.example.com/genomics/grch38-bwa:v1",
}

metaBytes, _ := json.Marshal(meta)

_, err := chunked.Publish(ctx, storePath, srcDir, "grch38-bwa:v1", chunked.PublishOptions{
    ChunkSize:       chunked.DefaultChunkSize, // 1 GiB
    DatasetMetadata: metaBytes,
})
```

---

## Step 2: dataset-metadata.json in the OCI Manifest

After publish, the OCI manifest contains these layers:

```json
{
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.sori.chunked-cas.config.v1+json",
    "digest": "sha256:abc123...",
    "size": 88
  },
  "layers": [
    {
      "mediaType": "application/vnd.sori.chunk-index.v1+json",
      "digest": "sha256:def456...",
      "size": 1234
    },
    {
      "mediaType": "application/vnd.sori.dataset.metadata.v1+json",
      "digest": "sha256:ghi789...",
      "size": 892
    },
    { "mediaType": "application/vnd.sori.chunk.v1", "digest": "sha256:...", "size": 1073741824 },
    { "mediaType": "application/vnd.sori.chunk.v1", "digest": "sha256:...", "size": 1073741824 },
    ...
  ]
}
```

---

## Step 3: Catalog Indexer Projection

An external catalog indexer (not part of sori's open core) resolves the tag, fetches
the `dataset-metadata.json` layer, and projects a `CatalogEntry`:

```json
{
  "id": "sha256:manifest-digest-here",
  "artifactRef": "registry.example.com/genomics/grch38-bwa:v1",
  "displayName": "GRCh38 BWA Index",
  "shortDescription": "Human reference genome GRCh38 indexed for BWA/BWA-MEM2",
  "tags": ["hg38", "GRCh38", "bwa", "alignment"],
  "category": "reference-index",
  "sizeBytes": 15032385536,
  "validated": true,
  "organism": {
    "name": "Homo sapiens",
    "taxonomyId": "9606"
  },
  "reference": {
    "name": "GRCh38",
    "version": "p14",
    "aliases": ["hg38", "GCA_000001405.15"]
  },
  "compatibleInputTypes": ["reference_genome", "bwa_index"],
  "compatibleInputs": [
    {
      "inputType": "reference_genome",
      "format": "bwa-index",
      "compatibleTools": ["bwa", "bwa-mem2"],
      "organism": "Homo sapiens",
      "reference": "GRCh38"
    }
  ]
}
```

Note: `id` (manifest digest) is filled by the indexer externally after OCI tag resolve.
It is NOT stored in `dataset-metadata.json` (self-reference problem — the metadata is
an OCI layer whose digest contributes to the manifest digest).

---

## Step 4: Pipeline Editor Filtering

When the user opens the BWA alignment node, the pipeline editor queries the catalog:

```
coarse filter:  compatibleInputTypes contains "reference_genome"
precise filter: compatibleInputs where inputType = "reference_genome"
                                    AND format   = "bwa-index"
                                    AND organism = "Homo sapiens"
                                    AND reference = "GRCh38"
```

Result: the GRCh38 BWA Index entry is shown to the user.

The pipeline editor does NOT read `chunk-index.json`.  It reads catalog entries only.

---

## Step 5: User Selects Dataset — Executor Fetches at Runtime

The user selects "GRCh38 BWA Index".  The editor stores the binding:

```json
{
  "nodeId":    "bwa-align-01",
  "inputName": "reference",
  "binding": {
    "type":        "dataset",
    "artifactRef": "registry.example.com/genomics/grch38-bwa:v1"
  }
}
```

At pipeline execution time, the executor calls:

```go
err := chunked.Fetch(ctx, storePath, destRoot, "grch38-bwa:v1", chunked.FetchOptions{
    Progress: func(cp chunked.ChunkProgress) {
        log.Printf("event=%s file=%s chunk=%d bytes=%d", cp.Event, cp.File, cp.ChunkIndex, cp.Bytes)
    },
})
```

After fetch, the dataset files are at `destRoot/` and metadata at `destRoot/.sori/dataset-metadata.json`.

---

## Degraded Mode (No dataset-metadata.json)

If a dataset was published without `DatasetMetadata`, the catalog indexer can still
create a minimal entry:

```json
{
  "id": "sha256:manifest-digest-here",
  "artifactRef": "registry.example.com/genomics/some-ref:v1",
  "displayName": "registry.example.com/genomics/some-ref:v1",
  "validated": false,
  "compatibleInputTypes": [],
  "compatibleInputs": []
}
```

The entry is shown in the catalog but without matching capability.  The user can still
manually select it if they know the content.  Fetch succeeds without any metadata.

---

## Commercialization Boundary

The catalog indexer, pipeline editor UI, and executor are **not** part of sori's open
core.  This document shows the **interface contract** that the open core exposes:

- sori publishes `dataset-metadata.json` as an OCI layer with a defined schema
- sori fetches `dataset-metadata.json` to `destRoot/.sori/dataset-metadata.json`
- Any catalog service, indexer, or pipeline editor can consume that schema

The minimal example above can be reproduced without any proprietary components.
