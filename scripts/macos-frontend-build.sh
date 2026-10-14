#!/bin/bash
set -euo pipefail
repo_dir="$(cd "$(dirname "$0")/.." && pwd)"
project_dir="$(dirname "$repo_dir")"
flutter_bin="${FLUTTER_BIN:-$project_dir/toolchains/flutter/bin/flutter}"
if [[ ! -x "$flutter_bin" ]]; then flutter_bin="$(command -v flutter)"; fi
go_bin="${GO_BIN:-$project_dir/toolchains/go/bin/go}"
if [[ ! -x "$flutter_bin" ]]; then echo "Flutter SDK missing: $flutter_bin" >&2; exit 1; fi
if [[ ! -x "$go_bin" ]]; then go_bin="$(command -v go)"; fi
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
source_app="$project_dir/artifacts/baseline/Gopeed.app"
core_dir="$project_dir/artifacts/build/media-core"
core_version="${GOPEED_CORE_VERSION:-1.9.3-kownload.1}"
build_number="${GOPEED_BUILD_NUMBER:-6}"
mkdir -p "$core_dir"
export CGO_ENABLED=1
export PATH="$(dirname "$go_bin"):$PATH"
export MACOSX_DEPLOYMENT_TARGET=12.0
"$go_bin" build -tags nosqlite -ldflags="-w -s -X github.com/GopeedLab/gopeed/pkg/base.Version=$core_version" -buildmode=c-shared -o "$core_dir/libgopeed.dylib" github.com/GopeedLab/gopeed/bind/desktop
"$go_bin" build -ldflags="-w -s" -o "$core_dir/host" github.com/GopeedLab/gopeed/cmd/host
codesign --force --sign - "$core_dir/libgopeed.dylib"
mkdir -p "$repo_dir/ui/flutter/macos/Frameworks" "$repo_dir/ui/flutter/assets/exec"
cp "$core_dir/libgopeed.dylib" "$repo_dir/ui/flutter/macos/Frameworks/"
cp "$core_dir/host" "$repo_dir/ui/flutter/assets/exec/host"
if [[ -f "$source_app/Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/updater" ]]; then
  cp "$source_app/Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/updater" "$repo_dir/ui/flutter/assets/exec/updater"
else
  "$go_bin" build -ldflags="-w -s" -o "$repo_dir/ui/flutter/assets/exec/updater" github.com/GopeedLab/gopeed/cmd/updater
fi
cd "$repo_dir/ui/flutter"
mkdir -p "$project_dir/artifacts/build"
cat > "$project_dir/artifacts/build/macos-local.xcconfig" <<'EOF'
MACOSX_DEPLOYMENT_TARGET = 12.0
ARCHS = arm64
EOF
export XCODE_XCCONFIG_FILE="$project_dir/artifacts/build/macos-local.xcconfig"
"$flutter_bin" pub get
"$flutter_bin" build macos --release --no-pub --build-name=1.9.3 --build-number="$build_number"
release_app="$repo_dir/ui/flutter/build/macos/Build/Products/Release/Gopeed.app"
# Restore the newly built core after Xcode's copy/sign phases, then reseal.
cp "$core_dir/libgopeed.dylib" "$release_app/Contents/Frameworks/"
cp "$core_dir/host" "$release_app/Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/host"
cp "$repo_dir/ui/flutter/assets/exec/updater" "$release_app/Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/updater"
mkdir -p "$release_app/Contents/Resources/third_party"
cp -R "$repo_dir/third_party/linkswift" "$repo_dir/third_party/openlist" "$release_app/Contents/Resources/third_party/"
codesign --force --sign - --entitlements macos/Runner/Release.entitlements "$release_app"
codesign --verify --deep --strict "$release_app"
if [[ -f "$project_dir/artifacts/baseline/installed-app.json" ]]; then
  python3 "$repo_dir/scripts/verify-macos-backend.py" "$project_dir/artifacts/baseline/installed-app.json" "$release_app" --media-core "$core_dir"
fi
