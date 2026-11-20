#!/usr/bin/env python3
"""Run a trial with an instrumented core or an isolated macOS App bundle."""
import argparse
import ctypes
import hashlib
import http.client
import json
import os
import pathlib
import plistlib
import resource
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request


class StartResult(ctypes.Structure):
    _fields_ = [('port', ctypes.c_longlong), ('error', ctypes.c_void_p)]


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=10)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class TrialCore:
    def __init__(self, lib_path, app, work, trace):
        self.app = app
        self.work = work
        self.trace = trace
        self.proc = None
        self.lib = None
        self.token = ''
        if app:
            info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
            if not info['CFBundleIdentifier'].endswith('.speedbench'):
                raise ValueError('App bundle must have the isolated .speedbench identifier')
            self.executable = app / 'Contents/MacOS' / info['CFBundleExecutable']
        else:
            self.lib = ctypes.CDLL(lib_path)
            self.lib.Start.argtypes = [ctypes.c_char_p]
            self.lib.Start.restype = StartResult
            self.lib.Stop.restype = None

    def start(self):
        state = self.work / 'state'
        if self.app:
            home = self.work / 'home'
            home.mkdir(exist_ok=True)
            self.proc = subprocess.Popen(
                [str(self.executable), '--hidden'],
                env=dict(os.environ, HOME=str(home), CFFIXED_USER_HOME=str(home),
                         GOPEED_PREVIEW_STORAGE=str(state),
                         KOWNLOAD_SPEEDBENCH_TRACE=str(self.trace)),
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            self.address = state / 'ipc/api.sock'
            deadline = time.monotonic() + 25
            while time.monotonic() < deadline and self.proc.poll() is None:
                try:
                    self.api('info')
                    return
                except (OSError, http.client.HTTPException):
                    time.sleep(0.05)
            raise RuntimeError(f'isolated App did not start (exit={self.proc.poll()})')
        self.token = secrets.token_hex(16)
        config = {'network': 'tcp', 'address': '127.0.0.1:0', 'apiToken': self.token,
                  'storage': 'mem', 'refreshInterval': 100, 'storageDir': str(state)}
        started = self.lib.Start(json.dumps(config).encode())
        if started.error:
            raise RuntimeError('core failed to start')
        self.address = f'127.0.0.1:{started.port}'

    def stop(self):
        if self.proc is not None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
            self.proc = None
        elif self.lib is not None:
            self.lib.Stop()

    def api(self, path, data=None, method='GET'):
        connection = UnixConnection(self.address) if self.app else http.client.HTTPConnection(
            self.address, timeout=60)
        try:
            connection.request(method, f'/api/v1/{path}',
                               body=None if data is None else json.dumps(data),
                               headers={'X-Api-Token': self.token, 'Content-Type': 'application/json'})
            response = connection.getresponse()
            if response.status >= 400:
                raise RuntimeError(f'API {method} {path.split("/")[0]} returned HTTP {response.status}')
            result = json.load(response)
            if result['code'] != 0:
                raise RuntimeError(f'API {method} {path.split("/")[0]} failed: {result.get("msg")}')
            return result['data']
        finally:
            connection.close()


def read_trace(path):
    if not path.exists():
        return {}
    # A terminal event may be appended as we read. Only consume complete lines.
    raw = path.read_bytes()
    rows = [json.loads(line) for line in raw.splitlines(keepends=True) if line.endswith(b'\n')]
    return {row['name']: row for row in rows}


def timeline(row, submitted_ns):
    def seconds(start, end):
        return (end - start) / 1e9 if start is not None and end is not None else None

    first, p90, last, terminal = (row.get(k) for k in ('firstNs', 'p90Ns', 'lastNs', 'terminalNs'))
    return {'elapsed': seconds(submitted_ns, terminal),
            'startupSeconds': seconds(submitted_ns, first),
            'throughputPhaseSeconds': seconds(first, p90),
            'tailSeconds': seconds(p90, last),
            'deliverySeconds': seconds(last, terminal)}


def verify(path, expected):
    if not path.is_file():
        return False
    with path.open('rb') as handle:
        return (path.stat().st_size == expected['size']
                and hashlib.file_digest(handle, 'sha256').hexdigest() == expected['sha'])


def run(core, trace, spec, work):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def fixture(path):
        with opener.open(spec['fixture'] + path, timeout=10) as response:
            return response.read()

    api = core.api
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
    tasks = spec['tasks']
    requests = [{'req': {'url': t['url'], 'skipVerifyCert': True, 'proxy': {'mode': 'none'}},
                 'opts': {'path': str(downloads), 'name': t['name'],
                          'extra': {'connections': spec['connections']}}} for t in tasks]

    submitted_ns = time.time_ns()
    begin = time.monotonic()
    usage0 = resource.getrusage(resource.RUSAGE_SELF)
    deadline = begin + spec['timeout']
    error = ''
    ids = []
    rows = {}
    adaptive_samples = []
    stats_errors = []
    next_sample = begin + 1

    def task_stats(task_id):
        try:
            return api(f'tasks/{task_id}/stats') or {}
        except (OSError, http.client.HTTPException, ValueError, RuntimeError) as exc:
            stats_errors.append(type(exc).__name__)
            return {}
    try:
        if spec.get('submission') == 'resolve-create':
            resolved = api('resolve', requests[0], 'POST')
            ids = [api('tasks', {'rid': resolved['id']}, 'POST')]
        else:
            ids = api('tasks/batch', {'reqs': requests, 'opts': {'path': str(downloads)}}, 'POST')
        if len(ids) != len(tasks):
            raise RuntimeError(f'submitted {len(ids)} of {len(tasks)} tasks')
        while time.monotonic() < deadline:
            rows = read_trace(trace)
            if len(rows) == len(tasks):
                break
            now = time.monotonic()
            if (spec.get('tuning') or {}).get('adaptive') and now >= next_sample:
                next_sample = now + 1
                for task_id, task in zip(ids, tasks):
                    if task['name'] in rows:
                        continue
                    stats = task_stats(task_id)
                    if stats and stats.get('adaptive'):
                        adaptive_samples.append({'name': task['name'], 'seconds': now - begin,
                                                 'streams': stats.get('activeStreams'),
                                                 **stats['adaptive']})
            time.sleep(0.02 if len(ids) <= 20 else 0.10)
        if len(rows) != len(tasks):
            rows = read_trace(trace)
            error = 'deadline expired before all terminal events were recorded'
    except (OSError, http.client.HTTPException, RuntimeError, KeyError) as exc:
        error = str(exc)

    usage1 = resource.getrusage(resource.RUSAGE_SELF)
    observed = time.monotonic() - begin
    states = {t['id']: t['status'] for t in api('tasks')}
    server = json.loads(fixture('/stats'))
    results = []
    for index, task in enumerate(tasks):
        task_id = ids[index] if index < len(ids) else None
        status = states.get(task_id, 'notSubmitted')
        row = rows.get(task['name'], {})
        if task_id is not None:
            stats = task_stats(task_id) or {}
        else:
            stats = {}
        correct = verify(downloads / task['name'], task)
        results.append(dict(timeline(row, submitted_ns), name=task['name'], status=status,
                            correct=correct, terminalRecorded=row.get('status') == status,
                            protocol=stats.get('protocol'), rangeState=stats.get('rangeState'),
                            newConnections=stats.get('newConnections'), reusedConnections=stats.get('reusedConnections'),
                            rateLimits=stats.get('rateLimits'), timings=stats.get('timings'),
                            adaptive=stats.get('adaptive'), lastError=stats.get('lastError')))

    terminal = [task['elapsed'] for task in results if task['elapsed'] is not None]
    ok = not error and all(t['status'] == 'done' and t['correct'] and t['terminalRecorded'] for t in results)
    result = {'version': version, 'ok': ok, 'elapsed': max(terminal) if len(terminal) == len(tasks) else None,
              'observedSeconds': observed, 'timeoutSeconds': spec['timeout'],
              'server': server, 'tasks': results, 'error': error, 'adaptiveSamples': adaptive_samples,
              'statsErrors': stats_errors,
              'taskCount': len(tasks), 'correctCount': sum(t['correct'] for t in results)}
    if not ok and spec.get('debug'):
        log = work / 'state/logs/core.log'
        if log.is_file():
            result['coreDiagnostics'] = [line for line in log.read_text().splitlines()
                                         if 'task download failed' in line][-len(tasks):]
    if not core.app:
        result['cpuSeconds'] = (usage1.ru_utime - usage0.ru_utime) + (usage1.ru_stime - usage0.ru_stime)
        result['maxRSS'] = usage1.ru_maxrss
    elif ok:
        try:
            core.stop()
            core.start()
            restored = {t['id']: t['status'] for t in api('tasks')}
            result['restoredCount'] = sum(restored.get(task_id) == 'done' for task_id in ids)
            result['ok'] = result['restoredCount'] == len(ids)
        except (OSError, http.client.HTTPException, RuntimeError) as exc:
            result.update(ok=False, error=f'App restart verification failed: {exc}')
    return result


def main():
    parser = argparse.ArgumentParser()
    surface = parser.add_mutually_exclusive_group(required=True)
    surface.add_argument('--lib')
    surface.add_argument('--app', type=pathlib.Path)
    parser.add_argument('--spec', required=True, type=pathlib.Path)
    args = parser.parse_args()
    spec = json.loads(args.spec.read_text())
    # The parent prepares a private, short path for the App's Unix socket.
    with tempfile.TemporaryDirectory(prefix='trial-', dir=spec.get('workDir')) as work:
        work = pathlib.Path(work)
        trace = pathlib.Path(os.environ['KOWNLOAD_SPEEDBENCH_TRACE'])
        core = TrialCore(args.lib, args.app, work, trace)
        try:
            core.start()
            result = run(core, trace, spec, work)
        finally:
            core.stop()
        print(json.dumps(result))


if __name__ == '__main__':
    main()
