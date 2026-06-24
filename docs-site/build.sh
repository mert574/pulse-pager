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

# Render the legal pages (terms / privacy / refund) from their templates, filling in
# the site identity. The values are kept OUT of the repo so this stays fork-friendly:
# defaults are generic placeholders, and a deployment supplies the real ones via env
# vars or an untracked docs-site/site.config (see site.config.example). The rendered
# *.html are build artifacts (gitignored), regenerated here and in CI like openapi.yaml.
if [ -f "${SCRIPT_DIR}/site.config" ]; then
  # shellcheck disable=SC1091
  . "${SCRIPT_DIR}/site.config"
fi
PROJECT_NAME="${PROJECT_NAME:-Pulse Pager}"
SELLER_NAME="${SELLER_NAME:-[seller legal name not set]}"
CONTACT_EMAIL="${CONTACT_EMAIL:-hi@pulsepager.com}"
SITE_URL="${SITE_URL:-https://pulsepager.com}"
JURISDICTION="${JURISDICTION:-[seller jurisdiction not set]}"
EFFECTIVE_DATE="${EFFECTIVE_DATE:-2026-06-24}"
# Paddle checkout page (checkout.html). The client-side token is safe in the browser
# (it is not the secret API key); env is "production" or "sandbox"; APP_URL is where
# Paddle returns the buyer after payment. Defaults keep this fork-friendly/unconfigured.
PADDLE_CLIENT_TOKEN="${PADDLE_CLIENT_TOKEN:-[paddle client token not set]}"
PADDLE_ENVIRONMENT="${PADDLE_ENVIRONMENT:-production}"
APP_URL="${APP_URL:-https://app.pulsepager.com}"

render_legal() { # $1 = page slug (terms|privacy|refund)
  local tmpl="${SCRIPT_DIR}/legal/${1}.template.html"
  local out="${SCRIPT_DIR}/${1}.html"
  # | as the sed delimiter so URLs (with /) pass through; values must not contain |.
  sed -e "s|{{PROJECT_NAME}}|${PROJECT_NAME}|g" \
      -e "s|{{SELLER_NAME}}|${SELLER_NAME}|g" \
      -e "s|{{CONTACT_EMAIL}}|${CONTACT_EMAIL}|g" \
      -e "s|{{SITE_URL}}|${SITE_URL}|g" \
      -e "s|{{JURISDICTION}}|${JURISDICTION}|g" \
      -e "s|{{EFFECTIVE_DATE}}|${EFFECTIVE_DATE}|g" \
      "${tmpl}" > "${out}"
  echo "rendered legal/${1}.template.html -> docs-site/${1}.html"
}
for page in terms privacy refund; do
  render_legal "${page}"
done

# Render the Paddle checkout page from its template, same approach as the legal pages.
sed -e "s|{{PROJECT_NAME}}|${PROJECT_NAME}|g" \
    -e "s|{{CONTACT_EMAIL}}|${CONTACT_EMAIL}|g" \
    -e "s|{{SITE_URL}}|${SITE_URL}|g" \
    -e "s|{{PADDLE_CLIENT_TOKEN}}|${PADDLE_CLIENT_TOKEN}|g" \
    -e "s|{{PADDLE_ENVIRONMENT}}|${PADDLE_ENVIRONMENT}|g" \
    -e "s|{{APP_URL}}|${APP_URL}|g" \
    "${SCRIPT_DIR}/checkout.template.html" > "${SCRIPT_DIR}/checkout.html"
echo "rendered checkout.template.html -> docs-site/checkout.html"

echo "docs site ready in docs-site/ (serve it statically; open index.html)"
if [ "${PADDLE_CLIENT_TOKEN}" = "[paddle client token not set]" ]; then
  echo "note: PADDLE_CLIENT_TOKEN is unset. Set it in docs-site/site.config (Paddle >" >&2
  echo "      Authentication > Client-side tokens) before checkout.html can run." >&2
fi
if [ "${SELLER_NAME}" = "[seller legal name not set]" ]; then
  echo "note: SELLER_NAME/JURISDICTION are unset placeholders. Set them in docs-site/site.config" >&2
  echo "      (copy site.config.example) before publishing the legal pages for Paddle." >&2
fi
