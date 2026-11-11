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

    def test_go_mcp_skips_apple_strip_but_keeps_signing_and_other_stripping(self):
        executable = self.app / 'Contents/MacOS/kownload'
        executable.parent.mkdir()
        executable.write_bytes(b'\xcf\xfa\xed\xfeApp fixture')
        sign = self.signer

        def reject_mcp_strip(*args, **kwargs):
            if args[:2] == ('xcrun', 'strip') and args[-1] == self.helper:
                raise RuntimeError('indirect symbols cannot be stripped')
            sign(*args, **kwargs)

        with mock.patch.object(build, 'run', reject_mcp_strip), mock.patch.object(build, 'output', self.version):
            build.seal_candidate(self.app, self.root / 'entitlements', self.manifest, build.MCP_SDK_VERSION)
        self.assertIn(('xcrun', 'strip', '-S', '-x', executable), self.calls)
        self.assertIn(('codesign', '--force', '--sign', '-', self.helper), self.calls)
        self.assertTrue(self.verified)

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
        deps = [{'module': 'example.invalid/core', 'version': 'v1.0.0', 'sum': 'h1:core'},
                {'module': 'github.com/modelcontextprotocol/go-sdk', 'version': 'v1.7.0', 'sum': 'h1:sdk'}]
        nested = {'modulePath': 'cmd/kownload-mcp', 'modules': [deps[1]]}
        zips = {}
        for i, dep in enumerate(deps):
            z = self.root / f'module-{i}.zip'
            z.write_bytes(b'original source zip ' + dep['module'].encode())
            zips[dep['module'] + '@' + dep['version']] = str(z)
            context = self.root if i == 0 else self.root / 'cmd/kownload-mcp'
            context.mkdir(parents=True, exist_ok=True)
            (context / 'go.sum').write_text(f"{dep['module']} {dep['version']} {dep['sum']}\n{dep['module']} {dep['version']}/go.mod h1:mod\n")
        def metadata(*args, **kwargs):
            i = 0 if args[-1].startswith('example.invalid/core@') else 1
            dep = deps[i]
            self.assertEqual(kwargs['cwd'], self.root if i == 0 else self.root / 'cmd/kownload-mcp')
            return json.dumps({'Path': dep['module'], 'Version': dep['version'], 'Sum': dep['sum'], 'Zip': zips[args[-1]]})
        archive = self.root / 'source.tar.gz'
        with tarfile.open(archive, 'w:gz') as bundle, mock.patch.object(build, 'load', return_value=notices), mock.patch.object(notices, 'run', metadata):
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

    def test_notices_use_nested_locked_checksums_without_mutating_root(self):
        nested = self.root / 'cmd/kownload-mcp'
        nested.mkdir(parents=True)
        module = 'example.invalid/nested'
        lock = f'{module} v1.0.0 h1:fixture\n{module} v1.0.0/go.mod h1:mod\n'
        (nested / 'go.sum').write_text(lock)
        (self.root / 'go.sum').write_text('# unchanged root lock\n')
        (self.root / 'third_party').mkdir()
        for name in ['LICENSE', 'THIRD_PARTY_NOTICES.md']:
            (self.root / name).write_text(name)
        dep = self.root / 'module'; dep.mkdir(); (dep / 'LICENSE').write_text('original license')
        goroot = self.root / 'go'; goroot.mkdir(); (goroot / 'LICENSE').write_text('Go license')
        flutter = self.root / 'flutter'; flutter.mkdir(); (flutter / 'LICENSE').write_text('Flutter license')
        pods = self.root / 'ui/flutter/macos/Pods/Target Support Files/Pods-Runner/Pods-Runner-acknowledgements.plist'
        pods.parent.mkdir(parents=True); pods.write_text('native acknowledgements')

        def query(*args, **kwargs):
            if args[1:3] == ('version', '-m'):
                return f'binary:\n\tdep\t{module}\tv1.0.0\th1:fixture\n'
            if args[1:4] == ('mod', 'download', '-json'):
                self.assertEqual(kwargs['cwd'], nested)
                return json.dumps({'Path': module, 'Version': 'v1.0.0', 'Sum': 'h1:fixture', 'Dir': str(dep)})
            self.assertEqual(args[1:], ('env', 'GOROOT'))
            return str(goroot)

        with mock.patch.object(notices, 'run', query):
            result = notices.collect(self.root, self.root / 'legal', [self.helper], 'go', flutter)
        self.assertEqual(result[0]['sum'], 'h1:fixture')
        self.assertEqual((self.root / 'go.sum').read_text(), '# unchanged root lock\n')
        self.assertEqual((nested / 'go.sum').read_text(), lock)

    def test_unknown_module_lock_and_mismatched_source_refuse(self):
        dep = {'module': 'example.invalid/core', 'version': 'v1.0.0', 'sum': 'h1:expected'}
        with mock.patch.object(notices, 'run') as query:
            with self.assertRaisesRegex(RuntimeError, 'no matching source lock'):
                notices.module_source('go', self.root, dep)
            query.assert_not_called()
        (self.root / 'go.sum').write_text('example.invalid/core v1.0.0 h1:expected\nexample.invalid/core v1.0.0/go.mod h1:mod\n')
        for patch in [{'Path': 'other'}, {'Version': 'v2.0.0'}, {'Sum': 'h1:other'}, {'Error': 'unavailable'}]:
            meta = {'Path': dep['module'], 'Version': dep['version'], 'Sum': dep['sum'], **patch}
            with self.subTest(patch=patch), mock.patch.object(notices, 'run', return_value=json.dumps(meta)):
                with self.assertRaisesRegex(RuntimeError, 'differs from binary'):
                    notices.module_source('go', self.root, dep)


if __name__ == '__main__':
    unittest.main()
