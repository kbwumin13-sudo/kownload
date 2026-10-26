#!/usr/bin/env python3
"""Download a loopback-seeded torrent with one core library, pausing midway.

btseed serves a deterministic file and prints a magnet whose x.pe points at
it, so the check needs no public tracker or swarm. Prints one JSON result.
"""
import argparse
import ctypes
import hashlib
import json
import pathlib
import secrets
import subprocess
import tempfile
import time
import urllib.request


class StartResult(ctypes.Structure):
    _fields_ = [('port', ctypes.c_longlong), ('error', ctypes.c_void_p)]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--lib', required=True)
    parser.add_argument('--seeder', required=True)
    parser.add_argument('--size', type=int, default=48 << 20)
    parser.add_argument('--rate', type=int, default=8 << 20, help='seeder upload limit, bytes/s')
    args = parser.parse_args()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryDirectory(prefix='kownload-bt-') as work:
        work = pathlib.Path(work)
        seed_dir = work / 'seed'
        seed_dir.mkdir()
        ready = work / 'ready.json'
        seeder = subprocess.Popen([args.seeder, '-dir', str(seed_dir), '-size', str(args.size),
                                   '-rate', str(args.rate), '-ready-file', str(ready)],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(600):
                if ready.exists():
                    break
                time.sleep(0.05)
            seed = json.loads(ready.read_text())
            lib = ctypes.CDLL(args.lib)
            lib.Start.argtypes = [ctypes.c_char_p]
            lib.Start.restype = StartResult
            lib.Stop.restype = None
            token = secrets.token_hex(16)
            started = lib.Start(json.dumps({'network': 'tcp', 'address': '127.0.0.1:0', 'apiToken': token,
                                            'storage': 'mem', 'refreshInterval': 100,
                                            'storageDir': str(work / 'state')}).encode())
            if started.error:
                raise SystemExit('core failed to start')

            def api(path, data=None, method='GET'):
                request = urllib.request.Request(f'http://127.0.0.1:{started.port}/api/v1/{path}',
                                                 data=None if data is None else json.dumps(data).encode(),
                                                 method=method,
                                                 headers={'X-Api-Token': token, 'Content-Type': 'application/json'})
                with opener.open(request, timeout=60) as response:
                    result = json.load(response)
                if result['code'] != 0:
                    raise RuntimeError(f'API {method} {path.split("/")[0]} failed: {result.get("msg")}')
                return result['data']

            try:
                cfg = api('config')
                cfg['proxy'] = {'enable': False}
                api('config', cfg, 'PUT')
                downloads = work / 'downloads'
                downloads.mkdir()
                begin = time.monotonic()
                task_id = api('tasks', {'req': {'url': seed['magnet']}, 'opts': {'path': str(downloads)}}, 'POST')
                paused_at = resumed_bytes = None
                pause_held = None
                status = None
                while time.monotonic() - begin < 300:
                    task = api('tasks/' + task_id)
                    status = task['status']
                    done = task['progress']['downloaded']
                    if paused_at is None and done >= args.size // 3:
                        api(f'tasks/{task_id}/pause', method='PUT')
                        time.sleep(0.5)
                        paused_at = api('tasks/' + task_id)['progress']['downloaded']
                        time.sleep(2)
                        after = api('tasks/' + task_id)
                        pause_held = after['status'] == 'pause' and after['progress']['downloaded'] == paused_at
                        api(f'tasks/{task_id}/continue', method='PUT')
                        resumed_bytes = paused_at
                    if status in ('done', 'error'):
                        break
                    time.sleep(0.2)
                elapsed = time.monotonic() - begin
                files = [p for p in downloads.rglob('*') if p.is_file()]
                digest = hashlib.sha256(files[0].read_bytes()).hexdigest() if len(files) == 1 else None
                print(json.dumps({
                    'version': api('info')['version'], 'status': status, 'elapsed': round(elapsed, 3),
                    'bytes': args.size, 'pausedAtBytes': resumed_bytes, 'pauseHeld': pause_held,
                    'sha256Matches': digest == seed['sha256'], 'files': len(files),
                    'ok': status == 'done' and digest == seed['sha256'] and bool(pause_held),
                }))
                api('tasks?force=true', method='DELETE')
            finally:
                lib.Stop()
        finally:
            seeder.terminate()
            seeder.wait(10)


if __name__ == '__main__':
    main()
