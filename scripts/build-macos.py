#!/usr/bin/env python3
"""Build and package a single Git commit. Never reads an installed/baseline app."""
import argparse
import gzip
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import tarfile
import tempfile
import zipfile

REPO = pathlib.Path(__file__).resolve().parents[1]


def run(*args, cwd=None, env=None):
    print('+', pathlib.Path(str(args[0])).name, *map(str, args[1:3]), flush=True)
    subprocess.run(list(map(str, args)), cwd=cwd, env=env, check=True)


def output(*args, cwd=None):
    return subprocess.check_output(list(map(str, args)), cwd=cwd, text=True).strip()


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--ref', default='HEAD')
    p.add_argument('--output', type=pathlib.Path, required=True)
    p.add_argument('--preview', action='store_true')
    p.add_argument('--work-dir', type=pathlib.Path)
    a = p.parse_args()
    # A clean tree makes the running build control and archived inputs reviewable.
    if output('git', 'status', '--porcelain', cwd=REPO):
        raise RuntimeError('Commit changes before packaging a source revision')
    commit = output('git', 'rev-parse', '--verify', a.ref + '^{commit}', cwd=REPO)
    if commit != output('git', 'rev-parse', 'HEAD', cwd=REPO):
        raise RuntimeError('Check out the selected commit before building')
    destination = a.output.resolve()
    destination.mkdir(parents=True, exist_ok=True)
    work = a.work_dir.resolve() if a.work_dir else pathlib.Path(tempfile.mkdtemp(prefix='kownload-build-'))
    work.mkdir(parents=True, exist_ok=True)
    source = work / 'source'
    if source.exists():
        raise RuntimeError('Build source directory already exists; use an empty work directory')
    source.mkdir()
    archive = subprocess.check_output(['git', 'archive', commit], cwd=REPO)
    with tarfile.open(fileobj=io.BytesIO(archive)) as t:
        t.extractall(source, filter='data')
    config = json.loads((source / 'config/release.json').read_text())
    version = config['appVersion']
    revision = config['revision']
    build = config['buildNumber']
    release = f'{version}-kownload.{revision}'
    if not all(isinstance(v, int) and v > 0 for v in [revision, build]):
        raise RuntimeError('Invalid release metadata')
    project = pathlib.Path(os.environ.get('KOWNLOAD_PROJECT_DIR', REPO))
    go = os.environ.get('GO_BIN') or shutil.which('go')
    flutter_bin = os.environ.get('FLUTTER_BIN') or str(project / 'toolchains/flutter/bin/flutter')
    flutter = pathlib.Path(flutter_bin).resolve().parents[1]
    if not go or not pathlib.Path(flutter_bin).is_file():
        raise RuntimeError('Set GO_BIN and FLUTTER_BIN to the pinned toolchains')
    go_version = output(go, 'version').split()[2].removeprefix('go')
    flutter_meta = json.loads(output(flutter_bin, '--version', '--machine'))
    if go_version != config['goVersion'] or (flutter_meta['frameworkVersion'] != config['flutterVersion'] or flutter_meta['frameworkRevision'] != config['flutterRevision']):
        raise RuntimeError('SDK version differs from config/release.json')
    env = os.environ.copy()
    env.update({'CGO_ENABLED': '1', 'MACOSX_DEPLOYMENT_TARGET': config['minimumMacOS'],
                'COCOAPODS_DISABLE_STATS': 'true', 'FLUTTER_SUPPRESS_ANALYTICS': 'true'})
    env['PATH'] = str(pathlib.Path(go).parent) + os.pathsep + env.get('PATH', '')
    ui = source / 'ui/flutter'
    core = work / 'libgopeed.dylib'
    host = work / 'host'
    run(go, 'build', '-trimpath', '-tags', 'nosqlite', '-buildmode=c-shared',
        '-ldflags=-w -s -X github.com/GopeedLab/gopeed/pkg/base.Version=' + release,
        '-o', core, './bind/desktop', cwd=source, env=env)
    run(go, 'build', '-trimpath', '-ldflags=-w -s', '-o', host, './cmd/host', cwd=source, env=env)
    (ui / 'macos/Frameworks').mkdir(parents=True, exist_ok=True)
    (ui / 'assets/exec').mkdir(parents=True, exist_ok=True)
    shutil.copy2(core, ui / 'macos/Frameworks/libgopeed.dylib')
    shutil.copy2(host, ui / 'assets/exec/host')
    # Map compiler source/debug paths instead of mutating emitted string tables.
    home = str(pathlib.Path.home())
    xcconfig = work / 'privacy.xcconfig'
    xcconfig.write_text('\n'.join([
        'MACOSX_DEPLOYMENT_TARGET = ' + config['minimumMacOS'], 'ARCHS = arm64',
        'CODE_SIGNING_ALLOWED = NO', 'SWIFT_SERIALIZE_DEBUGGING_OPTIONS = NO',
        'OTHER_SWIFT_FLAGS = $(inherited) -debug-prefix-map ' + home + '=/build/home -file-prefix-map ' + home + '=/build/home',
        'OTHER_CFLAGS = $(inherited) -ffile-prefix-map=' + home + '=/build/home -fdebug-prefix-map=' + home + '=/build/home',
        'OTHER_CPLUSPLUSFLAGS = $(inherited) -ffile-prefix-map=' + home + '=/build/home -fdebug-prefix-map=' + home + '=/build/home',
    ]) + '\n')
    env['XCODE_XCCONFIG_FILE'] = str(xcconfig)
    run(flutter_bin, 'pub', 'get', '--enforce-lockfile', cwd=ui, env=env)
    flags = ['--release', '--no-pub', '--build-name=' + version, '--build-number=' + str(build),
             '--dart-define=KOWNLOAD_RELEASE_VERSION=' + release,
             '--split-debug-info=' + str(work / 'symbols')]
    preview_storage = work / 'preview-data'
    if a.preview:
        flags.append('--dart-define=KOWNLOAD_PREVIEW_STORAGE=' + str(preview_storage))
    run(flutter_bin, 'build', 'macos', *flags, cwd=ui, env=env)
    stage = work / 'dmg'
    stage.mkdir()
    app = stage / 'kownload.app'
    shutil.copytree(ui / 'build/macos/Build/Products/Release/Gopeed.app', app, symlinks=True)
    info_path = app / 'Contents/Info.plist'
    info = plistlib.loads(info_path.read_bytes())
    if a.preview:
        info['CFBundleIdentifier'] = 'com.gopeed.gopeed.independence-preview'
        info['CFBundleDisplayName'] = 'kownload 阶段一预览'
        info['CFBundleName'] = 'kownload 阶段一预览'
        for key in ['CFBundleURLTypes', 'CFBundleDocumentTypes', 'UTExportedTypeDeclarations']:
            info.pop(key, None)
    info_path.write_bytes(plistlib.dumps(info))
    resources = app / 'Contents/Resources'
    legal = resources / 'legal'
    notices = load('kownload_notices', source / 'scripts/collect-macos-notices.py')
    modules = notices.collect(source, legal, [core, host], go, flutter)
    helper = resources / 'browser-media-extension'
    helper.mkdir()
    for file in (source / 'browser-media-extension').iterdir():
        if file.is_file() and file.suffix in ['.js', '.json', '.css', '.html', '.md'] and file.name != 'package.json':
            shutil.copy2(file, helper / file.name)
    shutil.copy2(source / 'LICENSE', helper / 'LICENSE')
    shutil.copy2(source / 'THIRD_PARTY_NOTICES.md', helper / 'THIRD_PARTY_NOTICES.md')
    shutil.copytree(source / 'third_party', helper / 'third_party')
    source_name = f'kownload-v{release}-source.tar.gz'
    source_notice = (f'kownload {release} (build {build})\nSource commit: {commit}\n'
        f'Corresponding Source: {source_name}, distributed beside this package.\n'
        f'Repository: https://github.com/kbwumin13-sudo/kownload\n'
        f'Commit archive after publication: https://github.com/kbwumin13-sudo/kownload/archive/{commit}.tar.gz\n' +
        ('This isolated local preview is unpublished.\n' if a.preview else
         'Distribute this binary and its Corresponding Source together.\n') +
        'Publication must include the source archive, build instructions,\n'
        'dependency source locations and license notices.\n'
        'Exact Go dependency source archives: go-modules.json in Corresponding Source\n'
        '(also App Contents/Resources/legal/go-modules.json).\n'
        'Flutter dependencies: ui/flutter/pubspec.lock in Corresponding Source;\n'
        'Flutter/Dart source: official versions identified in build-manifest.json.\n')
    (legal / 'SOURCE.txt').write_text(source_notice)
    (helper / 'SOURCE.txt').write_text(source_notice)
    helper_version = json.loads((helper / 'manifest.json').read_text())['version']
    manifest = {'sourceCommit': commit, 'releaseVersion': release, 'appVersion': version,
                'buildNumber': build, 'architecture': 'arm64', 'preview': a.preview,
                'goVersion': go_version, 'flutterVersion': config['flutterVersion'],
                'flutterRevision': flutter_meta['frameworkRevision'], 'helperVersion': helper_version,
                'projectComponentsFromSourceCommit': ['Go engine', 'Native host', 'Flutter app'],
                'nativePlugins': 'rebuilt from locked third-party source',
                'automaticInstaller': False}
    (resources / 'build-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    # Remove local object/debug symbols, then ad-hoc seal copied payloads only.
    mach = []
    for file in app.rglob('*'):
        if file.is_file() and not file.is_symlink():
            with file.open('rb') as f:
                magic = f.read(4)
            if magic in [b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf', b'\xca\xfe\xba\xbe']:
                mach.append(file)
    for file in mach:
        run('xcrun', 'strip', '-S', '-x', file)
        run('codesign', '--force', '--sign', '-', file)
    for framework in sorted(app.rglob('*.framework'), key=lambda x: len(x.parts), reverse=True):
        if not framework.is_symlink():
            run('codesign', '--force', '--sign', '-', framework)
    run('codesign', '--force', '--sign', '-', '--entitlements', ui / 'macos/Runner/Release.entitlements', app)
    run('codesign', '--verify', '--deep', '--strict', app)
    scanner = load('kownload_path_scan', source / 'scripts/scan-package-paths.py')
    result = scanner.scan(app)
    (destination / 'app-path-scan.json').write_text(json.dumps(result, indent=2) + '\n')
    if result['personalPaths']:
        raise RuntimeError('Personal paths remain in candidate: see app-path-scan.json')
    for name in ['LICENSE', 'THIRD_PARTY_NOTICES.md']:
        shutil.copy2(source / name, stage / name)
    (stage / 'SOURCE.txt').write_text(source_notice)
    (stage / 'Applications').symlink_to('/Applications')
    suffix = '-preview' if a.preview else ''
    dmg = destination / f'kownload-v{release}-macos-arm64{suffix}.dmg'
    run('hdiutil', 'create', '-srcfolder', stage, '-volname', 'kownload', '-format', 'UDZO', dmg)
    run('hdiutil', 'verify', dmg)
    # Inspect the actual final image, read-only, before handing it off.
    mount = work / 'image-check'
    mount.mkdir()
    try:
        run('hdiutil', 'attach', '-readonly', '-nobrowse', '-noautoopen', '-mountpoint', mount, dmg)
        final_scan = scanner.scan(mount)
        (destination / 'dmg-path-scan.json').write_text(json.dumps(final_scan, indent=2) + '\n')
        if final_scan['personalPaths']:
            raise RuntimeError('Personal paths in final DMG')
        run('codesign', '--verify', '--deep', '--strict', mount / 'kownload.app')
    finally:
        if os.path.ismount(mount):
            run('hdiutil', 'detach', mount)
    helper_zip = destination / f'kownload-browser-helper-{helper_version}-{release}.zip'
    with zipfile.ZipFile(helper_zip, 'w', zipfile.ZIP_DEFLATED) as z:
        for file in sorted(helper.rglob('*')):
            if file.is_file():
                z.write(file, 'kownload-browser-helper/' + str(file.relative_to(helper)))
    # The tracked source archive is supplemented by exact module source archives.
    with tarfile.open(destination / source_name, 'w:gz') as bundle:
        with tarfile.open(fileobj=io.BytesIO(archive)) as tree:
            for member in tree.getmembers():
                bundle.addfile(member, tree.extractfile(member) if member.isfile() else None)
        module_data = (json.dumps(modules, indent=2) + '\n').encode()
        member = tarfile.TarInfo('go-modules.json')
        member.size = len(module_data)
        bundle.addfile(member, io.BytesIO(module_data))
        for m in modules:
            meta = json.loads(output(go, 'mod', 'download', '-json', m['module'] + '@' + m['version'], cwd=source))
            bundle.add(meta['Zip'], arcname='dependency-sources/go/' + pathlib.Path(meta['Zip']).name.replace('.zip', '') + '-' + hashlib.sha256(m['module'].encode()).hexdigest()[:12] + '.zip')
    (destination / 'go-modules.json').write_text(json.dumps(modules, indent=2) + '\n')
    (destination / 'build-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    assets = [dmg, helper_zip, destination / source_name]
    (destination / 'SHA256SUMS.txt').write_text(''.join(hashlib.file_digest(f.open('rb'), 'sha256').hexdigest() + '  ' + f.name + '\n' for f in assets))
    print('Candidate app:', app, '\nTemporary preview data:', preview_storage, '\nAssets:', destination)


if __name__ == '__main__':
    main()
