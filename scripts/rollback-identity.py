#!/usr/bin/env python3
"""Copy the unchanged database format back; never installs/launches an app."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import time

IDENTITY = json.loads((pathlib.Path(__file__).resolve().parents[1] / 'config/identity.json').read_text())
# Browser profile directories under ~/Library/Application Support whose
# NativeMessagingHosts the installed app switches (identity/native_manifests.dart).
BROWSERS = {'chrome': ('Google', 'Chrome'), 'edge': ('Microsoft Edge',)}
SKIP = {'.kownload-identity.json', '.kownload-import-pending.json', '.kownload-import-owner',
        'ipc', 'native-manifest-backups', IDENTITY['hostExecutable'], 'logs'}

def sha256(path):
    # hashlib.file_digest needs Python 3.11; macOS ships 3.9 as /usr/bin/python3.
    digest = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            digest.update(block)
    return digest.digest()

@contextlib.contextmanager
def store_locks(roots):
    files = []
    try:
        for root in roots:
            for name in ['gopeed.db', 'database.lock']:
                file = root / name
                if not file.exists():
                    continue
                f = file.open('r+b')
                files.append(f)
                # Use each component's lock API, not both on the same inode.
                if name == 'gopeed.db':
                    fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
                else:
                    fcntl.lockf(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        for f in reversed(files):
            f.close()

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--new-root', required=True, type=pathlib.Path)
    p.add_argument('--old-root', required=True, type=pathlib.Path)
    p.add_argument('--backup-root', required=True, type=pathlib.Path)
    p.add_argument('--manifest-pair', action='append', nargs=2, default=[], metavar=('BACKUP_DIR', 'DESTINATION'))
    p.add_argument('--apply', action='store_true')
    p.add_argument('--restore-browser-manifests', action='store_true',
                   help='Also restore Chrome/Edge from <new root>/native-manifest-backups/<browser>')
    p.add_argument('--home', type=pathlib.Path, default=pathlib.Path.home(),
                   help='Home holding the browser profiles (fixtures use a temporary one)')
    p.add_argument('--fixture', action='store_true', help='Only temporary roots; skip installed-app process check')
    a = p.parse_args()
    new, old, backup = [x.absolute() for x in [a.new_root, a.old_root, a.backup_root]]
    pairs = [(pathlib.Path(x).absolute(), pathlib.Path(y).absolute()) for x, y in a.manifest_pair]
    if a.restore_browser_manifests:
        for browser, parts in BROWSERS.items():
            saved = new / 'native-manifest-backups' / browser
            if saved.is_dir():
                pairs.append((saved, a.home.absolute().joinpath('Library', 'Application Support', *parts, 'NativeMessagingHosts')))
    all_paths = [new, old, backup] + [x for pair in pairs for x in pair]
    if any(x.is_symlink() or x.resolve() != x for x in all_paths):
        p.error('Use canonical paths without symlinks')
    if len(set([new, old, backup])) != 3 or any(x in y.parents for x in [new, old, backup] for y in [new, old, backup] if x != y):
        p.error('Roots must be disjoint')
    if a.fixture and any(not x.is_relative_to(pathlib.Path('/private/tmp')) for x in all_paths):
        p.error('Fixture paths must be under canonical /private/tmp')
    if not a.apply:
        print(json.dumps({'apply': False, 'formatConversion': False, 'backupRequired': True}))
        return
    if not new.is_dir() or not (new / '.kownload-identity.json').is_file() or backup.exists():
        p.error('Require a completed new identity root and a new backup destination')
    # The new root is kept under another name after the copy. Leaving it in
    # place would let a later upgrade reopen it instead of importing what the
    # old version does after the rollback.
    kept = new.with_name(new.name + '.rolled-back-' + time.strftime('%Y%m%d-%H%M%S'))
    if kept.exists():
        p.error('Retry in a second: the kept-root name already exists')
    for saved, _ in pairs:
        for name in [IDENTITY['hostName'], IDENTITY['legacyHostName']]:
            if (saved / (name + '.json')).is_file() == (saved / (name + '.absent')).is_file():
                p.error('Require exactly one original/absent backup for each host name')
    if not a.fixture:
        commands = subprocess.check_output(['/bin/ps', '-axo', 'comm='], text=True).splitlines()
        if any(x.strip().endswith(('/Contents/MacOS/kownload', '/Contents/MacOS/Gopeed')) for x in commands):
            p.error('Quit both versions before rollback')
    old.parent.mkdir(parents=True, exist_ok=True)
    backup.parent.mkdir(parents=True, exist_ok=True)
    with store_locks([new, old]):
        stage = pathlib.Path(tempfile.mkdtemp(prefix='.kownload-rollback-', dir=old.parent))
        try:
            subprocess.run(['/bin/cp', '-cR', str(new) + '/.', str(stage)], check=True)
            for name in SKIP:
                item = stage / name
                if item.is_dir():
                    shutil.rmtree(item)
                elif item.exists():
                    item.unlink()
            if (old / 'host').is_file():
                shutil.copy2(old / 'host', stage / 'host')
            # Verify each source file before either active directory is moved.
            for src in new.rglob('*'):
                rel = src.relative_to(new)
                if rel.parts[0] in SKIP or src.is_dir():
                    continue
                if src.is_symlink():
                    raise RuntimeError('source_symlink')
                dst = stage / rel
                if sha256(src) != sha256(dst):
                    raise RuntimeError('copy_verification_failed')
            if old.exists():
                old.rename(backup)
            else:
                backup.mkdir(mode=0o700)
            os.chmod(backup, 0o700)
            try:
                stage.rename(old)
            except Exception:
                backup.rename(old)
                raise
        finally:
            if stage.exists():
                shutil.rmtree(stage)
    # Keep backups intact. Missing-original markers remove only the two known
    # alias manifests, never arbitrary files in a browser directory.
    for saved, destination in pairs:
        destination.mkdir(parents=True, exist_ok=True)
        for name in [IDENTITY['hostName'], IDENTITY['legacyHostName']]:
            original = saved / (name + '.json')
            dest = destination / (name + '.json')
            if original.is_file():
                temporary = dest.with_suffix('.rollback-tmp')
                shutil.copy2(original, temporary)
                temporary.replace(dest)
            elif (saved / (name + '.absent')).is_file() and dest.exists():
                dest.unlink()
    new.rename(kept)
    print(json.dumps({'applied': True, 'formatConversion': False, 'oldRootBackedUp': True, 'copyHashesMatch': True,
                      'manifestDirectoriesRestored': len(pairs), 'newRootKeptAt': str(kept)}))

if __name__ == '__main__':
    main()
