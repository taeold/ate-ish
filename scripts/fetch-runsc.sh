#!/usr/bin/env bash
# Fetches a pinned gVisor release and stages it in the full release layout
# (runsc plus its gvisor-bin/ sidecars) that ateworker-gvisor demanded in
# stage 0: runsc looks for gvisor_sentry and friends next to itself.
#
#   ./scripts/fetch-runsc.sh [DEST]   DEST defaults to bin/runsc
set -euo pipefail

VERSION=${RUNSC_VERSION:-release-20261019.0}
DEST=${1:-bin/runsc}

if [[ -x "$DEST/runsc" ]] && "$DEST/runsc" --version 2>&1 | grep -q "$VERSION"; then
  echo "runsc $VERSION already staged at $DEST"
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if command -v gh >/dev/null 2>&1; then
  gh release download -R google/gvisor "$VERSION" -p 'gvisor-x86_64.tar.zstd' -D "$work"
else
  # gh's own release asset URL; the GCS bucket URLs 404 (found in stage 0).
  curl -fsSL -o "$work/gvisor-x86_64.tar.zstd" \
    "https://github.com/google/gvisor/releases/download/$VERSION/gvisor-x86_64.tar.zstd"
fi

mkdir -p "$DEST"
tar --zstd -xf "$work/gvisor-x86_64.tar.zstd" -C "$DEST"
"$DEST/runsc" --version
echo "staged runsc $VERSION at $DEST"
