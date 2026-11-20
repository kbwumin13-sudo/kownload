#!/usr/bin/env python3
"""Clone a macOS App under a distinct identity and insert a speedbench core."""
import argparse
import pathlib
import plistlib
import shutil
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=pathlib.Path)
    parser.add_argument('core', type=pathlib.Path)
    parser.add_argument('output', type=pathlib.Path)
    args = parser.parse_args()
    if args.output.suffix != '.app':
        parser.error('output must be a new .app bundle')
    # Reserve a new path. In particular, an installed app is never overwritten.
    args.output.mkdir()
    subprocess.run(['ditto', args.source, args.output], check=True)
    info_path = args.output / 'Contents/Info.plist'
    info = plistlib.loads(info_path.read_bytes())
    info['CFBundleIdentifier'] += '.speedbench'
    info.pop('CFBundleURLTypes', None)
    info.pop('CFBundleDocumentTypes', None)
    info_path.write_bytes(plistlib.dumps(info))
    dylib = args.output / 'Contents/Frameworks/libgopeed.dylib'
    shutil.copyfile(args.core, dylib, follow_symlinks=False)
    subprocess.run(['codesign', '--force', '--sign', '-', dylib], check=True)
    subprocess.run(['codesign', '--force', '--sign', '-', args.output], check=True)
    subprocess.run(['codesign', '--verify', '--deep', '--strict', args.output], check=True)


if __name__ == '__main__':
    main()
