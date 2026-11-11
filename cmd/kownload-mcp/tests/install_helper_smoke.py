#!/usr/bin/env python3
"""Exercise the real installer, signed synthetic bundles and actual mcporter.
Usage: python3 tests/install_helper_smoke.py /absolute/go
All deployment/config/socket paths are temporary; no actual App is launched.
The two synthetic source IDs are fixture labels, not release provenance.
"""
import hashlib
import http.server
import json
import os
from pathlib import Path
import plistlib
import select
import shutil
import socketserver
import subprocess
import sys
import tempfile
import threading

from mcporter_smoke import PREFIX, PROGRESS, TASK, TOOLS

XATTR = 'io.github.kbwumin13-sudo.kownload.mcp-install'
MODULE = Path(__file__).resolve().parent.parent


def run(argv, env=None, cwd=None):
    result = subprocess.run([str(a) for a in argv], env=env, cwd=cwd,
                            capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise AssertionError((argv[0], result.returncode, result.stdout, result.stderr))
    return result.stdout


def make_bundle(root, go, digit):
    app = root / ('Fixture' + digit + '.app')
    contents = app / 'Contents'
    helper = contents / 'Helpers' / 'kownload-mcp'
    resources = contents / 'Resources'
    macos = contents / 'MacOS'
    for directory in [helper.parent, resources, macos]:
        directory.mkdir(parents=True, mode=0o700)
    source = digit * 40
    version = '0.0.0-fixture+' + source
    run([go, 'build', '-trimpath', '-ldflags', '-X main.version=' + version,
         '-o', helper, '.'], cwd=MODULE)
    shutil.copyfile(helper, macos / 'Fixture')
    os.chmod(macos / 'Fixture', 0o700)
    (contents / 'Info.plist').write_bytes(plistlib.dumps(dict(
        CFBundleIdentifier='io.kownload.mcp.install-fixture', CFBundleExecutable='Fixture',
        CFBundleName='Fixture', CFBundleVersion='1', CFBundlePackageType='APPL')))
    for binary in [helper, macos / 'Fixture']:
        run(['/usr/bin/codesign', '--force', '--sign', '-', binary])
    sha = hashlib.sha256(helper.read_bytes()).hexdigest()
    metadata = dict(sourceCommit=source, releaseVersion='0.0.0-fixture', buildNumber=1,
                    architecture='arm64', preview=True, mcp=dict(
                        manifestVersion=1, helperPath='Contents/Helpers/kownload-mcp',
                        version=version, sourceCommit=source, readApiVersion=1,
                        sdkVersion='v1.7.0', architecture='arm64', sha256=sha))
    (resources / 'build-manifest.json').write_text(json.dumps(metadata))
    run(['/usr/bin/codesign', '--force', '--sign', '-', app])
    run(['/usr/bin/codesign', '--verify', '--deep', '--strict', app])
    return app, metadata


def main():
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    go = Path(sys.argv[1]).resolve(strict=True)
    mcporter = shutil.which('mcporter')
    assert mcporter, 'existing real mcporter required'
    seen = []
    unexpected = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            seen.append(self.path)
            assert self.headers.get('X-Kownload-Read-Version') == '1'
            if self.path == PREFIX + '?limit=1':
                data = dict(tasks=[TASK], nextCursor='')
            elif self.path == PREFIX + '/task_1':
                data = TASK
            elif self.path == PREFIX + '/task_1/progress':
                data = PROGRESS
            elif self.path in [PREFIX + '/task_1/stats', PREFIX + '/task_1/errors']:
                data = dict(available=False)
            else:
                unexpected.append(self.path)
                self.send_error(404)
                return
            body = json.dumps(dict(version=1, data=data)).encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(body)))
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            unexpected.append('POST')
            self.send_error(405)

    class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True

    with tempfile.TemporaryDirectory(prefix='kmcp-install-', dir='/private/tmp') as tmp:
        root = Path(tmp)
        bundles = [make_bundle(root, go, digit) for digit in ['1', '2']]
        installer = bundles[1][0] / 'Contents/Helpers/kownload-mcp'
        home = root / 'home'
        home.mkdir(mode=0o700)
        data_root = home / 'Library/Application Support/io.github.kbwumin13-sudo.kownload'
        data_root.mkdir(parents=True, mode=0o700)
        stable = data_root / 'bin/kownload-mcp'
        socket = root / 'api.sock'
        server = Server(str(socket), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        env = dict(os.environ, HOME=str(home), XDG_CONFIG_HOME=str(home / '.config'))
        config = root / 'client.json'
        config.write_text(json.dumps(dict(imports=[], mcpServers=dict(kownload=dict(
            command=str(stable), args=['--socket', str(socket)])))))
        config_bytes = config.read_bytes()
        old = None
        try:
            def install(bundle, success=True):
                p = subprocess.run([str(installer), 'install-helper', '--bundle', str(bundle),
                                    '--data-root', str(data_root)], env=env, cwd=root,
                                   capture_output=True, text=True, timeout=30)
                assert (p.returncode == 0) == success, (p.returncode, p.stdout, p.stderr)
                if not success:
                    assert p.stdout == '' and p.stderr == 'kownload-mcp: session ended\n'

            def mcporter_check():
                def invoke(*args):
                    return json.loads(run([mcporter, '--config', config, *args], env=env, cwd=root))
                listing = invoke('list', 'kownload', '--schema', '--json')
                tools = listing['tools']
                assert sorted(t['name'] for t in tools) == sorted(TOOLS), listing
                for index, name in enumerate(TOOLS):
                    args = dict(limit=1) if index == 0 else dict(id='task_1')
                    result = invoke('call', 'kownload.' + name, '--args', json.dumps(args), '--output', 'json')
                    if index == 0:
                        assert result == dict(tasks=[TASK], nextCursor='')
                    elif index == 1:
                        assert result == TASK
                    elif index == 2:
                        assert result == PROGRESS
                    else:
                        assert result == dict(available=False)

            install(bundles[0][0])
            before = stable.stat()
            before_bytes = stable.read_bytes()
            before_record = run(['/usr/bin/xattr', '-p', XATTR, stable]).rstrip('\n')
            old = subprocess.Popen([str(stable), '--socket', str(socket)], env=env,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   bufsize=0)

            def frame(message):
                old.stdin.write(json.dumps(message).encode() + b'\n')
                old.stdin.flush()
                if 'id' not in message:
                    return None
                assert select.select([old.stdout], [], [], 10)[0], 'old process stopped responding'
                return json.loads(old.stdout.readline())

            init = frame(dict(jsonrpc='2.0', id=1, method='initialize', params=dict(
                protocolVersion='2026-07-28', capabilities={}, clientInfo=dict(name='fixture', version='1'))))
            assert init['result']['serverInfo']['version'] == bundles[0][1]['mcp']['version']
            frame(dict(jsonrpc='2.0', method='notifications/initialized'))
            mcporter_check()

            # Corrupt a sealed candidate: actual codesign rejects it and the old
            # installed inode/bytes/xattr must all remain untouched.
            bad = root / 'Invalid.app'
            shutil.copytree(bundles[1][0], bad)
            with (bad / 'Contents/Helpers/kownload-mcp').open('ab') as f:
                f.write(b'corrupted-signature')
            install(bad, False)
            assert stable.stat().st_ino == before.st_ino
            assert stable.read_bytes() == before_bytes and run(['/usr/bin/xattr', '-p', XATTR, stable]).rstrip('\n') == before_record

            install(bundles[1][0])
            assert stable.stat().st_ino != before.st_ino and stable.stat().st_mode & 0o7777 == 0o700
            record = json.loads(run(['/usr/bin/xattr', '-p', XATTR, stable]).rstrip('\n'))
            assert record['sha256'] == hashlib.sha256(stable.read_bytes()).hexdigest()
            assert record['sourceCommit'] == '2' * 40
            assert old.poll() is None
            result = frame(dict(jsonrpc='2.0', id=2, method='tools/list', params={}))
            assert sorted(t['name'] for t in result['result']['tools']) == sorted(TOOLS)
            result = frame(dict(jsonrpc='2.0', id=3, method='tools/call', params=dict(
                name='kownload_get_progress', arguments=dict(id='task_1'))))
            assert result['result']['structuredContent'] == PROGRESS
            assert run([stable, '--version'], env=env).strip() == 'kownload-mcp ' + bundles[1][1]['mcp']['version'] + ' read-api=1'
            mcporter_check()
            assert config.read_bytes() == config_bytes
            assert len(seen) == 11 and not unexpected
            assert sorted(p.name for p in stable.parent.iterdir()) == ['kownload-mcp']
            print(json.dumps(dict(real_installer=True, signed_fixture_bundles=2,
                                  mcporter=run([mcporter, '--version']).strip(), tools=5, reads=11,
                                  first_install=True, upgrade_new_inode=True,
                                  old_stdio_survived=True, failed_candidate_preserved_old=True,
                                  same_command_args_config_bytes=True, xattr_verified=True,
                                  app_integration=False, publisher_authentication=False), indent=2))
        finally:
            if old is not None:
                old.stdin.close()
                try:
                    old.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    old.terminate()
                    old.wait(timeout=10)
                old.stdout.close()
                old.stderr.close()
            server.shutdown()
            server.server_close()
            thread.join(timeout=3)


if __name__ == '__main__':
    main()
