#!/usr/bin/env bash
set -u

repo_root="$(cd "$(dirname "$0")" && pwd)"
cd "$repo_root"

# Auth requires a signing key even during local development; callers can
# override this predictable, development-only value through their environment.
export JWT_SECRET="${JWT_SECRET:-thecompany-local-development-jwt-secret}"
if [[ ${#JWT_SECRET} -lt 32 ]]; then
  echo "JWT_SECRET must contain at least 32 characters." >&2
  exit 1
fi

if [[ ! -x frontend/node_modules/.bin/vite ]]; then
  echo "Frontend dependencies are missing. Run: npm --prefix frontend install" >&2
  exit 1
fi

if ! command -v air >/dev/null 2>&1; then
  echo "Air is missing. Install it before starting development." >&2
  exit 1
fi

(cd frontend && exec ./node_modules/.bin/vite --host 127.0.0.1) &
vite_pid=$!
air -c .air.toml &
air_pid=$!

# Keep both development servers under this script so stopping it also stops
# Vite and Air; Air in turn stops the Go binary it launched.
cleanup() {
  trap - EXIT INT TERM
  kill "$vite_pid" "$air_pid" 2>/dev/null || true
  wait "$vite_pid" 2>/dev/null || true
  wait "$air_pid" 2>/dev/null || true
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
wait -n "$vite_pid" "$air_pid"
status=$?
exit "$status"
