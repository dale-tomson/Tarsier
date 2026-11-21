#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
OUTPUT_DIR="${REPO_ROOT}/wiki/images"

mkdir -p "${OUTPUT_DIR}"

echo "Capturing Tarsier dark-theme screenshots into ${OUTPUT_DIR}..."

docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  -v "${REPO_ROOT}/scripts/screenshots:/work" \
  -v "${OUTPUT_DIR}:/output" \
  -w /work \
  -e TARSIER_URL="${TARSIER_URL:-http://host.docker.internal:2090}" \
  -e TARSIER_USER="${TARSIER_USER:-admin}" \
  -e TARSIER_PASSWORD="${TARSIER_PASSWORD:-admin}" \
  mcr.microsoft.com/playwright:jammy \
  node capture.js
