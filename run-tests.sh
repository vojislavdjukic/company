#!/usr/bin/env bash

# Run each test package from the backend module and report every result.
set -uo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR/backend"

test_packages=(
	"./tests/unit"
	"./tests/integration"
)
results=()
failed=0

for package in "${test_packages[@]}"; do
	printf '\n=== Running %s ===\n' "$package"
	if go test -v "$package"; then
		results+=("PASS $package")
	else
		results+=("FAIL $package")
		failed=1
	fi
done

printf '\n=== Test results ===\n'
for result in "${results[@]}"; do
	printf '%s\n' "$result"
done

if (( failed != 0 )); then
	exit 1
fi
