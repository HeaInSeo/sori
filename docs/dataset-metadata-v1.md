# Dataset Metadata v1

`dataset-metadata.json` is the human- and machine-readable dataset description
attached to a `sori` chunked CAS OCI artifact.

It is not an SBOM. It does not describe software packages, build provenance, or
dependency vulnerability state. Its job is dataset identity, catalog display,
compatibility hints, and operational context for systems that discover or fetch
reference data.

## Media Type

`dataset-metadata.json` is stored as a dedicated OCI layer with this media type:

```text
application/vnd.sori.dataset.metadata.v1+json
```

The schema version inside the JSON document is:

```json
{
  "schemaVersion": "sori.dataset.metadata.v1"
}
```

## Required Fields

The v1 required fields are:

| Field | Type | Meaning |
|---|---|---|
| `schemaVersion` | string | Must be `sori.dataset.metadata.v1`. |
| `kind` | string | Dataset category, for example `reference_genome`, `annotation`, `index`, or `dataset`. |
| `displayName` | string | Human-readable name shown in CLI/catalog views. |
| `description` | string | Human-readable description of what the dataset contains and how it should be used. |

`ValidateDatasetMetadata` and `ValidateDatasetMetadataJSON` enforce only these
minimum catalog-capable fields.

## Optional Fields

| Field | Type | Meaning |
|---|---|---|
| `organism.name` | string | Scientific or domain name for the organism. |
| `organism.taxonomyId` | string | NCBI taxonomy ID or equivalent taxonomy identifier. |
| `reference.name` | string | Reference build name, for example `GRCh38`. |
| `reference.version` | string | Reference build or resource version. |
| `reference.aliases` | string array | Alternate names accepted by tools or catalogs. |
| `dataTypes` | string array | Dataset content categories, for example `reference_genome` or `bwa_index`. |
| `fileFormats` | string array | File or logical formats, for example `FASTA`, `GTF`, `BWA index`. |
| `compatibleTools` | string array | Tool names known to consume this dataset. |
| `compatibleNodeTypes` | string array | Higher-level workflow node categories. |
| `compatibleInputTypes` | string array | Coarse input type names used by workflow systems. |
| `compatibleInputs` | object array | Structured compatibility records for precise matching. |
| `sizeBytes` | integer | Dataset size in bytes when known. |
| `source` | string | Upstream source URI or origin note. |
| `license` | string | Dataset license or redistribution policy label. |
| `tags` | string array | Free-form catalog/search tags. |
| `createdAt` | string | RFC3339 timestamp when metadata was generated. |
| `createdBy` | string | Person, tool, or system that generated metadata. |
| `validationStatus` | string | Operational status such as `unverified` or `validated`. |
| `artifactRef` | string | OCI reference intended to carry this metadata. |

`manifestDigest` is intentionally not part of `dataset-metadata.json`. The
metadata blob contributes to the OCI manifest digest, so a self-reference inside
the same blob is structurally unstable. Catalog/index systems should record the
manifest digest externally after resolving the tag.

## Unknown Fields

The current Go decoder accepts unknown JSON fields. This is the v1 policy:

- Producers may include extra fields for organization-local catalog data.
- `sori` will ignore unknown fields during validation.
- Systems that require strict schemas should enforce that policy outside
  `ValidateDatasetMetadataJSON`.
- Future `sori.dataset.metadata.v1` additions must remain backward compatible.

## Example

```json
{
  "schemaVersion": "sori.dataset.metadata.v1",
  "kind": "reference_genome",
  "displayName": "GRCh38 BWA Index",
  "description": "Human GRCh38 reference index for BWA and bwa-mem2.",
  "organism": {
    "name": "Homo sapiens",
    "taxonomyId": "9606"
  },
  "reference": {
    "name": "GRCh38",
    "version": "v1",
    "aliases": ["hg38"]
  },
  "dataTypes": ["reference_genome", "bwa_index"],
  "fileFormats": ["FASTA", "BWA index"],
  "compatibleTools": ["bwa", "bwa-mem2"],
  "source": "https://example.org/ref/grch38",
  "license": "custom",
  "tags": ["human", "reference", "bwa"],
  "validationStatus": "validated",
  "artifactRef": "ghcr.io/OWNER/references:grch38-bwa-v1"
}
```

## Producer Guidance

For v1 producers:

- Always set the four required fields.
- Prefer explicit, stable tags in `artifactRef`.
- Put tool compatibility in `compatibleTools` or `compatibleInputs`.
- Put human-facing display text in `displayName` and `description`.
- Keep organization-specific fields additive and optional.

For v1 consumers:

- Treat missing optional fields as unknown, not invalid.
- Use `schemaVersion`, `kind`, `reference`, `dataTypes`, and compatibility fields
  for machine decisions.
- Use `displayName`, `description`, `tags`, and `license` for UI/catalog display.
- Resolve the OCI tag to a manifest digest separately when immutable identity is
  required.
