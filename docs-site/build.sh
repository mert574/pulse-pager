#!/usr/bin/env bash
# Assemble the static docs site.
#
# The only build step is copying the OpenAPI spec next to the HTML so the API
# reference (api.html, which loads ./openapi.yaml via Redoc) always matches the
# contract. Re-running this is how the reference stays in sync; CI runs it on
# every push so the published reference cannot drift from api/openapi/v1.yaml.
#
# No network is needed to build. The Redoc CDN script is only fetched in the
# browser at view time.
set -euo pipefail

# Repo root is the parent of this script's directory, so the target works from
# anywhere (Makefile, CI, or a manual run).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

SPEC_SRC="${ROOT_DIR}/api/openapi/v1.yaml"
SPEC_DST="${SCRIPT_DIR}/openapi.yaml"

if [ ! -f "${SPEC_SRC}" ]; then
  echo "error: spec not found at ${SPEC_SRC}" >&2
  exit 1
fi

cp "${SPEC_SRC}" "${SPEC_DST}"
echo "copied $(basename "${SPEC_SRC}") -> docs-site/openapi.yaml"
echo "docs site ready in docs-site/ (serve it statically; open index.html)"
