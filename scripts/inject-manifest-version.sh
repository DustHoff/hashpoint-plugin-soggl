#!/bin/sh
# Rewrite the `version = "..."` field in manifest.toml with the value passed
# as $1 and write the result to manifest.toml.versioned (in the repo root,
# NOT under dist/ — GoReleaser's "ensuring distribution directory" pipe
# runs after before-hooks and errors out if dist/ is non-empty even with
# --clean). The archive references this sidecar file via `archives.files`.
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <version>" >&2
  exit 2
fi

version="$1"
sed "s/^version *= *\"[^\"]*\"/version = \"${version}\"/" manifest.toml > manifest.toml.versioned
echo "wrote manifest.toml.versioned with version=${version}"
