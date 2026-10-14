#!/usr/bin/env python3
"""Typecheck the production host and run AppKit tests with existing local modules.

No Flutter/Dart build, application installation, production engine or app launch.
"""
from pathlib import Path
import platform
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[4]
PRODUCTS = ROOT / 'ui/flutter/build/macos/Build/Products/Release'
ENGINE = ROOT.parent / 'toolchains/flutter/bin/cache/artifacts/engine/darwin-x64-release/FlutterMacOS.xcframework/macos-arm64_x86_64'
RUNNER = ROOT / 'ui/flutter/macos/Runner'
TESTS = Path(__file__).resolve().parent


def run(command):
    subprocess.run(command, check=True)


with tempfile.TemporaryDirectory(prefix='kownload-panel-tests-') as temp:
    framework_flags = ['-F', str(ENGINE)]
    for parent in sorted({p.parent for p in PRODUCTS.glob('*/*.framework')}):
        framework_flags += ['-F', str(parent)]
    common = ['xcrun', 'swiftc', '-swift-version', '5', '-target', f'{platform.machine()}-apple-macos12.0',
              '-module-cache-path', str(Path(temp) / 'module-cache')] + framework_flags
    run(common + ['-typecheck', str(RUNNER / 'MainFlutterWindow.swift'), str(RUNNER / 'MouseEventRouter.swift'),
                  str(RUNNER / 'AppDelegate.swift'), str(ROOT / 'ui/flutter/macos/Flutter/GeneratedPluginRegistrant.swift')])
    print('PASS: production Swift host typecheck (real Flutter and generated plugin modules)', flush=True)
    binary = Path(temp) / 'panel-host-test'
    run(common + [str(RUNNER / 'MainFlutterWindow.swift'), str(RUNNER / 'MouseEventRouter.swift'),
                  str(TESTS / 'panel_host_test.swift'), '-o', str(binary),
                  '-framework', 'FlutterMacOS', '-framework', 'window_manager',
                  '-Xlinker', '-rpath', '-Xlinker', str(ENGINE),
                  '-Xlinker', '-rpath', '-Xlinker', str(PRODUCTS / 'Gopeed.app/Contents/Frameworks')])
    run([str(binary)])
