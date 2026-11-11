#!/usr/bin/env python3
"""Offline fixtures only: python3 scripts/test-rollback-identity.py."""
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name('rollback-identity.py')
COMMIT = '1' * 40
BUNDLE = 'io.github.kbwumin13-sudo.kownload'


def write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def tree(root):
    return {str(p.relative_to(root)): p.read_bytes()
            for p in sorted(root.rglob('*')) if p.is_file()}


def fixture_digest(root):
    records = []
    for p in sorted(root.rglob('*')):
        rel = p.relative_to(root).as_posix()
        if p.is_symlink():
            records.append([rel, 'link', os.readlink(p)])
        elif p.is_dir():
            records.append([rel, 'directory', p.stat().st_mode & 0o777])
        else:
            records.append([rel, 'file', p.stat().st_mode & 0o777,
                            hashlib.sha256(p.read_bytes()).hexdigest()])
    raw = json.dumps(records, ensure_ascii=False, separators=(',', ':')).encode()
    return hashlib.sha256(raw).hexdigest()


class Rollback(unittest.TestCase):
    def setUp(self):
        self.dir = pathlib.Path(tempfile.mkdtemp(prefix='kownload-rollback-test-', dir='/private/tmp'))
        self.new, self.old, self.backup = [self.dir / x for x in ['new', 'old', 'backup']]
        for name, data in {'.kownload-identity.json': b'{"version":1}',
                           'gopeed.db': b'active prepared/aborted fixture',
                           'database.lock': b'', 'database.hive': b'active hive'}.items():
            write(self.new / name, data)
        write(self.old / 'gopeed.db', b'old store')
        self.new_tree, self.old_tree = tree(self.new), tree(self.old)

    def tearDown(self):
        shutil.rmtree(self.dir)

    def run_legacy(self, *extra):
        return subprocess.run([sys.executable, str(SCRIPT), '--new-root', str(self.new),
                               '--old-root', str(self.old), '--backup-root', str(self.backup),
                               '--fixture', *extra], capture_output=True, text=True)

    def assert_unchanged(self):
        self.assertEqual(tree(self.new), self.new_tree)
        self.assertEqual(tree(self.old), self.old_tree)
        self.assertFalse(self.backup.exists())

    def test_dry_run_changes_nothing(self):
        p = self.run_legacy()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertFalse(json.loads(p.stdout)['apply'])
        self.assert_unchanged()

    def test_unpaired_active_database_apply_is_rejected_before_writes(self):
        p = self.run_legacy('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertIn('paired snapshot', p.stderr)
        self.assert_unchanged()

    def test_manifest_restore_cannot_bypass_pairing(self):
        home = self.dir / 'home'
        write(self.new / 'native-manifest-backups/chrome/com.gopeed.gopeed.json', b'{}')
        self.new_tree = tree(self.new)
        p = self.run_legacy('--apply', '--restore-browser-manifests', '--home', str(home))
        self.assertNotEqual(p.returncode, 0)
        self.assert_unchanged()
        self.assertFalse(home.exists())


class SnapshotPair(Rollback):
    def setUp(self):
        super().setUp()
        self.snapshot = self.dir / 'snapshot'
        self.app = self.snapshot / 'kownload.app'
        self.data = self.snapshot / 'data'
        self.preferences = self.snapshot / 'preferences.plist'
        write(self.app / 'Contents/Info.plist', plistlib.dumps({
            'CFBundleIdentifier': BUNDLE, 'CFBundleVersion': '39'}))
        write(self.app / 'Contents/Resources/build-manifest.json', json.dumps({
            'sourceCommit': COMMIT, 'buildNumber': 39, 'releaseVersion': 'fixture.39'}).encode())
        write(self.app / 'Contents/MacOS/kownload', b'never executed')
        write(self.data / 'gopeed.db', b'paired snapshot database')
        write(self.data / 'database.hive', b'paired snapshot hive')
        write(self.data / '.kownload-identity.json', b'{"version":1,"imported":true}')
        write(self.preferences, plistlib.dumps({'fixturePreference': 'original'}))
        self.manifest = self.snapshot / 'snapshot.json'
        self.meta = {
            'version': 1, 'sourceCommit': COMMIT, 'releaseVersion': 'fixture.39',
            'app': {'path': 'kownload.app', 'sha256': fixture_digest(self.app),
                    'bundleId': BUNDLE, 'buildNumber': 39},
            'data': {'path': 'data', 'sha256': fixture_digest(self.data)},
            'preferences': {'path': 'preferences.plist', 'domain': BUNDLE,
                            'sha256': hashlib.sha256(self.preferences.read_bytes()).hexdigest()}}
        self.save_manifest()
        self.output = self.dir / 'offline-copy'

    def save_manifest(self):
        self.manifest.write_text(json.dumps(self.meta))

    def run_pair(self, *extra, python=sys.executable):
        return subprocess.run([python, str(SCRIPT), '--snapshot-manifest', str(self.manifest),
                               '--output', str(self.output), '--fixture', *extra],
                              capture_output=True, text=True)

    def test_paired_dry_run_verifies_but_does_not_create_output(self):
        p = self.run_pair()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertTrue(json.loads(p.stdout)['pairVerified'])
        self.assertFalse(self.output.exists())
        self.assert_unchanged()

    def test_apply_copies_snapshot_not_active_data_and_keeps_preferences_offline(self):
        before = tree(self.snapshot)
        p = self.run_pair('--apply')
        self.assertEqual(p.returncode, 0, p.stderr)
        plan = json.loads((self.output / 'restore-plan.json').read_text())
        self.assertFalse(plan['automaticResumeAllowed'])
        self.assertFalse(plan['outputFilesVerified'])
        self.assertTrue(plan['requiresManualAppAndPreferencesRestore'])
        self.assertEqual((self.output / 'data/gopeed.db').read_bytes(), b'paired snapshot database')
        self.assertEqual((self.output / 'preferences.plist').read_bytes(), self.preferences.read_bytes())
        self.assertEqual(tree(self.snapshot), before)
        self.assert_unchanged()

    def test_tamper_each_component_rejects_before_output(self):
        for path in [self.app / 'Contents/MacOS/kownload', self.data / 'gopeed.db', self.preferences]:
            original = path.read_bytes()
            path.write_bytes(original + b'changed')
            p = self.run_pair('--apply')
            self.assertNotEqual(p.returncode, 0)
            self.assertIn('snapshot', p.stderr)
            self.assertFalse(self.output.exists())
            path.write_bytes(original)

    def test_missing_component_rejects_before_output(self):
        self.preferences.unlink()
        p = self.run_pair('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(self.output.exists())

    def test_unknown_manifest_or_app_pair_version_rejects(self):
        for edit in [lambda: self.meta.update(version=2),
                     lambda: self.meta['app'].update(buildNumber=40),
                     lambda: self.meta.update(sourceCommit='2' * 40),
                     lambda: self.meta['preferences'].update(domain='com.other.app')]:
            original = json.loads(json.dumps(self.meta))
            edit()
            self.save_manifest()
            p = self.run_pair('--apply')
            self.assertNotEqual(p.returncode, 0)
            self.assertFalse(self.output.exists())
            self.meta = original
        self.save_manifest()

    def test_external_path_or_symlink_rejects(self):
        self.meta['data']['path'] = '../new'
        self.save_manifest()
        p = self.run_pair('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(self.output.exists())
        self.meta['data']['path'] = 'data'
        link = self.data / 'external'
        link.symlink_to(self.new)
        self.meta['data']['sha256'] = fixture_digest(self.data)
        self.save_manifest()
        p = self.run_pair('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(self.output.exists())

    def test_existing_output_not_overwritten(self):
        write(self.output / 'keep', b'keep')
        before = tree(self.output)
        p = self.run_pair('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(tree(self.output), before)

    def test_internal_framework_symlink_is_preserved(self):
        versions = self.app / 'Contents/Frameworks/Fixture.framework/Versions'
        write(versions / 'A/Fixture', b'fixture framework')
        (versions / 'Current').symlink_to('A')
        self.meta['app']['sha256'] = fixture_digest(self.app)
        self.save_manifest()
        p = self.run_pair('--apply')
        self.assertEqual(p.returncode, 0, p.stderr)
        copied = self.output / 'App.app/Contents/Frameworks/Fixture.framework/Versions'
        self.assertEqual(os.readlink(copied / 'Current'), 'A')
        self.assertEqual((copied / 'Current/Fixture').read_bytes(), b'fixture framework')

    @unittest.skipUnless(os.path.exists('/usr/bin/python3'), 'system Python not present')
    def test_system_python_pair_support(self):
        p = self.run_pair('--apply', python='/usr/bin/python3')
        self.assertEqual(p.returncode, 0, p.stderr)


if __name__ == '__main__':
    unittest.main()
