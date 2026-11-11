#!/usr/bin/env python3
"""Verify an App/data/preferences snapshot pair and prepare an offline copy.

Never installs/starts an App, changes live data/preferences/manifests, or grants
execution permission. The former live-database backfill is deliberately refused.
"""
import argparse
import contextlib
import ctypes
import errno
import hashlib
import json
import os
import pathlib
import plistlib
import re
import shutil
import stat
import sys
from xml.parsers.expat import ExpatError

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


def mcp_attribute_functions():
    if sys.platform != 'darwin':
        raise ValueError('snapshot_mcp_marker_unsupported_platform')
    # Public Darwin fd APIs; fixed library and attribute, no path-based write
    # fallback when Python does not expose its own fd xattr operations.
    library = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
    get = library.fgetxattr
    get.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p,
                   ctypes.c_size_t, ctypes.c_uint32, ctypes.c_int]
    get.restype = ctypes.c_ssize_t
    put = library.fsetxattr
    put.argtypes = get.argtypes
    put.restype = ctypes.c_int
    return get, put


def read_mcp_marker(fd):
    get, _ = mcp_attribute_functions()
    name = MCP_MARKER.encode('ascii')
    size = get(fd, name, None, 0, 0, 0)
    if size < 0:
        raise OSError(ctypes.get_errno(), 'snapshot_mcp_marker_unavailable')
    if not 0 < size <= MCP_MARKER_LIMIT:
        raise ValueError('snapshot_mcp_marker_oversized')
    buffer = ctypes.create_string_buffer(size)
    count = get(fd, name, buffer, size, 0, 0)
    if count != size:
        raise ValueError('snapshot_mcp_marker_changed')
    return buffer.raw


def write_mcp_marker(fd, raw):
    if not 0 < len(raw) <= MCP_MARKER_LIMIT:
        raise ValueError('snapshot_mcp_marker_oversized')
    _, put = mcp_attribute_functions()
    buffer = ctypes.create_string_buffer(raw)
    if put(fd, MCP_MARKER.encode('ascii'), buffer, len(raw), 0, 0) != 0:
        raise OSError(ctypes.get_errno(), 'snapshot_mcp_marker_write_failed')


def mcp_fd_info(fd):
    info = os.fstat(fd)
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or
            info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError('snapshot_mcp_helper_invalid')
    return info


@contextlib.contextmanager
def mcp_helper_file(data, writable=False):
    if not data.is_absolute() or '..' in data.parts:
        raise ValueError('snapshot_mcp_helper_invalid')
    directories = []
    entry = None
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    try:
        # Walk every ancestor from / with openat + O_NOFOLLOW. A checked
        # pathname is never resolved again to perform the attribute write.
        directories.append(os.open('/', flags))
        for part in data.parts[1:]:
            directories.append(os.open(part, flags, dir_fd=directories[-1]))
        try:
            parent = os.stat('bin', dir_fd=directories[-1], follow_symlinks=False)
        except FileNotFoundError:
            yield None
            return
        if stat.S_ISLNK(parent.st_mode):
            raise ValueError('snapshot_mcp_helper_invalid')
        if not stat.S_ISDIR(parent.st_mode):
            yield None  # Preserve unrelated legacy ordinary bin files.
            return
        directories.append(os.open('bin', flags, dir_fd=directories[-1]))
        try:
            entry = os.open('kownload-mcp', (os.O_RDWR if writable else os.O_RDONLY) |
                            os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directories[-1])
        except FileNotFoundError:
            yield None
            return
        mcp_fd_info(entry)
        yield entry
    except OSError as error:
        if error.errno in (errno.ELOOP, errno.ENOTDIR):
            raise ValueError('snapshot_mcp_helper_invalid') from None
        raise
    finally:
        if entry is not None:
            os.close(entry)
        for directory in reversed(directories):
            os.close(directory)


def mcp_fd_digest(fd):
    os.lseek(fd, 0, os.SEEK_SET)
    digest = hashlib.sha256()
    while True:
        block = os.read(fd, 1 << 20)
        if not block:
            return digest.hexdigest()
        digest.update(block)


def mcp_install_state(data):
    with mcp_helper_file(data) as fd:
        return None if fd is None else mcp_state_from_fd(fd)


def mcp_state_from_fd(fd):
    info = mcp_fd_info(fd)
    raw = read_mcp_marker(fd)
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
    digest = mcp_fd_digest(fd)
    if marker['sha256'] != digest:
        raise ValueError('snapshot_mcp_marker_digest_mismatch')
    after = mcp_fd_info(fd)
    if (after.st_dev, after.st_ino, after.st_mode) != (info.st_dev, info.st_ino, info.st_mode):
        raise ValueError('snapshot_mcp_source_changed')
    return {'raw': raw, 'sha256': digest, 'mode': info.st_mode & 0o777,
            'device': info.st_dev, 'inode': info.st_ino}


def copy_mcp_marker(source, destination, original):
    if mcp_install_state(source) != original:
        raise ValueError('snapshot_mcp_source_changed')
    with mcp_helper_file(destination, writable=True) as fd:
        if original is None:
            if fd is not None:
                raise ValueError('snapshot_mcp_copy_changed')
            return
        if fd is None or mcp_fd_digest(fd) != original['sha256']:
            raise ValueError('snapshot_mcp_copy_changed')
        # Retain this verified inode for both the write and its immediate read.
        write_mcp_marker(fd, original['raw'])
        result = mcp_state_from_fd(fd)
        if any(result[key] != original[key] for key in ['raw', 'sha256', 'mode']):
            raise ValueError('snapshot_mcp_copy_changed')
        # Also reject renamed/replaced path entries before publishing a plan.
        if mcp_install_state(destination) != result:
            raise ValueError('snapshot_mcp_copy_changed')
    if mcp_install_state(source) != original:
        raise ValueError('snapshot_mcp_source_changed')


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
