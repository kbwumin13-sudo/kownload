#!/usr/bin/env python3
"""Collect licenses from the exact Go modules in compiled binaries and SDKs.

The Flutter build emits its own NOTICES.Z. Native Pod acknowledgements are
included separately. Missing module legal material stops packaging.
"""
import argparse
import json
import pathlib
import re
import shutil
import subprocess


def run(*args, cwd=None):
    return subprocess.check_output(list(map(str, args)), cwd=cwd, text=True)


def binary_modules(go, binary):
    modules = []
    for line in run(go, 'version', '-m', binary).splitlines():
        fields = line.strip().split('\t')
        if fields[0] == 'dep':
            modules.append({'module': fields[1], 'version': fields[2],
                            'sum': fields[3] if len(fields) > 3 else ''})
    return modules


def module_source(go, repo, module):
    name, version, checksum = module['module'], module['version'], module['sum']
    # Nested dependencies may not occur in the root go.sum. Use the reviewed
    # lock that owns both checksums, instead of updating the root or requiring
    # a fresh sumdb lookup for an already locked nested module.
    for directory in [repo, repo / 'cmd/kownload-mcp']:
        lock = directory / 'go.sum'
        if not lock.is_file():
            continue
        lines = lock.read_text().splitlines()
        if (not checksum or f'{name} {version} {checksum}' not in lines or
                not any(line.startswith(f'{name} {version}/go.mod h1:') for line in lines)):
            continue
        meta = json.loads(run(go, 'mod', 'download', '-json', name + '@' + version, cwd=directory))
        if ('Error' in meta or meta.get('Path') != name or
                meta.get('Version') != version or meta.get('Sum') != checksum):
            raise RuntimeError('Go module source differs from binary: ' + name)
        return meta
    raise RuntimeError('Go module has no matching source lock: ' + name + '@' + version)


def collect(repo, destination, binaries, go, flutter):
    destination.mkdir(parents=True, exist_ok=True)
    for name in ['LICENSE', 'THIRD_PARTY_NOTICES.md']:
        shutil.copy2(repo / name, destination / name)
    shutil.copytree(repo / 'third_party', destination / 'third_party', dirs_exist_ok=True)
    modules = {}
    for binary in binaries:
        for module in binary_modules(go, binary):
            modules[(module['module'], module['version'])] = module['sum']
    index = []
    for (name, version), checksum in sorted(modules.items()):
        meta = module_source(go, repo, {'module': name, 'version': version, 'sum': checksum})
        root = pathlib.Path(meta['Dir'])
        legal = [p for p in root.iterdir() if p.is_file() and
                 re.match(r'(?i)(licen[sc]e|copying|notice|copyright|patents)', p.name)]
        # Some Go x modules keep their BSD license in a parent source folder.
        if not legal:
            raise RuntimeError('Missing Go license: ' + name + '@' + version)
        target = destination / 'go' / (name.replace('/', '_') + '@' + version)
        target.mkdir(parents=True, exist_ok=True)
        for file in legal:
            shutil.copy2(file, target / file.name)
        escaped = ''.join('!' + c.lower() if c.isupper() else c for c in name)
        index.append({'module': name, 'version': version, 'sum': checksum,
                      'source': 'https://proxy.golang.org/' + escaped + '/@v/' + version + '.zip',
                      'notices': [str(p.relative_to(destination)) for p in target.iterdir()]})
    (destination / 'go-modules.json').write_text(json.dumps(index, indent=2) + '\n')
    sdk = destination / 'sdk'
    sdk.mkdir(exist_ok=True)
    shutil.copy2(pathlib.Path(run(go, 'env', 'GOROOT').strip()) / 'LICENSE', sdk / 'Go-LICENSE')
    shutil.copy2(flutter / 'LICENSE', sdk / 'Flutter-LICENSE')
    dart = flutter / 'bin/cache/dart-sdk/LICENSE'
    if dart.exists():
        shutil.copy2(dart, sdk / 'Dart-LICENSE')
    pods = repo / 'ui/flutter/macos/Pods/Target Support Files/Pods-Runner/Pods-Runner-acknowledgements.plist'
    if not pods.exists():
        raise RuntimeError('Missing native CocoaPods acknowledgements')
    shutil.copy2(pods, destination / 'CocoaPods-acknowledgements.plist')
    return index


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('--repo', type=pathlib.Path, required=True)
    p.add_argument('--destination', type=pathlib.Path, required=True)
    p.add_argument('--go', required=True)
    p.add_argument('--flutter', type=pathlib.Path, required=True)
    p.add_argument('binaries', type=pathlib.Path, nargs='+')
    a = p.parse_args()
    print('Go modules with notices:', len(collect(a.repo, a.destination, a.binaries, a.go, a.flutter)))
