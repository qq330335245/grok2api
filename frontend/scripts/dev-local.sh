#!/usr/bin/env bash
# grok2api admin UI HMR against local runtime API (:18000 by default).
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ ! -x node_modules/.bin/vite ]]; then
  echo "missing node_modules/.bin/vite — install frontend deps first (once)" >&2
  exit 1
fi
API="${VITE_DEV_API_TARGET:-http://127.0.0.1:18000}"
if ! curl -fsS -m 2 "$API/healthz" >/dev/null 2>&1; then
  echo "warning: $API/healthz not reachable — start local grok2api first" >&2
fi
echo "grok2api frontend dev → http://127.0.0.1:5173  (API proxy → $API)"
export VITE_DEV_API_TARGET="$API"
exec npm run dev -- --host 0.0.0.0 --port 5173
