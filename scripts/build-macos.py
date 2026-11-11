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
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import zipfile

REPO = pathlib.Path(__file__).resolve().parents[1]
MCP_HELPER = 'Contents/Helpers/kownload-mcp'
MCP_SDK_VERSION = 'v1.7.0'


def private_preview_storage(home, identity):
    """Validate an existing private HOME without creating or changing paths."""
    anchor = pathlib.Path('/private/tmp')
    if (not home.is_absolute() or not home.is_relative_to(anchor) or
            home == anchor or '..' in home.parts):
        raise RuntimeError('Preview HOME must be canonical and private under /private/tmp')

    def check_directory(path):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode):
            raise RuntimeError('Preview HOME entries must be ordinary directories')
        if info.st_uid != os.getuid():
            raise RuntimeError('Preview HOME directory owner differs')
        if info.st_mode & 0o022:
            raise RuntimeError('Preview HOME directory permissions allow other writers')

    current = anchor
    for part in home.relative_to(anchor).parts:
        current = current / part
        try:
            check_directory(current)
        except OSError as error:
            raise RuntimeError('Preview HOME cannot be inspected') from error
    if home.resolve(strict=True) != home:
        raise RuntimeError('Preview HOME must be canonical without links')
    bundle_id = identity.get('bundleId')
    if not isinstance(bundle_id, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]+', bundle_id):
        raise RuntimeError('Invalid managed preview bundle identity')
    storage = home / 'Library/Application Support' / bundle_id
    directory_targets = [storage]
    for key in ['apiSocket', 'hostSocket']:
        value = identity.get(key)
        if not isinstance(value, str) or not value or '\0' in value:
            raise RuntimeError('Invalid preview socket path')
        relative = pathlib.PurePosixPath(value)
        if relative.is_absolute() or '..' in relative.parts:
            raise RuntimeError('Invalid preview socket path')
        # Darwin sockaddr_un.sun_path has 104 bytes including the trailing NUL.
        if len(str(storage / relative).encode('utf-8')) > 103:
            raise RuntimeError('Preview socket path exceeds the Darwin byte limit')
        directory_targets.append((storage / relative).parent)
    for target in directory_targets:
        current = home
        for part in target.relative_to(home).parts:
            current = current / part
            try:
                check_directory(current)
            except FileNotFoundError:
                break
    return storage


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


def mcp_version(release, commit):
    if not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise RuntimeError('MCP source must be a full Git commit')
    return release + '+' + commit


def mcp_helper(app):
    helper = app / MCP_HELPER
    for entry in [app / 'Contents', helper.parent, helper]:
        if entry.is_symlink():
            raise RuntimeError('MCP helper must use ordinary package entries')
    if not helper.is_file():
        raise RuntimeError('MCP helper is missing or is not a regular file')
    return helper


def sha256_file(file):
    with file.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def mcp_metadata(app, manifest, sdk_version):
    if sdk_version != MCP_SDK_VERSION or manifest['architecture'] != 'arm64':
        raise RuntimeError('Unexpected MCP SDK or architecture')
    return {'manifestVersion': 1, 'helperPath': MCP_HELPER,
            'version': mcp_version(manifest['releaseVersion'], manifest['sourceCommit']),
            'sourceCommit': manifest['sourceCommit'], 'readApiVersion': 1,
            'sdkVersion': sdk_version, 'architecture': 'arm64',
            'sha256': sha256_file(mcp_helper(app))}


def verify_packaged_mcp(app, manifest, execute_version=False):
    metadata = manifest.get('mcp')
    if metadata is None:
        return
    expected = mcp_metadata(app, manifest, MCP_SDK_VERSION)
    if metadata != expected:
        raise RuntimeError('MCP package metadata or final helper digest differs')
    if execute_version:
        # The caller has verified the sealed App. Never execute a stable entry
        # or a path supplied by package metadata before the fixed-path checks.
        wanted = 'kownload-mcp ' + metadata['version'] + ' read-api=1'
        if output(mcp_helper(app), '--version') != wanted:
            raise RuntimeError('MCP packaged helper version differs')


def seal_candidate(app, entitlements, manifest, mcp_sdk=None):
    # Sign all inner code first; outer signing deliberately omits --deep, so
    # it cannot mutate an already hashed Helper behind the manifest's back.
    prestripped_mcp = mcp_helper(app) if mcp_sdk is not None else None
    mach = []
    for file in app.rglob('*'):
        if file.is_file() and not file.is_symlink():
            with file.open('rb') as f:
                magic = f.read(4)
            if magic in [b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf', b'\xca\xfe\xba\xbe']:
                mach.append(file)
    for file in mach:
        # Go already linked this helper with -w -s. Apple strip rejects its
        # cgo indirect symbols on supported SDKs; signing still applies below.
        if file != prestripped_mcp:
            run('xcrun', 'strip', '-S', '-x', file)
        run('codesign', '--force', '--sign', '-', file)
    for framework in sorted(app.rglob('*.framework'), key=lambda x: len(x.parts), reverse=True):
        if not framework.is_symlink():
            run('codesign', '--force', '--sign', '-', framework)
    if mcp_sdk is not None:
        manifest['mcp'] = mcp_metadata(app, manifest, mcp_sdk)
    (app / 'Contents/Resources/build-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    run('codesign', '--force', '--sign', '-', '--entitlements', entitlements, app)
    run('codesign', '--verify', '--deep', '--strict', app)
    verify_packaged_mcp(app, manifest, execute_version=True)


def add_module_sources(bundle, modules, go, source, mcp_index=None):
    for name, value in [('go-modules.json', modules), ('mcp-go-modules.json', mcp_index)]:
        if value is None:
            continue
        data = (json.dumps(value, indent=2) + '\n').encode()
        member = tarfile.TarInfo(name)
        member.size = len(data)
        bundle.addfile(member, io.BytesIO(data))
    # Includes the union of dependencies actually linked by the engine, hosts
    # and nested MCP module. Exact module ZIPs are shared, not omitted/doubled.
    notices = load('kownload_source_notices', source / 'scripts/collect-macos-notices.py')
    for module in modules:
        meta = notices.module_source(go, source, module)
        bundle.add(meta['Zip'], arcname='dependency-sources/go/' + pathlib.Path(meta['Zip']).name.replace('.zip', '') + '-' + hashlib.sha256(module['module'].encode()).hexdigest()[:12] + '.zip')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--ref', default='HEAD')
    p.add_argument('--output', type=pathlib.Path, required=True)
    p.add_argument('--preview', action='store_true')
    p.add_argument('--preview-home', type=pathlib.Path,
                   help='existing private /private/tmp HOME; requires --preview')
    p.add_argument('--migration-source', type=pathlib.Path)
    p.add_argument('--work-dir', type=pathlib.Path)
    a = p.parse_args()
    if a.preview_home is not None and not a.preview:
        p.error('--preview-home requires --preview')
    if a.migration_source and not a.preview:
        p.error('--migration-source is available only for isolated previews')
    if a.migration_source and (not a.migration_source.is_dir() or
        not a.migration_source.resolve().is_relative_to(pathlib.Path('/private/tmp'))):
        p.error('--migration-source must be a private clone under /private/tmp')
    # A clean tree makes the running build control and archived inputs reviewable.
    if output('git', 'status', '--porcelain', cwd=REPO):
        raise RuntimeError('Commit changes before packaging a source revision')
    commit = output('git', 'rev-parse', '--verify', a.ref + '^{commit}', cwd=REPO)
    if commit != output('git', 'rev-parse', 'HEAD', cwd=REPO):
        raise RuntimeError('Check out the selected commit before building')
    preview_storage_override = None
    if a.preview_home is not None:
        identity = json.loads((REPO / 'config/identity.json').read_text())
        preview_storage_override = private_preview_storage(a.preview_home, identity)
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
    identity = json.loads((source / 'config/identity.json').read_text())
    run('python3', source / 'scripts/generate-identity.py', '--check')
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
    xcode_version = output('xcodebuild', '-version')
    xcode_major = re.match(r'Xcode (\d+)', xcode_version)
    if not xcode_major or int(xcode_major[1]) < config['minimumXcodeMajor']:
        raise RuntimeError('Xcode 26 or newer SDK is required by the native panel')
    go_version = output(go, 'version').split()[2].removeprefix('go')
    flutter_meta = json.loads(output(flutter_bin, '--version', '--machine'))
    if go_version != config['goVersion'] or (flutter_meta['frameworkVersion'] != config['flutterVersion'] or flutter_meta['frameworkRevision'] != config['flutterRevision']):
        raise RuntimeError('SDK version differs from config/release.json')
    env = os.environ.copy()
    env.update({'CGO_ENABLED': '1', 'GOOS': 'darwin', 'GOARCH': 'arm64',
                'MACOSX_DEPLOYMENT_TARGET': config['minimumMacOS'],
                'COCOAPODS_DISABLE_STATS': 'true', 'FLUTTER_SUPPRESS_ANALYTICS': 'true'})
    env['PATH'] = str(pathlib.Path(go).parent) + os.pathsep + env.get('PATH', '')
    ui = source / 'ui/flutter'
    core = work / 'libgopeed.dylib'
    host = work / identity['hostExecutable']
    migration = work / identity['migrationExecutable']
    mcp_root = source / 'cmd/kownload-mcp'
    mcp = work / 'kownload-mcp' if (mcp_root / 'go.mod').is_file() else None
    mcp_sdk = None
    run(go, 'build', '-trimpath', '-tags', 'nosqlite', '-buildmode=c-shared',
        '-ldflags=-w -s -X github.com/GopeedLab/gopeed/pkg/base.Version=' + release,
        '-o', core, './bind/desktop', cwd=source, env=env)
    host_flags = ('-w -s -X main.sourceCommit=' + commit +
                  ' -X main.releaseVersion=' + release +
                  (' -X main.previewBuild=true' if a.preview else ''))
    run(go, 'build', '-trimpath', '-ldflags=' + host_flags, '-o', host, './cmd/native-host', cwd=source, env=env)
    run(go, 'build', '-trimpath', '-ldflags=-w -s', '-o', migration, './cmd/identity-migrate', cwd=source, env=env)
    if mcp is not None:
        sdk = json.loads(output(go, 'list', '-m', '-json', 'github.com/modelcontextprotocol/go-sdk', cwd=mcp_root))
        mcp_sdk = sdk['Version']
        if mcp_sdk != MCP_SDK_VERSION:
            raise RuntimeError('MCP SDK differs from the reviewed fixed version')
        run(go, 'mod', 'verify', cwd=mcp_root, env=env)
        run(go, 'build', '-mod=readonly', '-trimpath',
            '-ldflags=-w -s -X main.version=' + mcp_version(release, commit),
            '-o', mcp, '.', cwd=mcp_root, env=env)
    (ui / 'macos/Frameworks').mkdir(parents=True, exist_ok=True)
    (ui / 'assets/exec').mkdir(parents=True, exist_ok=True)
    shutil.copy2(core, ui / 'macos/Frameworks/libgopeed.dylib')
    shutil.copy2(host, ui / ('assets/exec/' + identity['hostExecutable']))
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
             '--dart-define=KOWNLOAD_SOURCE_COMMIT=' + commit,
             '--split-debug-info=' + str(work / 'symbols')]
    preview_storage = preview_storage_override if preview_storage_override is not None else work / 'preview-data'
    if a.preview:
        flags.append('--dart-define=KOWNLOAD_PREVIEW_STORAGE=' + str(preview_storage))
        if a.migration_source:
            flags.append('--dart-define=KOWNLOAD_MIGRATION_SOURCE=' + str(a.migration_source.resolve()))
        with xcconfig.open('a') as f:
            f.write('PRODUCT_BUNDLE_IDENTIFIER = ' + identity['previewBundleId'] + '\n')
    run(flutter_bin, 'build', 'macos', *flags, cwd=ui, env=env)
    stage = work / 'dmg'
    stage.mkdir()
    app = stage / 'kownload.app'
    shutil.copytree(ui / ('build/macos/Build/Products/Release/' + identity['productName'] + '.app'), app, symlinks=True)
    (app / 'Contents/Helpers').mkdir(exist_ok=True)
    shutil.copy2(migration, app / ('Contents/Helpers/' + identity['migrationExecutable']))
    if mcp is not None:
        shutil.copy2(mcp, app / MCP_HELPER)
    info_path = app / 'Contents/Info.plist'
    info = plistlib.loads(info_path.read_bytes())
    if a.preview:
        info['CFBundleIdentifier'] = identity['previewBundleId']
        info['CFBundleDisplayName'] = 'kownload 独立身份预览'
        info['CFBundleName'] = 'kownload 独立身份预览'
        for key in ['CFBundleURLTypes', 'CFBundleDocumentTypes', 'UTExportedTypeDeclarations']:
            info.pop(key, None)
    info_path.write_bytes(plistlib.dumps(info))
    resources = app / 'Contents/Resources'
    legal = resources / 'legal'
    notices = load('kownload_notices', source / 'scripts/collect-macos-notices.py')
    modules = notices.collect(source, legal, [core, host, migration] + ([mcp] if mcp is not None else []), go, flutter)
    mcp_index = None
    if mcp is not None:
        used = {(m['module'], m['version']) for m in notices.binary_modules(go, mcp)}
        mcp_index = {'modulePath': 'cmd/kownload-mcp',
                     'goModSha256': sha256_file(mcp_root / 'go.mod'),
                     'goSumSha256': sha256_file(mcp_root / 'go.sum'),
                     'modules': [m for m in modules if (m['module'], m['version']) in used]}
        if ('github.com/modelcontextprotocol/go-sdk', MCP_SDK_VERSION) not in used:
            raise RuntimeError('MCP compiled SDK provenance is missing')
        (legal / 'mcp-go-modules.json').write_text(json.dumps(mcp_index, indent=2) + '\n')
        shutil.copytree(mcp_root / 'licenses', legal / 'mcp-module-licenses')
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
                'xcodeVersion': xcode_version,
                'flutterRevision': flutter_meta['frameworkRevision'], 'helperVersion': helper_version,
                'projectComponentsFromSourceCommit': ['Go engine', 'Native host', 'Identity migration', 'Flutter app'],
                'nativePlugins': 'rebuilt from locked third-party source',
                'automaticInstaller': False}
    if mcp is not None:
        manifest['projectComponentsFromSourceCommit'].append('MCP stdio bridge')
    seal_candidate(app, ui / 'macos/Runner/Release.entitlements', manifest, mcp_sdk)
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
        embedded = json.loads((mount / 'kownload.app/Contents/Resources/build-manifest.json').read_text())
        if embedded != manifest:
            raise RuntimeError('Final image manifest differs from the sealed App')
        verify_packaged_mcp(mount / 'kownload.app', embedded)
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
        add_module_sources(bundle, modules, go, source, mcp_index)
    (destination / 'go-modules.json').write_text(json.dumps(modules, indent=2) + '\n')
    (destination / 'build-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    assets = [dmg, helper_zip, destination / source_name]
    (destination / 'SHA256SUMS.txt').write_text(''.join(hashlib.file_digest(f.open('rb'), 'sha256').hexdigest() + '  ' + f.name + '\n' for f in assets))
    print('Candidate app:', app, '\nTemporary preview data:', preview_storage, '\nAssets:', destination)


if __name__ == '__main__':
    main()
