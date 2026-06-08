#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/sorictl-registry-smoke.sh small
  scripts/sorictl-registry-smoke.sh real

Required:
  SORI_SMOKE_REF=REGISTRY/REPOSITORY:TAG

Optional:
  SORICTL_BIN=./bin/sorictl
  SORI_REAL_DATASET_DIR=/path/to/dataset   # required for "real"
  SORI_SMOKE_WORKDIR=/tmp/sori-smoke
  SORI_REGISTRY_USERNAME=...
  SORI_REGISTRY_TOKEN=... or SORI_REGISTRY_PASSWORD=...
  SORI_SMOKE_EXTRA_FLAGS="--plain-http"    # for lab registries

The script validates:
  metadata init -> dataset push -> dataset inspect -> dataset fetch
  source/fetch byte equality
  overwrite + skip-if-current
EOF
}

mode="${1:-}"
if [[ "$mode" == "-h" || "$mode" == "--help" || "$mode" == "" ]]; then
  usage
  exit 0
fi
if [[ "$mode" != "small" && "$mode" != "real" ]]; then
  usage >&2
  exit 2
fi

ref="${SORI_SMOKE_REF:-}"
if [[ "$ref" != */*:* ]]; then
  echo "SORI_SMOKE_REF must be REGISTRY/REPOSITORY:TAG, got: ${ref:-<empty>}" >&2
  exit 2
fi

sorictl="${SORICTL_BIN:-./bin/sorictl}"
if [[ ! -x "$sorictl" ]]; then
  echo "sorictl binary is not executable: $sorictl" >&2
  echo "Run: make build-sorictl" >&2
  exit 2
fi

workdir="${SORI_SMOKE_WORKDIR:-}"
if [[ "$workdir" == "" ]]; then
  workdir="$(mktemp -d "${TMPDIR:-/tmp}/sori-smoke.XXXXXX")"
else
  rm -rf "$workdir"
  mkdir -p "$workdir"
fi
trap 'rm -rf "$workdir"' EXIT

extra_flags=()
if [[ "${SORI_SMOKE_EXTRA_FLAGS:-}" != "" ]]; then
  # shellcheck disable=SC2206
  extra_flags=(${SORI_SMOKE_EXTRA_FLAGS})
fi

src="$workdir/source"
case "$mode" in
  small)
    mkdir -p "$src/subdir"
    printf ">chrSmoke\nACGTACGTACGT\n" > "$src/chrSmoke.fa"
    printf "feature\t1\t12\n" > "$src/subdir/annotation.tsv"
    ;;
  real)
    real_src="${SORI_REAL_DATASET_DIR:-}"
    if [[ "$real_src" == "" || ! -d "$real_src" ]]; then
      echo "SORI_REAL_DATASET_DIR must point to a directory for real dataset smoke" >&2
      exit 2
    fi
    src="$real_src"
    ;;
esac

metadata="$workdir/dataset-metadata.json"
inspect_json="$workdir/inspect.json"
fetch_dir="$workdir/fetched"

"$sorictl" metadata init \
  --ref "$ref" \
  --name "sori-${mode}-smoke" \
  --display-name "sori ${mode} smoke" \
  --description "sori ${mode} registry smoke fixture." \
  --kind reference_genome \
  --reference "smoke" \
  --reference-version "v1" \
  --format "FASTA" \
  --tool "sorictl" \
  --validation-status "unverified" \
  > "$metadata"

"$sorictl" dataset push "$src" \
  --ref "$ref" \
  --metadata "$metadata" \
  "${extra_flags[@]}"

"$sorictl" dataset inspect "$ref" \
  --output json \
  "${extra_flags[@]}" \
  > "$inspect_json"

grep -q '"artifactFormat": "chunked-cas"' "$inspect_json"
grep -q '"datasetMetadata": true' "$inspect_json"
grep -q '"metadataValid": true' "$inspect_json"

"$sorictl" dataset fetch "$ref" "$fetch_dir" "${extra_flags[@]}"

diff -ru -x volume-index.json -x .sori "$src" "$fetch_dir"
test -f "$fetch_dir/volume-index.json"
test -f "$fetch_dir/.sori/dataset-metadata.json"

"$sorictl" dataset fetch "$ref" "$fetch_dir" \
  --overwrite \
  --skip-if-current \
  "${extra_flags[@]}"

echo "sorictl ${mode} smoke passed: $ref"
