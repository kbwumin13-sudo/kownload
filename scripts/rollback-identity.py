#!/usr/bin/env python3
"""Verify an App/data/preferences snapshot pair and prepare an offline copy.

Never installs/starts an App, changes live data/preferences/manifests, or grants
execution permission. The former live-database backfill is deliberately refused.
"""
import argparse
import hashlib
import json
import os
import pathlib
import plistlib
import re
import shutil
import stat
import subprocess
from xml.parsers.expat import ExpatError

MCP_RELATIVE = pathlib.Path('bin/kownload-mcp')
MCP_MARKER = 'io.github.kbwumin13-sudo.kownload.mcp-install'
MCP_MARKER_LIMIT = 2048


def file_digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def tree_digest(root, app=False):
    # Content/mode/link digest only. The fixed MCP install marker is verified
    # separately; this is not a generic extended-attribute archive.
    records = []
    for p in sorted(root.rglob('*')):
        rel = p.relative_to(root).as_posix()
        mode = p.lstat().st_mode
        if stat.S_ISLNK(mode):
            target = os.readlink(p)
            if not app or os.path.isabs(target) or not p.resolve(strict=True).is_relative_to(root):
                raise ValueError('snapshot_symlink')
            records.append([rel, 'link', target])
        elif stat.S_ISDIR(mode):
            records.append([rel, 'directory', mode & 0o777])
        elif stat.S_ISREG(mode):
            records.append([rel, 'file', mode & 0o777, file_digest(p)])
        else:
            raise ValueError('snapshot_special_file')
    raw = json.dumps(records, ensure_ascii=False, separators=(',', ':')).encode()
    return hashlib.sha256(raw).hexdigest()


def read_mcp_marker(path):
    # macOS Python does not always expose os.getxattr. Read exactly this key,
    # with a bounded hex result, and never list attributes or touch quarantine.
    process = subprocess.Popen(['/usr/bin/xattr', '-s', '-px', MCP_MARKER, str(path)],
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    try:
        encoded = process.stdout.read(MCP_MARKER_LIMIT * 4 + 1)
        if len(encoded) > MCP_MARKER_LIMIT * 4:
            raise ValueError('snapshot_mcp_marker_oversized')
        if process.wait() != 0:
            raise ValueError('snapshot_mcp_marker_unavailable')
    finally:
        process.stdout.close()
        if process.poll() is None:
            process.kill()
            process.wait()
    try:
        raw = bytes.fromhex(encoded.decode('ascii'))
    except (ValueError, UnicodeError):
        raise ValueError('snapshot_mcp_marker_invalid') from None
    if not 0 < len(raw) <= MCP_MARKER_LIMIT:
        raise ValueError('snapshot_mcp_marker_oversized')
    return raw


def write_mcp_marker(path, raw):
    result = subprocess.run(['/usr/bin/xattr', '-s', '-wx', MCP_MARKER, raw.hex(), str(path)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if result.returncode != 0:
        raise ValueError('snapshot_mcp_marker_write_failed')


def mcp_helper_info(data):
    try:
        parent = (data / 'bin').lstat()
    except FileNotFoundError:
        return None
    if stat.S_ISLNK(parent.st_mode):
        raise ValueError('snapshot_mcp_helper_invalid')
    if not stat.S_ISDIR(parent.st_mode):
        return None  # Legacy non-MCP data may have an unrelated ordinary bin file.
    try:
        info = (data / MCP_RELATIVE).lstat()
    except FileNotFoundError:
        return None
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or
            info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('snapshot_mcp_helper_invalid')
    return info


def mcp_install_state(data):
    info = mcp_helper_info(data)
    if info is None:
        return None
    raw = read_mcp_marker(data / MCP_RELATIVE)
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('snapshot_mcp_marker_duplicate_field')
            result[key] = value
        return result
    try:
        marker = json.loads(raw.decode('utf-8'), object_pairs_hook=unique)
    except (ValueError, UnicodeError):
        raise ValueError('snapshot_mcp_marker_invalid') from None
    fields = {'recordVersion', 'product', 'sourceCommit', 'version',
              'readApiVersion', 'architecture', 'sha256'}
    if (not isinstance(marker, dict) or set(marker) != fields or
            type(marker['recordVersion']) is not int or marker['recordVersion'] != 1 or
            type(marker['readApiVersion']) is not int or marker['readApiVersion'] != 1 or
            marker['product'] != 'kownload-mcp' or marker['architecture'] != 'arm64' or
            not isinstance(marker['sourceCommit'], str) or
            not re.fullmatch('[0-9a-f]{40}', marker['sourceCommit']) or
            not isinstance(marker['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', marker['sha256']) or
            not isinstance(marker['version'], str) or
            not marker['version'].endswith('+' + marker['sourceCommit']) or
            not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._+-]{0,79}', marker['version'][:-41])):
        raise ValueError('snapshot_mcp_marker_unsupported')
    digest = file_digest(data / MCP_RELATIVE)
    if marker['sha256'] != digest:
        raise ValueError('snapshot_mcp_marker_digest_mismatch')
    after = mcp_helper_info(data)
    if after is None or (after.st_dev, after.st_ino, after.st_mode) != (info.st_dev, info.st_ino, info.st_mode):
        raise ValueError('snapshot_mcp_source_changed')
    return {'raw': raw, 'sha256': digest, 'mode': info.st_mode & 0o777,
            'device': info.st_dev, 'inode': info.st_ino}


def copy_mcp_marker(source, destination, original):
    if mcp_install_state(source) != original:
        raise ValueError('snapshot_mcp_source_changed')
    copied = mcp_helper_info(destination)
    if original is None:
        if copied is not None:
            raise ValueError('snapshot_mcp_copy_changed')
        return
    if copied is None or file_digest(destination / MCP_RELATIVE) != original['sha256']:
        raise ValueError('snapshot_mcp_copy_changed')
    write_mcp_marker(destination / MCP_RELATIVE, original['raw'])
    result = mcp_install_state(destination)
    if (result is None or any(result[key] != original[key] for key in ['raw', 'sha256', 'mode']) or
            mcp_install_state(source) != original):
        raise ValueError('snapshot_mcp_copy_changed')


def canonical(path):
    path = path.absolute()
    if path.resolve() != path or path.is_symlink():
        raise ValueError('snapshot_canonical_path_required')
    return path


def component(base, item, directory):
    if not isinstance(item, dict) or not isinstance(item.get('path'), str):
        raise ValueError('snapshot_component_required')
    rel = pathlib.PurePosixPath(item['path'])
    if rel.is_absolute() or '..' in rel.parts or not rel.parts or rel == pathlib.PurePosixPath('.'):
        raise ValueError('snapshot_relative_path_required')
    p = canonical(base / rel)
    if not p.is_relative_to(base) or (not p.is_dir() if directory else not p.is_file()):
        raise ValueError('snapshot_component_missing')
    if not isinstance(item.get('sha256'), str) or not re.fullmatch('[0-9a-f]{64}', item['sha256']):
        raise ValueError('snapshot_digest_required')
    return p


def verify_pair(manifest):
    manifest = canonical(manifest)
    meta = json.loads(manifest.read_text())
    if not isinstance(meta, dict) or type(meta.get('version')) is not int or meta['version'] != 1:
        raise ValueError('snapshot_version_unsupported')
    if not isinstance(meta.get('sourceCommit'), str) or not re.fullmatch('[0-9a-f]{40}', meta['sourceCommit']):
        raise ValueError('snapshot_source_required')
    if not isinstance(meta.get('releaseVersion'), str) or not meta['releaseVersion']:
        raise ValueError('snapshot_release_required')
    app = component(manifest.parent, meta.get('app'), True)
    data = component(manifest.parent, meta.get('data'), True)
    prefs = component(manifest.parent, meta.get('preferences'), False)
    paths = [app, data, prefs]
    if any(a == b or a in b.parents or b in a.parents
           for i, a in enumerate(paths) for b in paths[i + 1:]):
        raise ValueError('snapshot_components_overlap')
    # Metadata files themselves must be regular bundle files, not links outside it.
    info_path = canonical(app / 'Contents/Info.plist')
    build_path = canonical(app / 'Contents/Resources/build-manifest.json')
    if not info_path.is_relative_to(app) or not build_path.is_relative_to(app):
        raise ValueError('snapshot_app_metadata_invalid')
    info = plistlib.loads(info_path.read_bytes())
    build = json.loads(build_path.read_text())
    bundle, number = meta['app'].get('bundleId'), meta['app'].get('buildNumber')
    if (not isinstance(bundle, str) or not bundle or type(number) is not int or number <= 0
            or info.get('CFBundleIdentifier') != bundle or info.get('CFBundleVersion') != str(number)
            or build.get('sourceCommit') != meta['sourceCommit']
            or build.get('buildNumber') != number or build.get('releaseVersion') != meta['releaseVersion']
            or meta['preferences'].get('domain') != bundle):
        raise ValueError('snapshot_app_pair_mismatch')
    if not isinstance(plistlib.loads(prefs.read_bytes()), dict):
        raise ValueError('snapshot_preferences_invalid')
    for name, digest in [('app', tree_digest(app, app=True)),
                         ('data', tree_digest(data)), ('preferences', file_digest(prefs))]:
        if digest != meta[name]['sha256']:
            raise ValueError('snapshot_digest_mismatch')
    mcp_install_state(data)
    return meta, app, data, prefs


def prepare_copy(manifest, output, pair):
    meta, app, data, prefs = pair
    output = canonical(output)
    if output.exists():
        raise ValueError('snapshot_output_exists')
    for source in [manifest, app, data, prefs]:
        if output == source or output in source.parents or source in output.parents:
            raise ValueError('snapshot_output_overlaps_input')
    marker = mcp_install_state(data)
    # Validation above has finished before the first write. mkdir is exclusive;
    # an existing directory (including a competing empty one) is never replaced.
    output.mkdir(mode=0o700)
    shutil.copytree(app, output / 'App.app', symlinks=True)
    shutil.copytree(data, output / 'data')
    copy_mcp_marker(data, output / 'data', marker)
    shutil.copy2(prefs, output / 'preferences.plist')
    copied = json.loads(json.dumps(meta))
    copied['app']['path'] = 'App.app'
    copied['data']['path'] = 'data'
    copied['preferences']['path'] = 'preferences.plist'
    copied_manifest = output / 'snapshot.json'
    copied_manifest.write_text(json.dumps(copied, ensure_ascii=False, indent=2) + '\n')
    verify_pair(copied_manifest)
    verify_pair(manifest)  # Catch a changing input instead of certifying a mixed copy.
    plan = {'version': 1, 'pairVerified': True, 'sourceCommit': meta['sourceCommit'],
            'releaseVersion': meta['releaseVersion'], 'sourceManifestSha256': file_digest(manifest),
            'automaticResumeAllowed': False, 'startupTaskPolicy': 'imported-review',
            'outputFilesVerified': False, 'requiresManualAppAndPreferencesRestore': True,
            'note': 'Offline copy only. Referenced download files may have changed; review them before launch or resume.'}
    # This completion marker is published last. Failure leaves an incomplete
    # private copy without a restore-plan, never a partially changed live root.
    with (output / 'restore-plan.json').open('x') as f:
        json.dump(plan, f, indent=2)
        f.write('\n')
        f.flush()
        os.fsync(f.fileno())
    return plan


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--snapshot-manifest', type=pathlib.Path)
    p.add_argument('--output', type=pathlib.Path)
    p.add_argument('--apply', action='store_true', help='Create a new offline copy only')
    p.add_argument('--fixture', action='store_true', help='Require inputs/output under canonical /private/tmp')
    # Recognize former commands so they fail explicitly before touching a root.
    for name in ['new-root', 'old-root', 'backup-root', 'home']:
        p.add_argument('--' + name, type=pathlib.Path)
    p.add_argument('--manifest-pair', action='append', nargs=2, default=[])
    p.add_argument('--restore-browser-manifests', action='store_true')
    a = p.parse_args()
    legacy = any([a.new_root, a.old_root, a.backup_root, a.home,
                  a.manifest_pair, a.restore_browser_manifests])
    if legacy:
        if a.apply or a.snapshot_manifest or a.output:
            p.error('Live database rollback is refused; use a paired snapshot with --snapshot-manifest and --output')
        print(json.dumps({'apply': False, 'pairVerified': False, 'pairedSnapshotRequired': True,
                          'automaticResumeAllowed': False}))
        return
    if not a.snapshot_manifest or not a.output:
        p.error('A paired snapshot and a new offline --output are required')
    try:
        manifest, output = canonical(a.snapshot_manifest), canonical(a.output)
        if a.fixture and any(not x.is_relative_to(pathlib.Path('/private/tmp')) for x in [manifest, output]):
            raise ValueError('snapshot_fixture_path_required')
        pair = verify_pair(manifest)
        # Validate output conflicts on dry run too, without creating it.
        if output.exists() or any(output == s or output in s.parents or s in output.parents
                                  for s in [manifest, *pair[1:]]):
            raise ValueError('snapshot_output_conflict')
        if a.apply:
            result = prepare_copy(manifest, output, pair)
        else:
            result = {'pairVerified': True, 'automaticResumeAllowed': False, 'outputFilesVerified': False}
        print(json.dumps({'apply': a.apply, **result}))
    except (OSError, ValueError, KeyError, TypeError, AttributeError, RuntimeError, ExpatError, plistlib.InvalidFileException):
        # No private path, preference payload, or source exception is emitted.
        p.error('snapshot pair invalid, incomplete, mismatched, or output unavailable; no live state was changed')


if __name__ == '__main__':
    main()
