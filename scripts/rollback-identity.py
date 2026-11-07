#!/usr/bin/env python3
"""Copy the unchanged database format back; never installs/launches an app."""
import argparse
import contextlib
import fcntl
import json
import os
import pathlib
import shutil
import subprocess
import tempfile

IDENTITY = json.loads((pathlib.Path(__file__).resolve().parents[1] / 'config/identity.json').read_text())
SKIP = {'.kownload-identity.json', '.kownload-import-pending.json', '.kownload-import-owner',
        'ipc', 'native-manifest-backups', IDENTITY['hostExecutable'], 'logs'}

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
                # bbolt and Dart use different OS locking APIs on some hosts.
                fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
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
    p.add_argument('--fixture', action='store_true', help='Only temporary roots; skip installed-app process check')
    a = p.parse_args()
    new, old, backup = [x.absolute() for x in [a.new_root, a.old_root, a.backup_root]]
    all_paths = [new, old, backup] + [pathlib.Path(x).absolute() for pair in a.manifest_pair for x in pair]
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
            import hashlib
            for src in new.rglob('*'):
                rel = src.relative_to(new)
                if rel.parts[0] in SKIP or src.is_dir():
                    continue
                if src.is_symlink():
                    raise RuntimeError('source_symlink')
                dst = stage / rel
                if hashlib.file_digest(src.open('rb'), 'sha256').digest() != hashlib.file_digest(dst.open('rb'), 'sha256').digest():
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
    for saved, destination in a.manifest_pair:
        saved, destination = pathlib.Path(saved), pathlib.Path(destination)
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
    print(json.dumps({'applied': True, 'formatConversion': False, 'oldRootBackedUp': True, 'copyHashesMatch': True}))

if __name__ == '__main__':
    main()
