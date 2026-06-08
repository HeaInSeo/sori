# sorictl v1 CLI Contract

`sorictl` is the CLI UX layer for publishing and fetching `sori` dataset
artifacts. It is registry-neutral: GHCR, Harbor, local registries, and other OCI
registries use the same reference shape.

The v1 CLI contract is intentionally small.

## Stable Commands

The v1 command set is:

```text
sorictl metadata init [flags]
sorictl dataset push [flags] SOURCE_DIR
sorictl dataset inspect [flags] REGISTRY/REPOSITORY:TAG
sorictl dataset fetch [flags] REGISTRY/REPOSITORY:TAG DEST
```

Additional commands may be added later, but these four commands should remain
available throughout v1 patch/minor releases.

## Reference Format

The beginner and automation path is `--ref`:

```text
REGISTRY/REPOSITORY:TAG
```

Examples:

```text
ghcr.io/OWNER/references:grch38-bwa-v1
harbor.example.com/bio/refdata:grch38-bwa-v1
localhost:5000/bio/refdata:tiny-v1
```

`dataset inspect` and `dataset fetch` take the same reference as their
positional first argument.

## Auth Flags

The stable registry auth flags are:

| Flag | Applies to | Meaning |
|---|---|---|
| `--username` | push, inspect, fetch | Registry username. Defaults to `SORI_REGISTRY_USERNAME`. |
| `--password-env` | push, inspect, fetch | Environment variable containing a password or PAT. Defaults to `SORI_REGISTRY_PASSWORD`. |
| `--token-env` | push, inspect, fetch | Environment variable containing a bearer token or PAT. Defaults to `SORI_REGISTRY_TOKEN`. |
| `--plain-http` | push, inspect, fetch | Use HTTP for lab registries. |
| `--insecure-tls` | push, inspect, fetch | Skip TLS verification. Prefer `--ca-file` for production. |
| `--ca-file` | push, inspect, fetch | Custom CA bundle path. |

When username is present and password is empty, `SORI_REGISTRY_TOKEN` is treated
as a basic-auth password/PAT. When username is absent, token-only auth is treated
as bearer-token auth.

## Output Modes

The stable output modes are:

```text
--output text
--output json
```

`text` is for humans and may gain additional lines. It should not be parsed by
automation.

`json` is for automation. v1 patch/minor releases should avoid removing fields
or changing field meanings. New fields may be added.

## dataset push

Stable flags:

| Flag | Meaning |
|---|---|
| `--ref` | Full target reference. Preferred. |
| `--registry` | Registry host when not using `--ref`. |
| `--repository` | Repository path when not using `--ref`. |
| `--tag` | Artifact tag when not using `--ref`. |
| `--store` | Local OCI store path. |
| `--metadata` | Path to `dataset-metadata.json`. |
| `--name` | Dataset machine name used for generated metadata. |
| `--version` | Dataset version used for generated metadata. |
| `--display-name` | Human-readable display name. |
| `--description` | Human-readable description. |
| `--kind` | Dataset kind. |
| `--source-uri` | Upstream source URI. |
| `--require-metadata` | Fail when metadata cannot be attached. Defaults to true. |
| `--progress` | Print chunk progress to stderr. Defaults to true. |

The v1 default artifact format for the CLI is chunked CAS.

JSON output fields:

| Field | Meaning |
|---|---|
| `reference` | Pushed OCI reference. |
| `repository` | Remote repository path. |
| `tag` | Pushed tag. |
| `manifestDigest` | Resolved manifest digest after push. |
| `localStore` | Local OCI store path used during packaging. |
| `localTag` | Local OCI tag used during packaging. |
| `totalSize` | Total source size in bytes. |
| `datasetMetadata` | Whether dataset metadata was attached. |
| `metadataMediaType` | Metadata media type when attached. |

## dataset inspect

Stable behavior:

- Resolves the remote tag.
- Fetches and decodes the OCI manifest.
- Reports artifact format, layer count, chunk summary, and dataset metadata.
- Validates attached dataset metadata when present.

JSON output fields:

| Field | Meaning |
|---|---|
| `reference` | Inspected OCI reference. |
| `manifestDigest` | Resolved manifest digest. |
| `configMediaType` | OCI config media type. |
| `artifactFormat` | Human-readable artifact format when recognized. |
| `layerCount` | Manifest layer count. |
| `fileCount` | Number of files in chunk index when available. |
| `chunkCount` | Number of chunks in chunk index when available. |
| `chunkSize` | Chunk size from chunk index when available. |
| `totalSize` | Total file size from chunk index when available. |
| `datasetMetadata` | Whether metadata is attached. |
| `metadataValid` | Whether metadata passed v1 validation. |
| `metadataError` | Validation or decode error when metadata is invalid. |
| `metadataMediaType` | Metadata media type when attached. |
| `metadata` | Decoded dataset metadata when available. |

## dataset fetch

Stable flags:

| Flag | Meaning |
|---|---|
| `--overwrite` | Replace an existing destination through atomic overwrite. |
| `--skip-if-current` | With `--overwrite`, skip fetch when destination already has the requested manifest digest. |

JSON output fields:

| Field | Meaning |
|---|---|
| `reference` | Fetched OCI reference. |
| `destination` | Destination directory. |
| `volumeRef` | Manifest digest recorded in `volume-index.json`. |
| `skipped` | Whether fetch was skipped because destination was already current. |
| `datasetMetadata` | Local metadata path when metadata was written. |

## metadata init

`metadata init` writes `dataset-metadata.json` to stdout. Its output conforms to
[dataset-metadata-v1.md](dataset-metadata-v1.md).

Stable metadata flags:

| Flag | Meaning |
|---|---|
| `--ref` | Artifact reference recorded in metadata. |
| `--name` | Dataset machine name. |
| `--display-name` | Human-readable name. |
| `--description` | Human-readable description. |
| `--kind` | Dataset kind. |
| `--source-uri` | Upstream source URI. |
| `--organism` | Organism name. |
| `--taxonomy-id` | Taxonomy identifier. |
| `--reference` | Reference build name. |
| `--reference-version` | Reference build version. |
| `--reference-alias` | Comma-separated aliases. |
| `--data-type` | Comma-separated data types. |
| `--format` | Comma-separated file formats. |
| `--tool` | Comma-separated compatible tools. |
| `--tag-label` | Comma-separated catalog tags. |
| `--license` | Dataset license label. |
| `--created-by` | Metadata producer. |
| `--validation-status` | Validation status. |

## Compatibility Policy

Within v1:

- Existing stable commands should remain available.
- Stable flags should not be removed.
- JSON fields should not be removed or repurposed.
- Text output may be improved for humans.
- New optional flags and JSON fields may be added.
- Breaking CLI changes should wait for v2 unless they fix a safety issue.
