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

SELF_MODULE = 'github.com/GopeedLab/gopeed'
MCP_MODULE = 'github.com/kbwumin13-sudo/kownload/cmd/kownload-mcp'


def run(*args, cwd=None):
    return subprocess.check_output(list(map(str, args)), cwd=cwd, text=True)


def binary_modules(go, binary):
    modules = []
    current = None
    for line in run(go, 'version', '-m', binary).splitlines():
        fields = line.strip().split('\t')
        if fields[0] == 'dep':
            if len(fields) not in [3, 4]:
                raise RuntimeError('Invalid Go binary dependency metadata')
            current = {'module': fields[1], 'version': fields[2],
                       'sum': fields[3] if len(fields) > 3 else ''}
            modules.append(current)
        elif fields[0] == '=>':
            if current is None or 'replace' in current or len(fields) not in [3, 4]:
                raise RuntimeError('Invalid Go binary replacement metadata')
            current['replace'] = {'path': fields[1], 'version': fields[2],
                                  'sum': fields[3] if len(fields) > 3 else ''}
        elif fields[0]:
            current = None
    return modules


def repository_source(go, repo, module, source_commit):
    if (module['module'] != SELF_MODULE or module['version'] != 'v0.0.0' or
            module['sum'] != '' or module.get('replace') !=
            {'path': '../..', 'version': '(devel)', 'sum': ''} or
            not isinstance(source_commit, str) or
            not re.fullmatch('[0-9a-f]{40}', source_commit)):
        raise RuntimeError('Unsupported Go module replacement')
    repo = pathlib.Path(repo)
    nested = repo / 'cmd/kownload-mcp'
    for directory in [repo, repo / 'cmd', nested]:
        if directory.is_symlink() or not directory.is_dir():
            raise RuntimeError('Repository replacement directory differs')
    for directory, wanted in [(repo, SELF_MODULE), (nested, MCP_MODULE)]:
        mod = directory / 'go.mod'
        if mod.is_symlink() or not mod.is_file():
            raise RuntimeError('Repository module identity is missing')
        paths = re.findall(r'(?m)^module\s+(\S+)[ \t]*$', mod.read_text())
        if paths != [wanted]:
            raise RuntimeError('Repository module identity differs')
    meta = json.loads(run(go, 'list', '-m', '-mod=readonly', '-json',
                          SELF_MODULE, cwd=nested))
    replacement = meta.get('Replace', {})
    root = repo.resolve(strict=True)
    if (meta.get('Path') != SELF_MODULE or meta.get('Version') != 'v0.0.0' or
            replacement.get('Path') != '../..' or replacement.get('Version') or
            meta.get('Sum') or replacement.get('Sum') or
            not replacement.get('Dir') or not meta.get('Dir') or
            pathlib.Path(replacement['Dir']).resolve(strict=True) != root or
            pathlib.Path(meta['Dir']).resolve(strict=True) != root or
            not replacement.get('GoMod') or not meta.get('GoMod') or
            pathlib.Path(replacement['GoMod']).resolve(strict=True) != root / 'go.mod' or
            pathlib.Path(meta['GoMod']).resolve(strict=True) != root / 'go.mod'):
        raise RuntimeError('Repository replacement does not use this source root')
    return {'Path': SELF_MODULE, 'Version': 'v0.0.0', 'Dir': str(root),
            'SourceKind': 'repository', 'SourceCommit': source_commit,
            'SourcePath': '.'}


def module_source(go, repo, module, source_commit=None):
    name, version, checksum = module['module'], module['version'], module['sum']
    if name == SELF_MODULE or 'replace' in module:
        return repository_source(go, repo, module, source_commit)
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


def collect(repo, destination, binaries, go, flutter, source_commit=None):
    destination.mkdir(parents=True, exist_ok=True)
    for name in ['LICENSE', 'THIRD_PARTY_NOTICES.md']:
        shutil.copy2(repo / name, destination / name)
    shutil.copytree(repo / 'third_party', destination / 'third_party', dirs_exist_ok=True)
    modules = {}
    for binary in binaries:
        for module in binary_modules(go, binary):
            key = (module['module'], module['version'])
            if key in modules and modules[key] != module:
                raise RuntimeError('Go binaries use different module sources')
            modules[key] = module
    index = []
    for (name, version), module in sorted(modules.items()):
        checksum = module['sum']
        meta = module_source(go, repo, module, source_commit)
        if meta.get('SourceKind') == 'repository':
            index.append({**module, 'sourceKind': 'repository',
                          'sourceCommit': meta['SourceCommit'], 'sourcePath': '.',
                          'source': '.', 'notices': ['LICENSE', 'THIRD_PARTY_NOTICES.md']})
            continue
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
    p.add_argument('--source-commit')
    p.add_argument('binaries', type=pathlib.Path, nargs='+')
    a = p.parse_args()
    print('Go modules with notices:', len(collect(
        a.repo, a.destination, a.binaries, a.go, a.flutter, a.source_commit)))
