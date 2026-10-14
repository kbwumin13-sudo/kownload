#!/bin/bash
set -euo pipefail
repo_dir="$(cd "$(dirname "$0")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
xcrun swiftc "$repo_dir/ui/flutter/macos/Runner/MouseEventRouter.swift" \
  "$repo_dir/ui/flutter/macos/tests/pointer_routing_test.swift" \
  -o "$test_dir/pointer-routing-test"
"$test_dir/pointer-routing-test"
