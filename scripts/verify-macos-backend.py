"""Verify staged backend payloads, allowing only Mach-O signing changes."""
import argparse
import hashlib
import json
import pathlib
import struct
import sys


def payload_hash(path):
    data = bytearray(path.read_bytes())
    # LC_CODE_SIGNATURE and its signature blob are signing metadata. Everything
    # else, including executable text and data, must remain byte-for-byte equal.
    if data[:4] == b'\xcf\xfa\xed\xfe':
        commands = struct.unpack_from('<I', data, 16)[0]
        offset = 32
        for _ in range(commands):
            command, size = struct.unpack_from('<II', data, offset)
            if command == 0x1D:
                blob_offset, blob_size = struct.unpack_from('<II', data, offset + 8)
                data[offset:offset + size] = b'\0' * size
                data[blob_offset:blob_offset + blob_size] = b'\0' * blob_size
            offset += size
    return hashlib.sha256(data).hexdigest()


parser = argparse.ArgumentParser()
parser.add_argument('baseline', type=pathlib.Path)
parser.add_argument('target', type=pathlib.Path)
parser.add_argument('--media-core', type=pathlib.Path)
args = parser.parse_args()
baseline, target = args.baseline, args.target
original = baseline.parent / 'Gopeed.app'
entries = json.loads(baseline.read_text())['files']
results = []
for rel, expected in entries.items():
    if rel.startswith('Contents/MacOS/'):
        continue
    src, dest = original / rel, target / rel
    if args.media_core:
        if rel == 'Contents/Frameworks/libgopeed.dylib':
            src = args.media_core / 'libgopeed.dylib'
        elif rel.endswith('/assets/exec/host'):
            src = args.media_core / 'host'
    same = dest.exists() and payload_hash(src) == payload_hash(dest)
    results.append({'path': rel, 'payload_unchanged': same, 'expected_source': str(src),
                    'bytes_identical': dest.exists() and hashlib.sha256(dest.read_bytes()).hexdigest() == expected['sha256']})
print(json.dumps(results, indent=2))
if not all(row['payload_unchanged'] for row in results):
    sys.exit(1)
