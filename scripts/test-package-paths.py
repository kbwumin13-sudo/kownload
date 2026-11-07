#!/usr/bin/env python3
import gzip
import importlib.util
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('path_scan', pathlib.Path(__file__).with_name('scan-package-paths.py'))
scanner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scanner)


class PackagePaths(unittest.TestCase):
    def test_absolute_home_paths_including_compressed_notices(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'binary').write_bytes(b'\x00/Users/fixture/project\x00file:///home/fixture/file\x00')
            (root / 'NOTICES.Z').write_bytes(gzip.compress(b'file:///Users/fixture/plugin.swift'))
            self.assertEqual(scanner.scan(root)['personalPaths'], 3)

    def test_package_uri_home_module_is_not_a_machine_home(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'binary').write_bytes(b'package:gopeed/app/modules/home/views/home_view.dart\x00')
            self.assertEqual(scanner.scan(root)['personalPaths'], 0)


if __name__ == '__main__':
    unittest.main()
