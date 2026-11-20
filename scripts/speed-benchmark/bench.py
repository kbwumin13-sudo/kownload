#!/usr/bin/env python3
"""Alternating A/B comparison of kownload cores on controlled fixtures.

Each trial runs one core library in its own process (trial.py) against a
fixture started for the scenario, verifies every file's SHA-256 and records
completion time, CPU and the server's connection and request counts. Pairs
alternate their order; three pairs are run and up to five when the spread of
any variant exceeds 5 %.

Variants are NAME=LIB or NAME=LIB:TUNING_JSON, optionally suffixed with
@CONNECTIONS, for example:

  bench.py --scenario b1-h1 --variant base=old.dylib --variant cand=new.dylib \\
      --output results/b1-h1.json
"""
import argparse
import json
import os
import pathlib
import signal
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
MIB = 1 << 20


def files(prefix, count, size, listener, query=''):
    return [{'name': f'{prefix}{i}.bin', 'size': size, 'listener': listener, 'query': query}
            for i in range(count)]


# Each scenario fixes the content, listener, server latency and limits.
SCENARIOS = {
    'cooldown-slots': dict(tasks=files('cs-rate-', 1, 6 * MIB, 'plain',
                                     'perconn=4194304&limit=1&retry=2')
                           + files('cs-other-', 2, 2 * MIB, 'tls1', 'perconn=4194304'),
                           connections=4, maxRunning=1, timeout=30),
    'terminal-error': dict(tasks=files('er-', 1, MIB, 'plain', 'reject=404'), connections=4, timeout=8),
    'probe-delayed': dict(tasks=files('pd-', 1, 12 * MIB, 'plain',
                                    'mode=hidden&probedelay=1000&perconn=4194304'), connections=4),
    'probe-ignored': dict(tasks=files('pi-', 1, 4 * MIB, 'plain',
                                    'mode=ignore&probedelay=1000&perconn=2097152'), connections=4),
    'probe-high-rtt': dict(tasks=files('ph-', 1, 12 * MIB, 'plain', 'mode=hidden&perconn=4194304'),
                           connections=4, server=['-rtt', '200ms']),
    'slow-stream': dict(tasks=files('ss-', 1, 48 * MIB, 'plain',
                                   'perconn=4194304&slowonce=400000'), connections=4),
    'disconnect': dict(tasks=files('dc-', 1, 32 * MIB, 'plain',
                                   'perconn=4194304&abortafter=2097152'), connections=4),
    'adaptive-capped': dict(tasks=files('ac-', 1, 96 * MIB, 'plain',
                                       'total=2097152&group=ac'), connections=8, timeout=180),
    'adaptive-batch': dict(tasks=files('am-', 3, 32 * MIB, 'plain',
                                       'total=2097152&group=am'), connections=8, timeout=180, debug=True),
    'adaptive-changing': dict(tasks=files('an-', 1, 96 * MIB, 'plain',
                                          'perconn=524288&total=4194304&group=an&dropafter=8&droprate=1048576'),
                              connections=8, timeout=180),
    'app-dmg': dict(tasks=[{'name': 'valid.dmg', 'size': 0, 'listener': 'plain', 'query': ''}],
                    connections=4, submission='resolve-create'),
    'app-small': dict(tasks=files('as-', 1, 64 * 1024, 'plain'),
                      connections=4, submission='resolve-create'),
    'app-batch': dict(tasks=files('ab-', 5, 2 * MIB, 'plain', 'perconn=4194304'),
                      connections=4, maxRunning=3),
    # Hidden Range, every connection limited: segmentation should pay off.
    'r1': dict(tasks=files('r1-', 1, 32 * MIB, 'plain', 'mode=hidden&perconn=2097152'), connections=4),
    # The same over TLS, with and without HTTP/2 on the server.
    'r1-tls1': dict(tasks=files('rt1-', 1, 32 * MIB, 'tls1', 'mode=hidden&perconn=2097152'), connections=4,
                    server=['-accept-delay', '30ms', '-rtt', '10ms']),
    'r1-tls': dict(tasks=files('rt2-', 1, 32 * MIB, 'tls', 'mode=hidden&perconn=2097152'), connections=4,
                   server=['-accept-delay', '30ms', '-rtt', '10ms']),
    # Same content, total rate capped: more streams cannot help.
    'r3': dict(tasks=files('r3-', 1, 32 * MIB, 'plain', 'mode=hidden&total=4194304&group=r3'), connections=4),
    # 100 small same-origin files over TLS with handshake latency.
    'b1-h1': dict(tasks=files('b1-', 100, 64 * 1024, 'tls1'), connections=4, maxRunning=5,
                  server=['-accept-delay', '30ms', '-rtt', '10ms']),
    'b1-h2': dict(tasks=files('b1-', 100, 64 * 1024, 'tls'), connections=4, maxRunning=5,
                  server=['-accept-delay', '30ms', '-rtt', '10ms']),
    'b1-1000': dict(tasks=files('bk-', 1000, 16 * 1024, 'tls1'), connections=4, maxRunning=5,
                    server=['-accept-delay', '30ms', '-rtt', '10ms'], timeout=1200),
    # Two sources; the first limits requests with a token bucket (3/s, burst 3)
    # and answers 429/503 with Retry-After in seconds, as a date, or without it.
    'b2-bucket': dict(tasks=files('bka-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=bka&rps=3&burst=3')
                      + files('bkb-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=bkb'),
                      connections=4, maxRunning=5),
    'b2-bucket-date': dict(tasks=files('bkd-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=bkd&rps=3&burst=3&status=503&retry=date')
                           + files('bke-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=bke'),
                           connections=4, maxRunning=5),
    'b2-bucket-none': dict(tasks=files('bkn-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=bkn&rps=3&burst=3&retry=none')
                           + files('bko-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=bko'),
                           connections=4, maxRunning=5),
    # Two sources; the first refuses a fixed number of its first range requests
    # with 429 but does not enforce its own Retry-After (an unenforced limit).
    'b2': dict(tasks=files('b2a-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=b2a&limit=4&status=429&retry=2')
               + files('b2b-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=b2b'),
               connections=4, maxRunning=5),
    'b2-date': dict(tasks=files('b2d-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=b2d&limit=4&status=503&retry=date:2')
                    + files('b2e-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=b2e'),
                    connections=4, maxRunning=5),
    'b2-none': dict(tasks=files('b2n-', 5, 8 * MIB, 'plain', 'perconn=4194304&group=b2n&limit=4&status=429&retry=none')
                    + files('b2o-', 5, 8 * MIB, 'tls1', 'perconn=4194304&group=b2o'),
                    connections=4, maxRunning=5),
    # Long transfers for stream-count validation.
    'big-capped': dict(tasks=files('bc-', 1, 160 * MIB, 'plain', 'total=1048576&group=bc'), connections=8, timeout=900),
    'big-perconn': dict(tasks=files('bp-', 1, 160 * MIB, 'plain', 'perconn=131072'), connections=8, timeout=1500),
    'big-both': dict(tasks=files('bb-', 1, 160 * MIB, 'plain', 'perconn=131072&total=786432&group=bb'),
                     connections=8, timeout=1500),
    'slow-tail': dict(tasks=files('st-', 1, 64 * MIB, 'plain', 'perconn=2097152&tailfrom=0.85&tailrate=262144'),
                      connections=4),
    'link-drop': dict(tasks=files('ld-', 1, 96 * MIB, 'plain', 'perconn=1048576&total=4194304&group=ld&dropafter=8&droprate=1048576'),
                      connections=4, timeout=900),
    # Local source without limits, for CPU and buffer comparisons.
    'd1': dict(tasks=files('d1-', 1, 1024 * MIB, 'plain'), connections=4, timeout=900),
}


def parse_variant(text):
    name, _, rest = text.partition('=')
    connections = None
    if '@' in rest:
        rest, _, conn = rest.rpartition('@')
        connections = int(conn)
    lib, tuning = rest, None
    if ':{' in rest:
        lib, _, raw = rest.partition(':{')
        tuning = json.loads('{' + raw)
    return {'name': name, 'lib': str(pathlib.Path(lib).resolve()), 'tuning': tuning, 'connections': connections}


class Fixture:
    def __init__(self, binary, flags):
        self.dir = tempfile.TemporaryDirectory(prefix='kownload-fixture-')
        ready = pathlib.Path(self.dir.name) / 'ready.json'
        self.proc = subprocess.Popen([binary, '-ready-file', str(ready), *flags],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(200):
            if ready.exists():
                break
            time.sleep(0.05)
        self.ports = json.loads(ready.read_text())
        self.base = f'http://127.0.0.1:{self.ports["plain"]}'

    def url(self, task):
        scheme = 'http' if task['listener'] == 'plain' else 'https'
        query = f'size={task["size"]}' + (('&' + task['query']) if task['query'] else '')
        return f'{scheme}://127.0.0.1:{self.ports[task["listener"]]}/f/{task["name"]}?{query}'

    def sha(self, task):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(f'{self.base}/sha?name={task["name"]}&size={task["size"]}', timeout=600) as r:
            return r.read().decode()

    def close(self):
        self.proc.terminate()
        self.proc.wait(10)
        self.dir.cleanup()


def run_trial(variant, scenario, fixture, work_dir):
    spec = {
        'fixture': fixture.base,
        'connections': variant['connections'] or scenario['connections'],
        'maxRunning': scenario.get('maxRunning', 5),
        'submission': scenario.get('submission', 'batch'),
        'debug': scenario.get('debug', False),
        'tuning': variant['tuning'],
        'timeout': scenario.get('timeout', 600),
        'workDir': work_dir,
        'tasks': [{'url': fixture.url(t), 'name': t['name'], 'size': t['size'], 'sha': t['sha']}
                  for t in scenario['tasks']],
    }
    parent = '/private/tmp' if variant['lib'].endswith('.app') else work_dir
    with tempfile.TemporaryDirectory(prefix='kown-bench-', dir=parent) as scratch:
        scratch = pathlib.Path(scratch)
        spec['workDir'] = str(scratch)
        spec_path = scratch / 'spec.json'
        spec_path.write_text(json.dumps(spec))
        trace = str(scratch / 'timeline.jsonl')
        surface = '--app' if variant['lib'].endswith('.app') else '--lib'
        with subprocess.Popen(
            [sys.executable, '-I', str(HERE / 'trial.py'), surface, variant['lib'], '--spec', spec_path],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True,
            env=dict(os.environ, KOWNLOAD_SPEEDBENCH_TRACE=trace),
        ) as process:
            try:
                stdout, stderr = process.communicate(timeout=spec['timeout'] + 75)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
                return {'ok': False, 'error': 'trial process deadline exceeded',
                        'timeoutSeconds': spec['timeout'], 'taskCount': len(spec['tasks'])}
            exit_code = process.returncode
    if exit_code != 0:
        return {'ok': False, 'error': (stderr or stdout)[-2000:],
                'timeoutSeconds': spec['timeout'], 'taskCount': len(spec['tasks'])}
    try:
        return json.loads(stdout.strip().splitlines()[-1])
    except (json.JSONDecodeError, IndexError):
        return {'ok': False, 'error': 'trial produced no valid result',
                'timeoutSeconds': spec['timeout'], 'taskCount': len(spec['tasks'])}


def spread(values):
    median = statistics.median(values)
    return (max(values) - min(values)) / median if median else 0.0


def summarize(name, results, variants):
    summary = {'scenario': name, 'variants': {}}
    for v in variants:
        runs = [r['result'] for r in results if r['variant'] == v['name']]
        good = [r for r in runs if r['ok']]
        times = [r['elapsed'] for r in good]
        entry = {'runs': len(runs), 'ok': len(good), 'failed': len(runs) - len(good),
                 'failureRate': (len(runs) - len(good)) / len(runs),
                 'tasks': sum(r['taskCount'] for r in runs),
                 'verifiedTasks': sum(sum(t['correct'] and t['status'] == 'done' and t['terminalRecorded']
                                           for t in r.get('tasks', [])) for r in runs)}
        # A failed trial cannot deliver the requested batch. Charge its
        # configured deadline in the all-run score, even for fast failures.
        scored = [r['elapsed'] if r['ok'] else r['timeoutSeconds'] for r in runs]
        entry['medianDeadlineScoredSeconds'] = statistics.median(scored)
        entry['meanDeadlineScoredSeconds'] = statistics.mean(scored)
        if times:
            entry.update(medianSeconds=statistics.median(times), seconds=times, spread=spread(times))
        for metric in ('cpuSeconds',):
            values = [r[metric] for r in runs if metric in r]
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
        for metric in ('connections', 'requests', 'refused', 'bytesSent', 'requestSeconds', 'maxInflight'):
            values = [r['server'][metric] for r in runs if metric in r.get('server', {})]
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
        for metric in ('startupSeconds', 'throughputPhaseSeconds', 'tailSeconds', 'deliverySeconds'):
            values = [t[metric] for r in good for t in r['tasks'] if t[metric] is not None]
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
        summary['variants'][v['name']] = entry
    if len(variants) == 2:
        a, b = (summary['variants'][v['name']] for v in variants)
        summary['deadlineScoredTimeChange'] = b['medianDeadlineScoredSeconds'] / a['medianDeadlineScoredSeconds'] - 1
        summary['meanDeadlineScoredTimeChange'] = b['meanDeadlineScoredSeconds'] / a['meanDeadlineScoredSeconds'] - 1
        if a['failed'] == b['failed'] == 0:
            summary['timeChange'] = b['medianSeconds'] / a['medianSeconds'] - 1
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--scenario', required=True, choices=sorted(SCENARIOS))
    parser.add_argument('--variant', action='append', required=True)
    parser.add_argument('--fixture-bin', required=True)
    parser.add_argument('--dmg', type=pathlib.Path, help='synthetic macOS image for app-dmg')
    parser.add_argument('--pairs', type=int, default=3)
    parser.add_argument('--max-pairs', type=int, default=5)
    parser.add_argument('--output', required=True)
    parser.add_argument('--work-dir', default=None)
    args = parser.parse_args()
    variants = [parse_variant(v) for v in args.variant]
    scenario = SCENARIOS[args.scenario]
    flags = scenario.get('server', [])
    if args.scenario == 'app-dmg':
        if not args.dmg or not args.dmg.is_file():
            parser.error('app-dmg requires a synthetic --dmg file')
        scenario['tasks'][0]['size'] = args.dmg.stat().st_size
        flags = [*flags, '-dmg', str(args.dmg.resolve())]
    fixture = Fixture(args.fixture_bin, flags)
    results = []
    try:
        for task in scenario['tasks']:
            task['sha'] = fixture.sha(task)
        pair = 0
        while True:
            order = variants if pair % 2 == 0 else list(reversed(variants))
            for variant in order:
                result = run_trial(variant, scenario, fixture, args.work_dir)
                results.append({'pair': pair, 'variant': variant['name'], 'result': result})
                print(json.dumps({'pair': pair, 'variant': variant['name'], 'ok': result.get('ok'),
                                  'elapsed': result.get('elapsed'), 'cpu': result.get('cpuSeconds'),
                                  'conns': (result.get('server') or {}).get('connections'),
                                  'requests': (result.get('server') or {}).get('requests'),
                                  'error': result.get('error', '')[:300]}), flush=True)
            pair += 1
            if pair < args.pairs:
                continue
            times = [[r['result']['elapsed'] for r in results if r['variant'] == v['name'] and r['result'].get('ok')]
                     for v in variants]
            if pair >= args.max_pairs or all(len(t) == pair and spread(t) <= 0.05 for t in times):
                break
    finally:
        fixture.close()
    report = {'scenario': args.scenario, 'variants': variants,
              'summary': summarize(args.scenario, results, variants), 'results': results}
    pathlib.Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2, ensure_ascii=False) + '\n')
    print(json.dumps(report['summary'], ensure_ascii=False))


if __name__ == '__main__':
    main()
