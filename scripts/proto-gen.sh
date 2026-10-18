#!/usr/bin/env bash
# Regenerates the Go code for api/sori/ops/v1 with the pinned toolchain and
# rewrites api/sori/ops/v1/GENERATED.provenance. The output has no timestamps, so a rerun on an
# unchanged tree is byte-identical; CI reruns it and fails on any diff or new
# untracked file.
#
# The .proto files are the restored D01.4-r3 contract with the r4 comment-only
# patch (accepted through D01.4-r7). They are a restoration of accepted source,
# not a released wire baseline.
set -euo pipefail

BUF_VERSION="1.73.0"
PROTOC_GEN_GO_VERSION="v1.36.11"
PROTOC_GEN_GO_GRPC_VERSION="1.6.1"

cd "$(dirname "$0")/.."

check_version() {
	local name="$1" want="$2" got
	got="$("$name" --version 2>&1 | awk '{print $NF}')"
	if [ "$got" != "$want" ]; then
		echo "proto-gen: $name version $got, want $want" >&2
		exit 1
	fi
}
check_version buf "$BUF_VERSION"
check_version protoc-gen-go "$PROTOC_GEN_GO_VERSION"
check_version protoc-gen-go-grpc "$PROTOC_GEN_GO_GRPC_VERSION"

buf build
buf lint
buf format -d --exit-code
buf generate

# sha256sum lines ("<hex>  <path>"), paths relative to api/, sorted by path.
hash_lines() {
	(cd api && LC_ALL=C find sori -type f -name "$1" | LC_ALL=C sort | xargs sha256sum)
}
json_map() {
	awk 'BEGIN { sep = "" } { printf "%s\n    \"%s\": \"%s\"", sep, $2, $1; sep = "," }'
}

proto_lines="$(hash_lines '*.proto')"
set_digest="$(printf '%s\n' "$proto_lines" | sha256sum | awk '{print $1}')"

# GENERATED.provenance (D01.4-r2 §3): schemaVersion, protoSetDigest,
# sourceRevision, bufVersion, plugins; no generation time. sourceRevision names
# the accepted contract source; a commit SHA cannot be recorded inside the
# commit it belongs to.
{
	printf '{\n'
	printf '  "schemaVersion": 1,\n'
	printf '  "protoSetDigest": "%s",\n' "$set_digest"
	printf '  "protoSetDigestRule": "cd api && sha256sum sori/ops/v1/*.proto | LC_ALL=C sort -k2 | sha256sum",\n'
	printf '  "sourceRevision": "D01.4-r3 proto blocks + r4 comment-only patch, unchanged through D01.4-r7; contract_revision 1; not an accepted wire baseline",\n'
	printf '  "bufVersion": "%s",\n' "$BUF_VERSION"
	printf '  "plugins": [\n'
	printf '    {"name": "protoc-gen-go", "version": "%s", "opt": "paths=source_relative"},\n' "$PROTOC_GEN_GO_VERSION"
	printf '    {"name": "protoc-gen-go-grpc", "version": "%s", "opt": "paths=source_relative"}\n' "$PROTOC_GEN_GO_GRPC_VERSION"
	printf '  ],\n'
	printf '  "proto": {'
	printf '%s\n' "$proto_lines" | json_map
	printf '\n  },\n'
	printf '  "generated": {'
	hash_lines '*.pb.go' | json_map
	printf '\n  }\n'
	printf '}\n'
} >api/sori/ops/v1/GENERATED.provenance
