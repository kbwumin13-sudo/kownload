#!/usr/bin/env python3
"""Offline fixtures only: python3 scripts/test-rollback-identity.py."""
import hashlib
import importlib.util
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SCRIPT = pathlib.Path(__file__).with_name('rollback-identity.py')
COMMIT = '1' * 40
BUNDLE = 'io.github.kbwumin13-sudo.kownload'
MCP_MARKER = 'io.github.kbwumin13-sudo.kownload.mcp-install'


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

    def mcp_fixture(self):
        entry = self.data / 'bin/kownload-mcp'
        write(entry, b'controlled MCP helper fixture, never executed')
        entry.chmod(0o700)
        self.meta['data']['sha256'] = fixture_digest(self.data)
        self.save_manifest()
        value = {'recordVersion': 1, 'product': 'kownload-mcp',
                 'sourceCommit': COMMIT, 'version': 'fixture.39+' + COMMIT,
                 'readApiVersion': 1, 'architecture': 'arm64',
                 'sha256': hashlib.sha256(entry.read_bytes()).hexdigest()}
        return entry, value

    def marker_write(self, entry, raw):
        subprocess.run(['/usr/bin/xattr', '-wx', MCP_MARKER, raw.hex(), str(entry)],
                       check=True, capture_output=True)

    def marker_read(self, entry):
        return bytes.fromhex(subprocess.check_output(
            ['/usr/bin/xattr', '-px', MCP_MARKER, str(entry)], text=True))

    def rollback_module(self):
        spec = importlib.util.spec_from_file_location('rollback_marker_test', SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_marker_real_roundtrip_preserves_exact_bytes(self):
        entry, value = self.mcp_fixture()
        raw = b' \n' + json.dumps(value).encode() + b'\n'
        self.marker_write(entry, raw)
        original = tree(self.snapshot)
        p = self.run_pair('--apply')
        self.assertEqual(p.returncode, 0, p.stderr)
        copied = self.output / 'data/bin/kownload-mcp'
        self.assertEqual(self.marker_read(copied), raw)
        self.assertEqual(self.marker_read(entry), raw)
        self.assertEqual(copied.read_bytes(), entry.read_bytes())
        self.assertEqual(copied.stat().st_mode & 0o777, 0o700)
        self.assertEqual(tree(self.snapshot), original)
        self.assertTrue(json.loads(p.stdout)['pairVerified'])
        verify = subprocess.run([sys.executable, str(SCRIPT), '--snapshot-manifest',
                                 str(self.output / 'snapshot.json'), '--output',
                                 str(self.dir / 'second-offline-copy'), '--fixture'],
                                capture_output=True, text=True)
        self.assertEqual(verify.returncode, 0, verify.stderr)

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_missing_unknown_bad_marker_refuses_before_output(self):
        entry, good = self.mcp_fixture()
        for args in [[], ['--apply']]:
            p = self.run_pair(*args)
            self.assertNotEqual(p.returncode, 0)
            self.assertFalse(self.output.exists())
        bad = [b'{', b'null', b' ' * 2049,
               b'{"recordVersion":2,"recordVersion":1}',
               *[json.dumps({**good, **patch}).encode() for patch in [
                   {'recordVersion': 2}, {'recordVersion': True},
                   {'readApiVersion': 2}, {'readApiVersion': True},
                   {'architecture': 'x86_64'}, {'product': 'other'},
                   {'sourceCommit': 'BAD'}, {'version': 'unmatched-source'},
                   {'sha256': '0' * 64}, {'unknown': 'preserve'}]]]
        for index, raw in enumerate(bad):
            with self.subTest(index=index):
                self.marker_write(entry, raw)
                p = self.run_pair('--apply')
                self.assertNotEqual(p.returncode, 0)
                self.assertFalse(self.output.exists())
                self.assertEqual(self.marker_read(entry), raw)
                self.assertNotIn(raw.hex(), p.stderr)
        self.marker_write(entry, json.dumps(good).encode())
        entry.chmod(0o755)
        self.meta['data']['sha256'] = fixture_digest(self.data)
        self.save_manifest()
        self.assertNotEqual(self.run_pair('--apply').returncode, 0)
        self.assertFalse(self.output.exists())

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_marker_write_failure_never_publishes_plan(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        module = self.rollback_module()
        pair = module.verify_pair(self.manifest)
        with mock.patch.object(module, 'write_mcp_marker', side_effect=OSError('fixture')):
            with self.assertRaises(OSError):
                module.prepare_copy(self.manifest, self.output, pair)
        self.assertFalse((self.output / 'restore-plan.json').exists())
        self.assertEqual(self.marker_read(entry), raw)
        self.assert_unchanged()

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_special_mode_refuses_before_output(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        entry.chmod(0o4700)
        self.assertEqual(entry.stat().st_mode & 0o7777, 0o4700)
        self.meta['data']['sha256'] = fixture_digest(self.data)
        self.save_manifest()
        p = self.run_pair('--apply')
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(self.output.exists())
        self.assertEqual(self.marker_read(entry), raw)
        self.assertEqual(entry.stat().st_mode & 0o7777, 0o4700)

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_destination_link_swap_never_changes_its_target(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        other = self.dir / 'outside-staging'
        other.write_bytes(b'untouched outside staging')
        self.marker_write(other, b'untouched marker')
        module = self.rollback_module()
        pair = module.verify_pair(self.manifest)
        write_marker = module.write_mcp_marker
        def swapped_copy(path, original):
            path.rename(path.with_name('copied-before-swap'))
            path.symlink_to(other)
            write_marker(path, original)
        with mock.patch.object(module, 'write_mcp_marker', side_effect=swapped_copy):
            with self.assertRaises(ValueError):
                module.prepare_copy(self.manifest, self.output, pair)
        self.assertEqual(self.marker_read(other), b'untouched marker')
        self.assertEqual(other.read_bytes(), b'untouched outside staging')
        self.assertEqual(self.marker_read(entry), raw)
        self.assertFalse((self.output / 'restore-plan.json').exists())

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_marker_read_failure_refuses_before_copy(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        module = self.rollback_module()
        pair = module.verify_pair(self.manifest)
        with mock.patch.object(module, 'read_mcp_marker', side_effect=OSError('fixture')):
            with self.assertRaises(OSError):
                module.prepare_copy(self.manifest, self.output, pair)
        self.assertFalse(self.output.exists())
        self.assertEqual(self.marker_read(entry), raw)

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_marker_postwrite_mismatch_never_publishes_plan(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        module = self.rollback_module()
        pair = module.verify_pair(self.manifest)
        write_marker = module.write_mcp_marker
        def changed_copy(path, original):
            write_marker(path, original + b' ')
        with mock.patch.object(module, 'write_mcp_marker', side_effect=changed_copy):
            with self.assertRaises(ValueError):
                module.prepare_copy(self.manifest, self.output, pair)
        self.assertFalse((self.output / 'restore-plan.json').exists())
        self.assertEqual(self.marker_read(entry), raw)

    @unittest.skipUnless(sys.platform == 'darwin', 'MCP install marker is macOS-only')
    def test_mcp_source_marker_changed_during_copy_refuses_plan(self):
        entry, value = self.mcp_fixture()
        raw = json.dumps(value).encode()
        self.marker_write(entry, raw)
        module = self.rollback_module()
        pair = module.verify_pair(self.manifest)
        copy = module.shutil.copytree
        def concurrent_source(source, destination, *args, **kwargs):
            result = copy(source, destination, *args, **kwargs)
            if source == self.data:
                self.marker_write(entry, raw + b' ')
            return result
        with mock.patch.object(module.shutil, 'copytree', side_effect=concurrent_source):
            with self.assertRaises(ValueError):
                module.prepare_copy(self.manifest, self.output, pair)
        self.assertFalse((self.output / 'restore-plan.json').exists())


if __name__ == '__main__':
    unittest.main()
