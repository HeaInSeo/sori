# sorictl Quickstart

This guide is for users who want to publish a reference dataset as an OCI
artifact, record human- and machine-readable metadata, inspect it, and fetch it
back from a registry.

`sorictl` is registry-neutral. The same command shape works for GHCR, Harbor,
local registries, and other OCI registries.

## 1. Install sorictl

From this repository:

```bash
go install ./cmd/sorictl
```

Or build a local binary:

```bash
make build-sorictl
./bin/sorictl help
```

Release builds are attached to GitHub Releases starting with tags that match
`v*`.

## 2. Choose a Registry Reference

Use this shape:

```text
REGISTRY/REPOSITORY:TAG
```

Examples:

```text
ghcr.io/OWNER/references:grch38-bwa-v1
harbor.example.com/bio/refdata:grch38-bwa-v1
localhost:5000/bio/refdata:tiny-v1
```

For GHCR, `OWNER` is your GitHub user or organization. For Harbor,
`REPOSITORY` usually includes the Harbor project name.

## 3. Configure Auth

The easiest path is environment variables:

```bash
export SORI_REGISTRY_USERNAME="YOUR_USER"
export SORI_REGISTRY_TOKEN="YOUR_TOKEN_OR_PASSWORD"
```

You can also point to a different secret variable:

```bash
export GHCR_TOKEN="YOUR_GHCR_PAT"
sorictl dataset inspect ghcr.io/OWNER/references:grch38-bwa-v1 \
  --username YOUR_USER \
  --password-env GHCR_TOKEN
```

For a plain HTTP test registry, add `--plain-http`. For a registry with a
private CA, prefer `--ca-file /path/to/ca.pem` over `--insecure-tls`.

## 4. Prepare a Small First Dataset

Start with a small directory before publishing large genomics references:

```bash
mkdir -p ./example-ref
printf ">chrTest\nACGTACGT\n" > ./example-ref/chrTest.fa
```

## 5. Create Metadata

Create `dataset-metadata.json`:

```bash
sorictl metadata init \
  --ref ghcr.io/OWNER/references:example-ref-v1 \
  --name example-ref \
  --display-name "Example Reference" \
  --description "Small reference dataset used to test sori publishing." \
  --kind reference_genome \
  --organism "synthetic construct" \
  --reference chrTest \
  --reference-version v1 \
  --format FASTA \
  --tool bwa,bwa-mem2 \
  --validation-status unverified \
  > dataset-metadata.json
```

This file is attached to the OCI artifact. Humans can read it, and systems can
parse it through the `sori.dataset.metadata.v1` schema.

## 6. Push the Dataset

```bash
sorictl dataset push ./example-ref \
  --ref ghcr.io/OWNER/references:example-ref-v1 \
  --metadata dataset-metadata.json
```

The CLI packages data with the chunked CAS format by default. Small datasets
work normally, and large datasets avoid loading the whole dataset into memory.

## 7. Inspect the Remote Artifact

```bash
sorictl dataset inspect ghcr.io/OWNER/references:example-ref-v1
```

Expected output includes:

```text
Dataset artifact
  reference: ghcr.io/OWNER/references:example-ref-v1
  format:    chunked-cas
  metadata:  attached
```

Use JSON when another system will consume the output:

```bash
sorictl dataset inspect ghcr.io/OWNER/references:example-ref-v1 --output json
```

## 8. Fetch It Back

```bash
sorictl dataset fetch \
  ghcr.io/OWNER/references:example-ref-v1 \
  ./fetched-example-ref
```

To update an existing destination atomically:

```bash
sorictl dataset fetch \
  ghcr.io/OWNER/references:example-ref-v1 \
  ./fetched-example-ref \
  --overwrite \
  --skip-if-current
```

## Harbor Example

```bash
export SORI_REGISTRY_USERNAME="admin"
export SORI_REGISTRY_TOKEN="YOUR_HARBOR_PASSWORD_OR_TOKEN"

sorictl metadata init \
  --ref harbor.example.com/bio/refdata:example-ref-v1 \
  --name example-ref \
  --display-name "Example Reference" \
  --description "Small Harbor publishing test." \
  > dataset-metadata.json

sorictl dataset push ./example-ref \
  --ref harbor.example.com/bio/refdata:example-ref-v1 \
  --metadata dataset-metadata.json

sorictl dataset inspect harbor.example.com/bio/refdata:example-ref-v1
```

For HTTP-only lab Harbor instances, add `--plain-http` to `push`, `inspect`,
and `fetch`.

## Troubleshooting

- `registry is required`: pass `--ref REGISTRY/REPOSITORY:TAG`.
- `metadata is required`: pass `--metadata dataset-metadata.json`, or provide
  enough metadata flags on `dataset push` to generate metadata automatically.
- `unauthorized` or `denied`: check `SORI_REGISTRY_USERNAME`,
  `SORI_REGISTRY_TOKEN`, and repository push/pull permissions.
- TLS errors: use a trusted registry certificate or pass `--ca-file`.
- Fetching over an existing directory fails: use `--overwrite` for atomic
  replacement.

## Production Notes

Use stable, explicit tags for references, such as `grch38-bwa-v1` or
`gencode-v44-v1`. Keep the source URI, reference version, compatible tools, and
validation status in metadata. For large references, test with a small fixture
first, then publish the full dataset after registry auth and TLS behavior are
confirmed.
