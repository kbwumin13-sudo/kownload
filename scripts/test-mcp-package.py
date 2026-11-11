#!/usr/bin/env python3
"""Package behavior checks, with real temporary bytes and mocked signing tools."""
import hashlib
import importlib.util
import io
import json
import pathlib
import shutil
import tarfile
import tempfile
import unittest
from unittest import mock


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, pathlib.Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


build = load('kownload_build_test', 'build-macos.py')
notices = load('kownload_notice_test', 'collect-macos-notices.py')


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='k14-mcp-package-')
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.app = self.root / 'kownload.app'
        self.helper = self.app / build.MCP_HELPER
        self.helper.parent.mkdir(parents=True)
        (self.app / 'Contents/Resources').mkdir()
        self.helper.write_bytes(b'\xcf\xfa\xed\xfeMCP fixture')
        self.manifest = {'sourceCommit': 'a' * 40, 'releaseVersion': '1.9.3-kownload.26',
                         'architecture': 'arm64', 'preview': True}
        self.calls = []
        self.verified = False
        self.break_outer = False

    def signer(self, *args, **kwargs):
        self.calls.append(args)
        if args[0] != 'codesign':
            return
        target = pathlib.Path(args[-1])
        if '--force' in args:
            self.assertNotIn('--deep', args)
            if target == self.helper:
                self.helper.write_bytes(self.helper.read_bytes() + b'signed bytes')
            if target == self.app:
                metadata = json.loads((self.app / 'Contents/Resources/build-manifest.json').read_text())
                self.assertEqual(metadata['mcp']['sha256'], hashlib.sha256(self.helper.read_bytes()).hexdigest())
                if self.break_outer:
                    self.helper.write_bytes(self.helper.read_bytes() + b'late recursive change')
        elif '--verify' in args and target == self.app:
            self.verified = True

    def version(self, *args, **kwargs):
        self.assertTrue(self.verified, 'must verify App before executing packaged version')
        self.assertEqual(args, (self.helper, '--version'))
        return 'kownload-mcp ' + build.mcp_version(self.manifest['releaseVersion'], self.manifest['sourceCommit']) + ' read-api=1'

    def seal(self):
        with mock.patch.object(build, 'run', self.signer), mock.patch.object(build, 'output', self.version):
            build.seal_candidate(self.app, self.root / 'entitlements', self.manifest, build.MCP_SDK_VERSION)

    def test_hash_is_after_inner_signing_and_version_after_outer_verification(self):
        old = hashlib.sha256(self.helper.read_bytes()).hexdigest()
        self.seal()
        self.assertNotEqual(self.manifest['mcp']['sha256'], old)
        self.assertEqual(self.manifest['mcp']['sha256'], hashlib.sha256(self.helper.read_bytes()).hexdigest())
        self.assertEqual(self.manifest['mcp']['sourceCommit'], self.manifest['sourceCommit'])
        self.assertEqual(self.manifest['mcp']['readApiVersion'], 1)

    def test_unexpected_outer_resigning_is_detected_before_version_execution(self):
        self.break_outer = True
        with self.assertRaisesRegex(RuntimeError, 'digest differs'):
            self.seal()

    def test_final_image_bytes_are_checked_again(self):
        self.seal()
        mounted = self.root / 'mounted.app'
        shutil.copytree(self.app, mounted)
        metadata = json.loads((mounted / 'Contents/Resources/build-manifest.json').read_text())
        build.verify_packaged_mcp(mounted, metadata)
        (mounted / build.MCP_HELPER).write_bytes(b'changed image bytes')
        with self.assertRaisesRegex(RuntimeError, 'digest differs'):
            build.verify_packaged_mcp(mounted, metadata)

    def test_unknown_helper_path_is_not_executed(self):
        self.manifest['mcp'] = build.mcp_metadata(self.app, self.manifest, build.MCP_SDK_VERSION)
        self.manifest['mcp']['helperPath'] = '../unknown'
        with mock.patch.object(build, 'output') as output:
            with self.assertRaises(RuntimeError):
                build.verify_packaged_mcp(self.app, self.manifest, execute_version=True)
            output.assert_not_called()

    def test_symlink_helper_and_parents_refuse(self):
        target = self.root / 'elsewhere'
        self.helper.rename(target)
        self.helper.symlink_to(target)
        with self.assertRaisesRegex(RuntimeError, 'ordinary'):
            build.mcp_metadata(self.app, self.manifest, build.MCP_SDK_VERSION)
        self.helper.unlink()
        self.helper.parent.rmdir()
        self.helper.parent.symlink_to(self.root)
        with self.assertRaisesRegex(RuntimeError, 'ordinary'):
            build.mcp_metadata(self.app, self.manifest, build.MCP_SDK_VERSION)

    def test_incompatible_version_or_sdk_refuses(self):
        self.manifest['mcp'] = build.mcp_metadata(self.app, self.manifest, build.MCP_SDK_VERSION)
        with mock.patch.object(build, 'output', return_value='kownload-mcp old read-api=1'):
            with self.assertRaisesRegex(RuntimeError, 'version differs'):
                build.verify_packaged_mcp(self.app, self.manifest, execute_version=True)
        with self.assertRaisesRegex(RuntimeError, 'SDK'):
            build.mcp_metadata(self.app, self.manifest, 'v9.0.0')

    def test_existing_non_mcp_package_does_not_claim_or_execute_mcp(self):
        self.helper.unlink()
        def sign(*args, **kwargs):
            self.calls.append(args)
        with mock.patch.object(build, 'run', sign), mock.patch.object(build, 'output') as output:
            build.seal_candidate(self.app, self.root / 'entitlements', self.manifest)
            output.assert_not_called()
        self.assertNotIn('mcp', self.manifest)

    def test_nested_dependencies_and_root_sources_share_complete_archive(self):
        deps = [{'module': 'example.invalid/core', 'version': 'v1.0.0'},
                {'module': 'github.com/modelcontextprotocol/go-sdk', 'version': 'v1.7.0'}]
        nested = {'modulePath': 'cmd/kownload-mcp', 'modules': [deps[1]]}
        zips = {}
        for i, dep in enumerate(deps):
            z = self.root / f'module-{i}.zip'
            z.write_bytes(b'original source zip ' + dep['module'].encode())
            zips[dep['module'] + '@' + dep['version']] = str(z)
        def metadata(*args, **kwargs):
            return json.dumps({'Zip': zips[args[-1]]})
        archive = self.root / 'source.tar.gz'
        with tarfile.open(archive, 'w:gz') as bundle, mock.patch.object(build, 'output', metadata):
            original = b'original tracked source'
            entry = tarfile.TarInfo('cmd/kownload-mcp/go.mod')
            entry.size = len(original)
            bundle.addfile(entry, io.BytesIO(original))
            build.add_module_sources(bundle, deps, 'go', self.root, nested)
        with tarfile.open(archive) as bundle:
            self.assertEqual(bundle.extractfile('cmd/kownload-mcp/go.mod').read(), original)
            self.assertEqual(json.load(bundle.extractfile('mcp-go-modules.json')), nested)
            self.assertEqual(json.load(bundle.extractfile('go-modules.json')), deps)
            archived = [bundle.extractfile(m).read() for m in bundle.getmembers() if m.name.endswith('.zip')]
            self.assertCountEqual(archived, [pathlib.Path(p).read_bytes() for p in zips.values()])

    def test_binary_index_includes_nested_sdk(self):
        raw = 'binary:\n\tpath\tkownload-mcp\n\tdep\tgithub.com/modelcontextprotocol/go-sdk\tv1.7.0\th1:fixture\n'
        with mock.patch.object(notices, 'run', return_value=raw):
            self.assertEqual(notices.binary_modules('go', self.helper),
                             [{'module': 'github.com/modelcontextprotocol/go-sdk', 'version': 'v1.7.0', 'sum': 'h1:fixture'}])


if __name__ == '__main__':
    unittest.main()
