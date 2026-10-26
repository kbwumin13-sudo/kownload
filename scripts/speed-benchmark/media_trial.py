#!/usr/bin/env python3
"""Download the local HLS and DASH samples with one core library and check the
results with ffprobe. The media fixture runs from scripts/media-fixture-server.py
with an optional per-chunk delay; the core uses in-memory storage. Prints JSON.
"""
import argparse
import ctypes
import json
import pathlib
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]


class StartResult(ctypes.Structure):
    _fields_ = [('port', ctypes.c_longlong), ('error', ctypes.c_void_p)]


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def probe(path):
    out = subprocess.check_output(['/opt/homebrew/bin/ffprobe', '-v', 'error', '-show_entries',
                                   'stream=codec_type:format=duration,format_name', '-of', 'json', str(path)])
    data = json.loads(out)
    return {'streams': sorted(s['codec_type'] for s in data.get('streams', [])),
            'duration': round(float(data.get('format', {}).get('duration', 0)), 2),
            'format': data.get('format', {}).get('format_name')}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--lib', required=True)
    parser.add_argument('--connections', type=int, default=4)
    parser.add_argument('--delay', type=float, default=0)
    parser.add_argument('--yt-dlp', required=True)
    parser.add_argument('--ffmpeg', default='/opt/homebrew/bin/ffmpeg')
    parser.add_argument('--node', default='/opt/homebrew/bin/node')
    args = parser.parse_args()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryDirectory(prefix='kownload-media-') as work:
        work = pathlib.Path(work)
        port = free_port()
        fixture = subprocess.Popen([sys.executable, '-I', str(ROOT / 'scripts/media-fixture-server.py'),
                                    '--port', str(port), '--root', str(work / 'fixtures'), '--delay', str(args.delay)],
                                   stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        try:
            fixture.stdout.readline()  # ready once samples are generated
            base = f'http://127.0.0.1:{port}'
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
                with opener.open(request, timeout=90) as response:
                    result = json.load(response)
                if result['code'] != 0:
                    raise RuntimeError(f'API {method} {path.split("/")[0]} failed: {result.get("msg")}')
                return result['data']

            try:
                cfg = api('config')
                cfg['proxy'] = {'enable': False}
                media = cfg['protocolConfig'].setdefault('media', {})
                media.update(ytDlpPath=args.yt_dlp, ffmpegPath=args.ffmpeg, jsRuntimePath=args.node,
                             connections=args.connections)
                api('config', cfg, 'PUT')
                results = []
                for title, source in [('HLS', '/hls/master.m3u8'), ('DASH', '/dash/stream.mpd')]:
                    out_dir = work / ('out-' + title)
                    out_dir.mkdir()
                    req = {'url': base + source, 'protocol': 'media',
                           'extra': {'title': title, 'sourcePage': base + '/index.html', 'mediaKind': 'video'}}
                    inspection = api('media/inspect', {'req': req}, 'POST')
                    begin = time.monotonic()
                    task_id = api('tasks', {'req': req, 'opts': {'path': str(out_dir)}}, 'POST')
                    status = None
                    while time.monotonic() - begin < 180:
                        task = api('tasks/' + task_id)
                        status = task['status']
                        if status in ('done', 'error'):
                            break
                        time.sleep(0.2)
                    elapsed = time.monotonic() - begin
                    files = [p for p in out_dir.rglob('*') if p.is_file()]
                    entry = {'title': title, 'status': status, 'elapsed': round(elapsed, 3),
                             'formats': len(inspection.get('formats') or []), 'files': len(files)}
                    if status == 'done' and len(files) == 1:
                        entry.update(probe(files[0]))
                    entry['ok'] = status == 'done' and entry.get('streams') == ['audio', 'video'] and entry.get('duration', 0) > 7
                    results.append(entry)
                print(json.dumps({'version': api('info')['version'], 'connections': args.connections,
                                  'delay': args.delay, 'results': results,
                                  'ok': all(r['ok'] for r in results)}, ensure_ascii=False))
                api('tasks?force=true', method='DELETE')
            finally:
                lib.Stop()
        finally:
            fixture.terminate()
            fixture.wait(10)


if __name__ == '__main__':
    main()
