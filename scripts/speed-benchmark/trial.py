#!/usr/bin/env python3
"""Run one controlled download trial against one kownload core library.

The core runs in this process with in-memory storage and its own temporary
download directory, so the installed app, its tasks and its settings are never
touched. Prints one JSON result. Output holds no URLs beyond the local fixture.
"""
import argparse
import ctypes
import hashlib
import json
import os
import pathlib
import resource
import secrets
import sys
import tempfile
import time
import urllib.request


class StartResult(ctypes.Structure):
    _fields_ = [('port', ctypes.c_longlong), ('error', ctypes.c_void_p)]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--lib', required=True)
    parser.add_argument('--spec', required=True, help='JSON file describing the trial')
    args = parser.parse_args()
    spec = json.loads(pathlib.Path(args.spec).read_text())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def fixture(path):
        with opener.open(spec['fixture'] + path, timeout=60) as response:
            return response.read()

    lib = ctypes.CDLL(args.lib)
    lib.Start.argtypes = [ctypes.c_char_p]
    lib.Start.restype = StartResult
    lib.Stop.restype = None
    token = secrets.token_hex(16)
    with tempfile.TemporaryDirectory(prefix='kownload-trial-', dir=spec.get('workDir')) as work:
        work = pathlib.Path(work)
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
            version = api('info')['version']
            cfg = api('config')
            cfg['proxy'] = {'enable': False}
            cfg['maxRunning'] = spec.get('maxRunning', 5)
            http_cfg = cfg['protocolConfig'].setdefault('http', {})
            http_cfg['connections'] = spec['connections']
            if spec.get('tuning') is not None:
                http_cfg['tuning'] = spec['tuning']
            else:
                http_cfg.pop('tuning', None)
            api('config', cfg, 'PUT')
            fixture('/reset')
            downloads = work / 'downloads'
            downloads.mkdir()
            usage0 = resource.getrusage(resource.RUSAGE_SELF)
            begin = time.monotonic()
            reqs = [{'req': {'url': t['url'], 'skipVerifyCert': True, 'proxy': {'mode': 'none'}},
                     'opts': {'path': str(downloads), 'name': t['name'],
                              'extra': {'connections': spec['connections']}}} for t in spec['tasks']]
            ids = api('tasks/batch', {'reqs': reqs, 'opts': {'path': str(downloads)}}, 'POST')
            pending = set(ids)
            finished = {}
            peak_streams = {}
            # Integral of running streams over time: the cost a stream policy pays.
            stream_seconds = {}
            last_sample = {}
            deadline = begin + spec.get('timeout', 600)
            poll = 0
            interval = 0.1 if len(ids) <= 20 else 0.25
            while pending and time.monotonic() < deadline:
                unfinished = {t['id'] for t in api('tasks?notStatus=done&notStatus=error')}
                now = time.monotonic() - begin
                for task_id in list(pending):
                    if task_id not in unfinished:
                        finished[task_id] = {'elapsed': now}
                        pending.discard(task_id)
                # Sample stream counts of a few running tasks without slowing small batches.
                if poll % 5 == 0:
                    for task_id in list(pending)[:4]:
                        stats = api(f'tasks/{task_id}/stats')
                        active = stats.get('activeStreams')
                        if active is None:
                            active = sum(1 for c in stats.get('connections') or [] if not c.get('completed'))
                        peak_streams[task_id] = max(peak_streams.get(task_id, 0), active)
                        sampled = time.monotonic()
                        if task_id in last_sample:
                            stream_seconds[task_id] = stream_seconds.get(task_id, 0) + active * (sampled - last_sample[task_id])
                        last_sample[task_id] = sampled
                poll += 1
                time.sleep(interval)
            elapsed = time.monotonic() - begin
            for task in api('tasks'):
                if task['id'] in finished:
                    finished[task['id']]['status'] = task['status']
            usage1 = resource.getrusage(resource.RUSAGE_SELF)
            results = []
            ok = not pending
            for index, task_id in enumerate(ids):
                expected = spec['tasks'][index]
                path = downloads / expected['name']
                digest = hashlib.sha256()
                written = 0
                if path.exists():
                    with path.open('rb') as handle:
                        for block in iter(lambda: handle.read(1 << 20), b''):
                            digest.update(block)
                            written += len(block)
                stats = api(f'tasks/{task_id}/stats')
                correct = digest.hexdigest() == expected['sha'] and written == expected['size']
                ok = ok and correct and finished.get(task_id, {}).get('status') == 'done'
                results.append({
                    'status': finished.get(task_id, {}).get('status', 'timeout'),
                    'elapsed': round(finished.get(task_id, {}).get('elapsed', elapsed), 4),
                    'correct': correct,
                    'streams': len(stats.get('connections') or []),
                    'peakActiveStreams': peak_streams.get(task_id),
                    'streamSeconds': round(stream_seconds.get(task_id, 0), 1),
                    'protocol': stats.get('protocol'),
                    'rangeState': stats.get('rangeState'),
                    'newConnections': stats.get('newConnections'),
                    'reusedConnections': stats.get('reusedConnections'),
                    'rateLimits': stats.get('rateLimits'),
                    'timings': stats.get('timings'),
                    'adaptive': stats.get('adaptive'),
                })
            server = json.loads(fixture('/stats'))
            total_bytes = sum(t['size'] for t in spec['tasks'])
            cpu = (usage1.ru_utime - usage0.ru_utime) + (usage1.ru_stime - usage0.ru_stime)
            print(json.dumps({
                'version': version, 'ok': ok, 'elapsed': round(elapsed, 4),
                'cpuSeconds': round(cpu, 4), 'cpuSecondsPerGiB': round(cpu / (total_bytes / (1 << 30)), 4),
                'maxRSS': usage1.ru_maxrss, 'bytes': total_bytes,
                'server': server, 'tasks': results if len(results) <= 20 else results[:5],
                'taskCount': len(results), 'correctCount': sum(1 for r in results if r['correct']),
            }))
            api('tasks?force=true', method='DELETE')
        finally:
            lib.Stop()


if __name__ == '__main__':
    main()
