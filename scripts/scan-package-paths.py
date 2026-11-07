#!/usr/bin/env python3
"""Scan all ordinary package files; report locations, never personal path values."""
import argparse
import json
import pathlib
import re


def scan(root):
    findings = []
    count = 0
    for file in sorted(root.rglob('*')):
        if not file.is_file() or file.is_symlink():
            continue
        data = file.read_bytes()
        count += 1
        # UTF-8 strings as well as Windows and Unix home-path forms.
        patterns = [rb'/Users/[^/\s\x00"<>]+/', rb'/home/[^/\s\x00"<>]+/',
                    rb'[A-Za-z]:\\Users\\[^\\\s\x00"<>]+\\']
        offsets = []
        for pattern in patterns:
            offsets.extend(m.start() for m in re.finditer(pattern, data))
        if offsets:
            findings.append({'file': str(file.relative_to(root)),
                             'count': len(offsets), 'offsets': sorted(offsets)[:20]})
    return {'filesScanned': count, 'personalPaths': sum(x['count'] for x in findings),
            'findings': findings}


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('root', type=pathlib.Path)
    p.add_argument('--json', type=pathlib.Path, required=True)
    a = p.parse_args()
    result = scan(a.root)
    a.json.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({k: result[k] for k in ['filesScanned', 'personalPaths']}))
    raise SystemExit(1 if result['personalPaths'] else 0)
