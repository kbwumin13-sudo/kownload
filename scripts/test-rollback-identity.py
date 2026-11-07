#!/usr/bin/env python3
"""Fixture tests for rollback-identity.py. Run: python3 scripts/test-rollback-identity.py"""
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name('rollback-identity.py')
IDENTITY = json.loads((SCRIPT.parents[1] / 'config/identity.json').read_text())


def write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def tree(root):
    return {str(p.relative_to(root)): p.read_bytes() for p in sorted(root.rglob('*')) if p.is_file()}


class Rollback(unittest.TestCase):
    def setUp(self):
        # Fixture mode accepts only canonical /private/tmp paths.
        self.dir = pathlib.Path(tempfile.mkdtemp(prefix='kownload-rollback-test-', dir='/private/tmp'))
        self.new, self.old, self.backup = self.dir / 'new', self.dir / 'old', self.dir / 'backup'
        for name, data in {'.kownload-identity.json': b'{"version":1}', 'gopeed.db': b'new store',
                           'database.lock': b'', 'database.hive': b'new hive',
                           'extensions/fixture/index.js': b'new extension', 'ipc/host.sock': b'',
                           IDENTITY['hostExecutable']: b'new host', 'logs/core.log': b'log',
                           'native-manifest-backups/chrome/com.gopeed.gopeed.json': b'{}'}.items():
            write(self.new / name, data)
        for name, data in {'gopeed.db': b'old store', 'database.lock': b'', 'database.hive': b'old hive',
                           'host': b'old host'}.items():
            write(self.old / name, data)
        self.new_tree, self.old_tree = tree(self.new), tree(self.old)

    def tearDown(self):
        shutil.rmtree(self.dir)

    def run_script(self, *extra, python=sys.executable):
        return subprocess.run([python, str(SCRIPT), '--new-root', str(self.new), '--old-root', str(self.old),
                               '--backup-root', str(self.backup), '--fixture', *extra],
                              capture_output=True, text=True)

    def test_dry_run_changes_nothing(self):
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['apply'], False)
        self.assertEqual(tree(self.new), self.new_tree)
        self.assertEqual(tree(self.old), self.old_tree)
        self.assertFalse(self.backup.exists())

    def check_applied(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        reply = json.loads(result.stdout.strip().splitlines()[-1])
        kept = pathlib.Path(reply['newRootKeptAt'])
        # The next upgrade must import the old root again instead of reopening
        # this copy, so the new root no longer exists under its own name.
        self.assertFalse(self.new.exists())
        self.assertEqual(kept.parent, self.new.parent)
        self.assertTrue(kept.name.startswith(self.new.name + '.rolled-back-'))
        self.assertEqual(tree(kept), self.new_tree)
        rolled = tree(self.old)
        self.assertEqual(rolled['gopeed.db'], b'new store')
        self.assertEqual(rolled['database.hive'], b'new hive')
        self.assertEqual(rolled['extensions/fixture/index.js'], b'new extension')
        self.assertEqual(rolled['host'], b'old host')
        for name in ['.kownload-identity.json', 'ipc', IDENTITY['hostExecutable'], 'logs', 'native-manifest-backups']:
            self.assertFalse((self.old / name).exists(), name)
        self.assertEqual(tree(self.backup), self.old_tree)
        self.assertEqual(os.stat(self.backup).st_mode & 0o777, 0o700)
        again = self.run_script('--apply')
        self.assertNotEqual(again.returncode, 0)
        self.assertEqual(tree(self.old), rolled)

    def test_apply_moves_new_root_aside_and_keeps_every_side(self):
        self.check_applied(self.run_script('--apply'))

    @unittest.skipUnless(os.path.exists('/usr/bin/python3'), 'system Python not present')
    def test_system_python(self):
        self.check_applied(self.run_script('--apply', python='/usr/bin/python3'))


if __name__ == '__main__':
    unittest.main()
