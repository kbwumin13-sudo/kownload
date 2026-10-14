"""Build the arm64 Swift host with CLT, reusing pinned plugin binaries.

Only plugin interface modules are emitted. Plugin implementations and the Go
backend remain those of the archived 1.9.3 application.
"""
import concurrent.futures
import json
import os
import pathlib
import plistlib
import shlex
import shutil
import subprocess

REPO = pathlib.Path(__file__).resolve().parents[1]
PROJECT = REPO.parent
FLUTTER = PROJECT / 'toolchains/flutter'
UI = REPO / 'ui/flutter'
OUT = pathlib.Path(os.environ.get('GOPEED_NATIVE_OUT', PROJECT / 'artifacts/native-build')).resolve()
APP = pathlib.Path(os.environ.get('GOPEED_NATIVE_APP', PROJECT / 'artifacts/release/Gopeed.app')).resolve()
BASELINE = pathlib.Path(os.environ.get('GOPEED_NATIVE_BASELINE', PROJECT / 'artifacts/baseline/Gopeed.app')).resolve()
FRONTEND = pathlib.Path(os.environ.get('GOPEED_FRONTEND_OUT', UI / 'build/frontend-preview')).resolve()
ENGINE = FLUTTER / 'bin/cache/artifacts/engine/darwin-x64-release/FlutterMacOS.xcframework/macos-arm64_x86_64'
MODULES = OUT / 'modules'
MODULES.mkdir(parents=True, exist_ok=True)
(OUT / 'tmp').mkdir(exist_ok=True)
os.environ['TMPDIR'] = str(OUT / 'tmp')
os.environ['CLANG_MODULE_CACHE_PATH'] = str(OUT / 'cache')
SDK = subprocess.check_output(['xcrun', '--show-sdk-path'], text=True).strip()
TOOLS = pathlib.Path(subprocess.check_output(['xcrun', '--find', 'swiftc'], text=True).strip()).parent


def run(name, command, cwd=UI):
    (OUT / f'{name}.command').write_text(shlex.join(map(str, command)) + '\n')
    result = subprocess.run(list(map(str, command)), cwd=cwd, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    (OUT / f'{name}.log').write_text(result.stdout)
    if result.returncode:
        raise RuntimeError(f'{name} failed ({result.returncode}):\n{result.stdout[-5000:]}')
    print(f'{name}: passed', flush=True)


def engine_version(root):
    with (root / 'FlutterMacOS.framework/Resources/Info.plist').open('rb') as f:
        return plistlib.load(f)['FlutterEngine']


if engine_version(ENGINE) != engine_version(BASELINE / 'Contents/Frameworks'):
    raise RuntimeError('Flutter engine mismatch: use the archived application SDK version')
run('frontend', [FLUTTER / 'bin/flutter', 'assemble', f'--output={FRONTEND}',
                 '-dTargetPlatform=darwin', '-dBuildMode=release', '-dDarwinArchs=arm64',
                 '-dTargetFile=lib/main.dart', 'release_macos_bundle_flutter_assets'])

plugins = json.loads((UI / '.flutter-plugins-dependencies').read_text())['plugins']['macos']
native = [plugin for plugin in plugins if plugin['native_build']]
base = [TOOLS / 'swift-frontend', '-sdk', SDK, '-target', 'arm64-apple-macos11.0',
        '-swift-version', '5', '-module-cache-path', OUT / 'cache', '-F', ENGINE, '-I', MODULES]


def emit(plugin):
    name = plugin['name']
    root = pathlib.Path(plugin['path']) / ('darwin' if plugin.get('shared_darwin_source') else 'macos')
    if name == 'hotkey_manager_macos':
        # This is the actual Objective-C registration class exported by the
        # archived framework. No replacement method implementation is emitted.
        module = MODULES / name
        module.mkdir(exist_ok=True)
        (module / 'hotkey.h').write_text(
            '#import <FlutterMacOS/FlutterMacOS.h>\n'
            '__attribute__((objc_runtime_name("_TtC20hotkey_manager_macos24HotkeyManagerMacosPlugin")))\n'
            '@interface HotkeyManagerMacosPlugin : NSObject <FlutterPlugin>\n@end\n')
        (module / 'module.modulemap').write_text(
            f'module {name} {{ header "hotkey.h" export * link framework "{name}" }}\n')
        return
    sources = [p for p in root.rglob('*.swift') if p.name != 'Package.swift' and not any(part.endswith('Tests') for part in p.parts)]
    if not sources:
        module = MODULES / name
        module.mkdir(exist_ok=True)
        headers = list(root.rglob('*.h'))
        (module / 'module.modulemap').write_text(f'module {name} {{\n' +
            ''.join(f'header "{p}"\n' for p in headers) + f'export *\nlink framework "{name}"\n}}\n')
        return
    if name == 'tray_manager':
        target = OUT / 'tray-source'
        target.mkdir(exist_ok=True)
        copies = []
        for source in sources:
            dest = target / source.name
            dest.write_text('import Cocoa\n' + source.read_text())
            copies.append(dest)
        sources = copies
    run(f'module-{name}', base + ['-emit-module', '-module-name', name,
        '-emit-module-path', MODULES / f'{name}.swiftmodule'] + sources)


with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
    list(pool.map(emit, native))

runner = [UI / 'macos/Runner/MainFlutterWindow.swift', UI / 'macos/Runner/MouseEventRouter.swift', UI / 'macos/Runner/AppDelegate.swift',
          UI / 'macos/Flutter/GeneratedPluginRegistrant.swift']
command = [TOOLS / 'swiftc', '-disallow-use-new-driver'] + base[1:] + [
    '-O', '-parse-as-library', '-module-name', 'Gopeed', '-o', OUT / 'Gopeed'] + runner + [
    '-F', BASELINE / 'Contents/Frameworks', '-L', TOOLS.parent / 'lib/swift/macosx',
    '-Xlinker', '-rpath', '-Xlinker', '@executable_path/../Frameworks',
    '-Xlinker', '-rpath', '-Xlinker', '/usr/lib/swift']
for name in [p['name'] for p in native] + ['HotKey', 'FlutterMacOS', 'Cocoa']:
    command += ['-framework', name]
run('swift-runner', command)
APP.parent.mkdir(parents=True, exist_ok=True)
if APP.exists():
    shutil.rmtree(APP)
subprocess.run(['ditto', str(BASELINE), str(APP)], check=True)
shutil.copy2(OUT / 'Gopeed', APP / 'Contents/MacOS/Gopeed')
framework = APP / 'Contents/Frameworks/App.framework'
shutil.rmtree(framework)
subprocess.run(['ditto', str(FRONTEND / 'App.framework'), str(framework)], check=True)
for helper in ['host', 'updater']:
    relative = f'Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/{helper}'
    shutil.copy2(BASELINE / relative, APP / relative)
info_path = APP / 'Contents/Info.plist'
with info_path.open('rb') as f:
    info = plistlib.load(f)
info['CFBundleVersion'] = os.environ.get('GOPEED_BUILD_NUMBER', '6')
info['CFBundleName'] = 'kownload'
info['CFBundleDisplayName'] = 'kownload'
with info_path.open('wb') as f:
    plistlib.dump(info, f)
run('sign-frontend', ['codesign', '--force', '--sign', '-', framework])
run('sign-app', ['codesign', '--force', '--sign', '-', '--entitlements',
                UI / 'macos/Runner/Release.entitlements', APP])
run('verify-signature', ['codesign', '--verify', '--deep', '--strict', APP])
# The preview must retain exactly the selected source application's backend.
import hashlib
for relative in ['Contents/Frameworks/libgopeed.dylib',
                 'Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/host',
                 'Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/updater']:
    original = BASELINE / relative
    generated = APP / relative
    if not original.is_file() or not generated.is_file() or hashlib.sha256(original.read_bytes()).digest() != hashlib.sha256(generated.read_bytes()).digest():
        raise RuntimeError(f'Backend changed in frontend-only build: {relative}')
print('verify-backend: unchanged from selected source app', flush=True)
print(APP, flush=True)
