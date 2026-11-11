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
from xml.parsers.expat import ExpatError


def file_digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def tree_digest(root, app=False):
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
    return meta, app, data, prefs


def prepare_copy(manifest, output, pair):
    meta, app, data, prefs = pair
    output = canonical(output)
    if output.exists():
        raise ValueError('snapshot_output_exists')
    for source in [manifest, app, data, prefs]:
        if output == source or output in source.parents or source in output.parents:
            raise ValueError('snapshot_output_overlaps_input')
    # Validation above has finished before the first write. mkdir is exclusive;
    # an existing directory (including a competing empty one) is never replaced.
    output.mkdir(mode=0o700)
    shutil.copytree(app, output / 'App.app', symlinks=True)
    shutil.copytree(data, output / 'data')
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
