#!/usr/bin/env python3
"""Run a trial with an instrumented core or an isolated macOS App bundle."""
import argparse
from contextlib import suppress
import ctypes
import hashlib
import http.client
import json
import os
import pathlib
import plistlib
import re
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
        self.running = False
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
        self.running = True

    def stop(self):
        if self.proc is not None:
            with suppress(ProcessLookupError):
                self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
            self.proc = None
        elif self.running:
            self.lib.Stop()
            self.running = False

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
                raise RuntimeError(f'API {method} {path.split("/")[0]} failed with code {result["code"]}')
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


REQUEST_ERRORS = (OSError, http.client.HTTPException, ValueError, RuntimeError, KeyError)
DRAIN_SECONDS = 10


class FixtureClient:
    def __init__(self, base):
        self.base = base
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def get(self, path, timeout):
        with self.opener.open(self.base + path, timeout=timeout) as response:
            return response.read()

    def drain(self, timeout, detailed=False):
        """Return the final zero-inflight snapshot or the last diagnostic sample."""
        deadline = time.monotonic() + timeout
        last = None
        endpoint = '/stats?summary=1'
        while (remaining := deadline - time.monotonic()) > 0:
            try:
                last = json.loads(self.get(endpoint, min(2, remaining)))
            except REQUEST_ERRORS as exc:
                return last, f'fixture_stats_{type(exc).__name__}'
            if not isinstance(last, dict) or type(last.get('inflight')) is not int:
                return None, 'fixture_missing_inflight'
            if last['inflight'] == 0:
                if not detailed or endpoint == '/stats':
                    return last, None
                # Read the detailed evidence only once the cheap counter sees
                # zero, and confirm that snapshot is still drained.
                endpoint = '/stats'
                continue
            endpoint = '/stats?summary=1'
            time.sleep(min(0.05, max(0, deadline - time.monotonic())))
        return last, 'fixture_drain_timeout'


def safe_last_error(value):
    # HTTP diagnostics have a small, documented vocabulary. Core logs and
    # arbitrary API messages can echo URLs or authentication material.
    if not isinstance(value, str):
        return None
    if value in {
        'network error', 'timeout', 'cannot verify resource identity; partial file preserved',
        'local file write failed', 'invalid range response',
    }:
        return value
    status = re.match(r'^HTTP ([1-5][0-9]{2})(?: |$)', value)
    return f'HTTP {status[1]}' if status else None


def failure_reason(status, correct, terminal_recorded, row, last_error, trial_error):
    if status in ('error', 'failed'):
        tagged = row.get('reason')
        if isinstance(tagged, str) and re.fullmatch(r'[a-z][a-z0-9_]{0,47}', tagged):
            return tagged
        return last_error or 'core_terminal_error'
    if status == 'done':
        if not correct:
            return 'missing_or_invalid_file'
        if not terminal_recorded:
            return 'terminal_trace_missing'
        return None
    if status in ('running', 'wait', 'waiting', 'ready'):
        return trial_error or 'task_nonterminal'
    return trial_error or status


def run(core, trace, spec, work):
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
        except REQUEST_ERRORS as exc:
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
            error = 'terminal_deadline'
    except REQUEST_ERRORS as exc:
        error = f'submission_or_trace_{type(exc).__name__}'

    usage1 = resource.getrusage(resource.RUSAGE_SELF)
    observed = time.monotonic() - begin
    try:
        states = {t['id']: t['status'] for t in api('tasks')}
    except REQUEST_ERRORS as exc:
        states = {}
        error = error or f'task_snapshot_{type(exc).__name__}'
    results = []
    for index, task in enumerate(tasks):
        task_id = ids[index] if index < len(ids) else None
        status = states.get(task_id, 'notSubmitted' if task_id is None else 'statusUnavailable')
        row = rows.get(task['name'], {})
        stats = task_stats(task_id) if task_id is not None else {}
        try:
            correct = verify(downloads / task['name'], task)
        except OSError:
            correct = False
        terminal_recorded = row.get('status') == status
        last_error = safe_last_error(stats.get('lastError'))
        reason = failure_reason(status, correct, terminal_recorded, row, last_error, error)
        results.append(dict(timeline(row, submitted_ns), name=task['name'], source=task['source'],
                            status=status, correct=correct, terminalRecorded=terminal_recorded,
                            success=reason is None, failureReason=reason, lastError=last_error,
                            protocol=stats.get('protocol'), rangeState=stats.get('rangeState'),
                            newConnections=stats.get('newConnections'), reusedConnections=stats.get('reusedConnections'),
                            requests=stats.get('requests'), rateLimits=stats.get('rateLimits'),
                            retries=row.get('retries'), deferrals=row.get('deferrals'),
                            timings=stats.get('timings'), adaptive=stats.get('adaptive')))

    terminal = [task['elapsed'] for task in results if task['elapsed'] is not None]
    ok = not error and all(t['success'] for t in results)
    result = {'version': version, 'ok': ok, 'elapsed': max(terminal) if len(terminal) == len(tasks) else None,
              'observedSeconds': observed, 'timeoutSeconds': spec['timeout'],
              'tasks': results, 'error': error, 'adaptiveSamples': adaptive_samples,
              'statsErrors': stats_errors,
              'taskCount': len(tasks), 'correctCount': sum(t['correct'] for t in results)}
    if not core.app:
        result['cpuSeconds'] = (usage1.ru_utime - usage0.ru_utime) + (usage1.ru_stime - usage0.ru_stime)
        result['maxRSS'] = usage1.ru_maxrss
    return result, ids


def verify_restored(core, ids, result):
    # Download task evidence was captured before stopping the measured App.
    try:
        core.start()
        restored = {t['id']: t['status'] for t in core.api('tasks')}
        result['restoredCount'] = sum(restored.get(task_id) == 'done' for task_id in ids)
        if result['restoredCount'] != len(ids):
            result.update(ok=False, error='app_restore_mismatch')
    except REQUEST_ERRORS as exc:
        result.update(ok=False, error=f'app_restore_{type(exc).__name__}')
    finally:
        core.stop()


def failed_trial(spec, error, server=None):
    return {'ok': False, 'error': error, 'timeoutSeconds': spec['timeout'],
            'statsIncomplete': True, 'server': server,
            'taskCount': len(spec['tasks']),
            'tasks': [{'name': t['name'], 'source': t['source'], 'success': False,
                       'correct': False, 'terminalRecorded': False, 'status': 'unobserved',
                       'failureReason': error} for t in spec['tasks']]}


def read_budget_timeline(trace):
    path = pathlib.Path(str(trace) + '.budget')
    if not path.exists():
        return []
    events = []
    for line in path.read_bytes().splitlines(keepends=True):
        if not line.endswith(b'\n'):
            continue
        row = json.loads(line)
        if row.get('kind') not in {'cooldown', 'probe', 'reopened', 'probeTimeout'}:
            continue
        events.append({k: row[k] for k in ('kind', 'port', 'atNs', 'untilNs') if k in row})
    return events


def execute(core, trace, spec, work, fixture):
    server, reason = fixture.drain(DRAIN_SECONDS)
    if reason:
        return failed_trial(spec, 'pretrial_' + reason, server)
    try:
        fixture.get('/reset', 2)
    except REQUEST_ERRORS as exc:
        return failed_trial(spec, f'fixture_reset_{type(exc).__name__}', server)

    try:
        core.start()
        result, ids = run(core, trace, spec, work)
    except REQUEST_ERRORS as exc:
        result, ids = failed_trial(spec, f'core_trial_{type(exc).__name__}'), []
    finally:
        core.stop()

    # Stopping joins the Core and closes pooled transfers. Cancelled handlers
    # finish flushing their counters asynchronously, so take only a drained
    # snapshot as final evidence.
    server, reason = fixture.drain(DRAIN_SECONDS, detailed=True)
    result['server'] = server
    with suppress(OSError, ValueError):
        result['budgetTimeline'] = read_budget_timeline(trace)
    result['statsIncomplete'] = result.get('statsIncomplete', False) or bool(reason)
    if reason:
        result['statsIncompleteReason'] = reason
    if core.app and result['ok'] and not result['statsIncomplete']:
        verify_restored(core, ids, result)
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
        result = execute(core, trace, spec, work, FixtureClient(spec['fixture']))
        print(json.dumps(result))


if __name__ == '__main__':
    main()
