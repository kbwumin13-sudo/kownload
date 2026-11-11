#!/usr/bin/env python3
"""Real mcporter CLI, real bridge binaries, controlled UDS; no production access.
Usage: python3 tests/mcporter_smoke.py /tmp/bridge-v1 /tmp/bridge-v2
"""
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import socketserver
import subprocess
import sys
import tempfile
import threading

PREFIX = '/api/v1/mcp/read/v1/tasks'
PROGRESS = dict(downloadedBytes=42, speedBytesPerSecond=0, uploadedBytes=0,
                uploadSpeedBytesPerSecond=0, totalBytes=None, phase='downloading')
TASK = dict(id='task_1', protocol='http', status='pause',
            createdAt='2026-11-11T00:00:00Z', updatedAt='2026-11-11T00:01:00Z', progress=PROGRESS)
TOOLS = ['kownload_list_tasks', 'kownload_get_task', 'kownload_get_progress',
         'kownload_get_stats', 'kownload_get_errors']


def main():
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    binaries = [Path(arg).resolve(strict=True) for arg in sys.argv[1:]]
    cli = shutil.which('mcporter')
    assert cli, 'existing mcporter CLI required'
    seen = []
    unexpected = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            seen.append(self.path)
            if self.headers.get('X-Kownload-Read-Version') != '1':
                unexpected.append('read version')
            if self.path == PREFIX + '?limit=1':
                data = dict(tasks=[TASK], nextCursor='')
            elif self.path == PREFIX + '/task_1':
                data = TASK
            elif self.path == PREFIX + '/task_1/progress':
                data = PROGRESS
            elif self.path == PREFIX + '/task_1/stats':
                data = dict(available=False)
            elif self.path == PREFIX + '/task_1/errors':
                data = dict(available=False)
            else:
                unexpected.append(self.path)
                self.send_error(404)
                return
            body = json.dumps(dict(version=1, data=data)).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            unexpected.append('POST')
            self.send_error(405)

    class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True

    with tempfile.TemporaryDirectory(prefix='kmcp-cli-', dir='/tmp') as temp:
        root = Path(temp).resolve()
        os.chmod(root, 0o700)
        socket = root / 'api.sock'
        server = Server(str(socket), Handler)
        os.chmod(socket, 0o600)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            stable = root / 'new data root' / 'bin' / 'kownload-mcp'
            stable.parent.mkdir(parents=True, mode=0o700)
            helpers = root / 'Candidate.app' / 'Contents' / 'Helpers'
            helpers.mkdir(parents=True)
            config = root / 'mcporter.json'
            config.write_text(json.dumps(dict(imports=[], mcpServers=dict(kownload=dict(
                command=str(stable), args=['--socket', str(socket)])))))
            original_config = config.read_bytes()
            cli_env = os.environ.copy()
            # Isolate client home too, so global credentials/config cannot participate.
            cli_home = root / 'client-home'
            cli_home.mkdir(mode=0o700)
            cli_env.update(HOME=str(cli_home), XDG_CONFIG_HOME=str(cli_home / '.config'))

            def invoke(*args):
                result = subprocess.run([cli, '--config', str(config), *args],
                                        cwd=root, env=cli_env, capture_output=True,
                                        text=True, timeout=20)
                assert result.returncode == 0, (args, result.returncode, result.stdout, result.stderr)
                return json.loads(result.stdout)

            versions = []
            for binary in binaries:
                helper = helpers / 'kownload-mcp'
                shutil.copyfile(binary, helper)
                os.chmod(helper, 0o700)
                subprocess.run(['codesign', '--verify', '--strict', str(helper)], check=True,
                               capture_output=True)
                stage = stable.with_suffix('.staging')
                shutil.copyfile(helper, stage)
                os.chmod(stage, 0o700)
                assert hashlib.sha256(stage.read_bytes()).digest() == hashlib.sha256(helper.read_bytes()).digest()
                subprocess.run(['codesign', '--verify', '--strict', str(stage)], check=True,
                               capture_output=True)
                with stage.open('rb') as stream:
                    os.fsync(stream.fileno())
                os.replace(stage, stable)
                version = subprocess.check_output([str(stable), '--version'], text=True).strip()
                versions.append(version)
                listing = invoke('list', 'kownload', '--schema', '--json')
                # CLI versions may wrap server/tool metadata differently.
                listed = json.dumps(listing)
                for name in TOOLS:
                    assert name in listed, (name, listing)
                assert 'kownload_create_task' not in listed
                for index, name in enumerate(TOOLS):
                    args = dict(limit=1) if index == 0 else dict(id='task_1')
                    result = invoke('call', 'kownload.' + name, '--args', json.dumps(args), '--output', 'json')
                    # mcporter unwraps structuredContent in JSON output.
                    if index == 0:
                        assert result['tasks'][0]['id'] == 'task_1', result
                    elif index == 1:
                        assert result['status'] == 'pause', result
                    elif index == 2:
                        assert result['totalBytes'] is None and result['downloadedBytes'] == 42, result
                    else:
                        assert result == dict(available=False), result
                assert config.read_bytes() == original_config, 'client configuration drifted'
            assert versions[0] != versions[1], 'two versioned binaries required'
            count = len(seen)
            denied = subprocess.run([cli, '--config', str(config), 'call',
                                     'kownload.kownload_create_task', '--args', '{}', '--output', 'json'],
                                    cwd=root, env=cli_env, text=True, capture_output=True, timeout=20)
            assert denied.returncode != 0 or 'error' in (denied.stdout + denied.stderr).lower(), denied
            assert len(seen) == count == 10 and not unexpected, (seen, unexpected)
            print(json.dumps(dict(client=subprocess.check_output([cli, '--version'], text=True).strip(),
                                  tools=5, read_calls=10, versions=versions, write_rejected=True,
                                  same_config_bytes=True, signature='Go ad-hoc; isolated simulation only',
                                  app_integration=False), ensure_ascii=False, indent=2))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


if __name__ == '__main__':
    main()
