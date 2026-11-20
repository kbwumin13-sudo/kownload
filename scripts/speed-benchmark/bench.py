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
from collections import Counter
from contextlib import suppress
import json
import math
import os
import pathlib
import random
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
        'tuning': variant['tuning'],
        'timeout': scenario.get('timeout', 600),
        'workDir': work_dir,
        'tasks': [{'url': fixture.url(t), 'name': t['name'], 'size': t['size'], 'sha': t['sha'], 'source': t['listener']}
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
                stdout, _ = process.communicate(timeout=spec['timeout'] + 100)
            except subprocess.TimeoutExpired:
                with suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
                return incomplete_trial(spec, 'trial_process_deadline')
            finally:
                # The isolated App may leave descendants after its direct
                # subprocess exits. No other trial shares this process group.
                with suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
            exit_code = process.returncode
    if exit_code != 0:
        return incomplete_trial(spec, f'trial_process_exit_{exit_code}')
    try:
        return json.loads(stdout.strip().splitlines()[-1])
    except (json.JSONDecodeError, IndexError):
        return incomplete_trial(spec, 'trial_result_unreadable')


def incomplete_trial(spec, error):
    return {'ok': False, 'error': error, 'timeoutSeconds': spec['timeout'],
            'taskCount': len(spec['tasks']), 'statsIncomplete': True,
            'tasks': [{'name': t['name'], 'source': t['source'], 'status': 'unobserved',
                       'success': False, 'correct': False, 'terminalRecorded': False,
                       'failureReason': error} for t in spec['tasks']]}


def spread(values):
    median = statistics.median(values)
    return (max(values) - min(values)) / median if median else 0.0


def percentile(ascending, fraction):
    index = (len(ascending) - 1) * fraction
    lower, upper = math.floor(index), math.ceil(index)
    return ascending[lower] + (ascending[upper] - ascending[lower]) * (index - lower)


def wilson_interval(rate, n):
    z = 1.959963984540054
    denom = 1 + z * z / n
    midpoint = (rate + z * z / (2 * n)) / denom
    half_width = z * math.sqrt(rate * (1 - rate) / n + z * z / (4 * n * n)) / denom
    return [max(0.0, midpoint - half_width), min(1.0, midpoint + half_width)]


def bootstrap_interval(batches, stat):
    rng = random.Random(51)
    quantiles = sorted(stat(rng.choices(batches, k=len(batches))) for _ in range(10_000))
    return [percentile(quantiles, .025), percentile(quantiles, .975)]


def ratio(batches):
    return sum(done for done, _ in batches) / sum(total for _, total in batches)


def task_counts(run, source=None):
    tasks = [t for t in run.get('tasks', []) if source is None or t['source'] == source]
    return sum(t['success'] for t in tasks), len(tasks)


def task_summary(runs, source=None):
    counts = [task_counts(r, source) for r in runs]
    counts = [(done, total) for done, total in counts if total]
    total = sum(total for _, total in counts)
    done = sum(done for done, _ in counts)
    rate = done / total if total else None
    if not counts:
        interval = method = None
    elif len({done / total for done, total in counts}) == 1:
        interval = wilson_interval(rate, len(counts))
        method = 'wilson_effective_n_batches_boundary_or_no_variation'
    else:
        interval = bootstrap_interval(counts, ratio)
        method = 'percentile_bootstrap_resampling_batches'
    return {'tasks': total, 'verifiedTasks': done, 'taskSuccessRate': rate,
            'taskSuccessRate95CI': interval, 'taskSuccessRate95CIMethod': method,
            'failureReasons': dict(Counter(
                t['failureReason'] for r in runs for t in r.get('tasks', [])
                if not t['success'] and (source is None or t['source'] == source)))}


def deadline_score(run):
    return run['elapsed'] if run['ok'] and run.get('elapsed') is not None else run['timeoutSeconds']


def compare_deltas(deltas):
    if not deltas:
        return None
    positive = sum(d > 0 for d in deltas)
    negative = sum(d < 0 for d in deltas)
    nonzero = positive + negative
    p = min(1.0, 2 * sum(math.comb(nonzero, i) for i in range(min(positive, negative) + 1))
            / (2 ** nonzero)) if nonzero else 1.0
    return {'meanDifference': statistics.mean(deltas), 'medianDifference': statistics.median(deltas),
            'difference95CI': bootstrap_interval(deltas, statistics.mean) if len(set(deltas)) > 1 else None,
            'positivePairs': positive, 'negativePairs': negative, 'tiedPairs': len(deltas) - nonzero,
            'pairedSignTestPValue': p, 'differences': deltas}


def paired_summary(results, names, sources):
    pairs = {}
    for row in results:
        pairs.setdefault(row['pair'], {})[row['variant']] = row['result']
    eligible = [(i, pair[names[0]], pair[names[1]]) for i, pair in pairs.items()
                if all(n in pair and not pair[n].get('statsIncomplete', False) for n in names)]

    def task_deltas(source=None):
        differences = []
        for _, base, candidate in eligible:
            left, n_left = task_counts(base, source)
            right, n_right = task_counts(candidate, source)
            if n_left and n_right:
                differences.append(right / n_right - left / n_left)
        return compare_deltas(differences)

    return {'base': names[0], 'candidate': names[1], 'totalPairs': len(pairs),
            'validPairs': len(eligible), 'pairNumbers': [i for i, _, _ in eligible],
            'excludedPairs': sorted(set(pairs) - {i for i, _, _ in eligible}),
            'taskSuccessRateDifference': task_deltas(),
            'taskSuccessRateDifferenceBySource': {source: task_deltas(source) for source in sources},
            'batchSuccessRateDifference': compare_deltas([int(right['ok']) - int(left['ok'])
                                                          for _, left, right in eligible]),
            'deadlineScoredSecondsDifference': compare_deltas([deadline_score(right) - deadline_score(left)
                                                                for _, left, right in eligible])}


def summarize(name, results, variants):
    summary = {'scenario': name, 'variants': {}}
    sources = sorted({t['source'] for row in results for t in row['result'].get('tasks', [])})
    for variant in variants:
        observed = [r['result'] for r in results if r['variant'] == variant['name']]
        runs = [r for r in observed if not r.get('statsIncomplete', False)]
        good = [r for r in runs if r['ok']]
        times = sorted(r['elapsed'] for r in good if r.get('elapsed') is not None)
        entry = dict(task_summary(runs), runs=len(observed), validRuns=len(runs),
                     statsIncompleteRuns=len(observed) - len(runs),
                     statsIncompleteReasons=dict(Counter(r.get('statsIncompleteReason') or r['error']
                                                         for r in observed if r.get('statsIncomplete'))),
                     ok=len(good), failed=len(runs) - len(good),
                     batchSuccessRate=len(good) / len(runs) if runs else None,
                     failureRate=(len(runs) - len(good)) / len(runs) if runs else None,
                     sources={source: task_summary(runs, source) for source in sources})
        # Keep incomplete evidence out of the score, but retain terminal task
        # failures and timeouts with their predeclared deadline penalty.
        scored = sorted(deadline_score(r) for r in runs)
        if scored:
            entry.update(medianDeadlineScoredSeconds=statistics.median(scored),
                         p95DeadlineScoredSeconds=percentile(scored, .95),
                         meanDeadlineScoredSeconds=statistics.mean(scored))
        if times:
            entry.update(medianSeconds=statistics.median(times), p95Seconds=percentile(times, .95),
                         seconds=times, spread=spread(times))
        for metric in ('cpuSeconds',):
            values = [r[metric] for r in runs if metric in r]
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
        for metric in ('connections', 'requests', 'refused', 'bytesSent', 'duplicateBytesSent',
                       'requestSeconds', 'maxInflight'):
            values = [r['server'][metric] for r in runs if metric in (r.get('server') or {})]
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
        for metric in ('retries', 'deferrals'):
            values = [sum(t.get(metric) or 0 for t in r['tasks']) for r in runs
                      if all(t.get(metric) is not None for t in r['tasks'])]
            if values:
                entry['total' + metric.title()] = sum(values)
                entry['median' + metric.title()] = statistics.median(values)
            entry[metric + 'ObservedRuns'] = len(values)
        for field in ('statusCounts', 'retryAfterCounts'):
            total = Counter()
            for r in runs:
                total.update((r.get('server') or {}).get(field, {}))
            entry[field] = dict(total)
        waits = {}
        for r in runs:
            for request in (r.get('server') or {}).get('requestTimeline', []):
                wait = request.get('retryAfterWaitNs')
                if wait is not None and request['status'] in (429, 503):
                    waits.setdefault(request.get('retryAfterType'), []).append(wait / 1e9)
        entry['retryAfterWaitSecondsByType'] = {
            kind: {'count': len(seconds), 'p50': statistics.median(seconds),
                   'p95': percentile(sorted(seconds), .95)} for kind, seconds in waits.items()}
        for metric in ('startupSeconds', 'throughputPhaseSeconds', 'tailSeconds', 'deliverySeconds'):
            values = sorted(t[metric] for r in runs for t in r['tasks']
                            if t['success'] and t[metric] is not None)
            if values:
                entry['median' + metric[0].upper() + metric[1:]] = statistics.median(values)
                entry['p95' + metric[0].upper() + metric[1:]] = percentile(values, .95)
        summary['variants'][variant['name']] = entry
    if len(variants) == 2:
        names = [v['name'] for v in variants]
        summary['paired'] = paired_summary(results, names, sources)
        left, right = (summary['variants'][n] for n in names)
        if summary['paired']['validPairs'] == left['runs'] == right['runs'] and left['runs']:
            summary['deadlineScoredTimeChange'] = right['medianDeadlineScoredSeconds'] / left['medianDeadlineScoredSeconds'] - 1
            summary['meanDeadlineScoredTimeChange'] = right['meanDeadlineScoredSeconds'] / left['meanDeadlineScoredSeconds'] - 1
            if left['failed'] == right['failed'] == 0:
                summary['timeChange'] = right['medianSeconds'] / left['medianSeconds'] - 1
    return summary


def write_report(output, report):
    # Readers can inspect partial evidence while long matrices are running.
    tmp = output.with_suffix(output.suffix + '.tmp')
    tmp.write_text(json.dumps(report, indent=2, ensure_ascii=False) + '\n')
    tmp.replace(output)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--scenario', required=True, choices=sorted(SCENARIOS))
    parser.add_argument('--variant', action='append', required=True)
    parser.add_argument('--fixture-bin', required=True)
    parser.add_argument('--dmg', type=pathlib.Path, help='synthetic macOS image for app-dmg')
    parser.add_argument('--pairs', type=int, default=3)
    parser.add_argument('--max-pairs', type=int)
    parser.add_argument('--pair-offset', type=int, default=0, help='next pair number for a separate supplemental run')
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--work-dir', default=None)
    args = parser.parse_args()
    variants = [parse_variant(v) for v in args.variant]
    if args.max_pairs is None:
        args.max_pairs = max(5, args.pairs)
    if not 0 < args.pairs <= args.max_pairs or args.pair_offset < 0:
        parser.error('require 0 < pairs <= max-pairs and a nonnegative pair-offset')
    if len({v['name'] for v in variants}) != len(variants):
        parser.error('variant names must be distinct')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    scenario = SCENARIOS[args.scenario]
    flags = scenario.get('server', [])
    if args.scenario == 'app-dmg':
        if not args.dmg or not args.dmg.is_file():
            parser.error('app-dmg requires a synthetic --dmg file')
        scenario['tasks'][0]['size'] = args.dmg.stat().st_size
        flags = [*flags, '-dmg', str(args.dmg.resolve())]
    fixture = Fixture(args.fixture_bin, flags)
    results = []
    report = {'schemaVersion': 2, 'scenario': args.scenario, 'variants': variants,
              'completed': False, 'results': results, 'pairOffset': args.pair_offset,
              'minimumPairs': args.pairs, 'maximumPairs': args.max_pairs}
    try:
        for task in scenario['tasks']:
            task['sha'] = fixture.sha(task)
        pair = 0
        while True:
            pair_number = pair + args.pair_offset
            order = variants if pair_number % 2 == 0 else list(reversed(variants))
            for variant in order:
                result = run_trial(variant, scenario, fixture, args.work_dir)
                results.append({'pair': pair_number, 'variant': variant['name'], 'result': result})
                write_report(args.output, report)
                print(json.dumps({'pair': pair_number, 'variant': variant['name'], 'ok': result.get('ok'),
                                  'statsIncomplete': result.get('statsIncomplete', False),
                                  'elapsed': result.get('elapsed'), 'cpu': result.get('cpuSeconds'),
                                  'conns': (result.get('server') or {}).get('connections'),
                                  'requests': (result.get('server') or {}).get('requests'),
                                  'error': result.get('error', '')[:300]}), flush=True)
            pair += 1
            if pair < args.pairs:
                continue
            times = [[r['result']['elapsed'] for r in results if r['variant'] == v['name'] and r['result'].get('ok')
                      and not r['result'].get('statsIncomplete') and r['result'].get('elapsed') is not None]
                     for v in variants]
            if pair >= args.max_pairs or all(len(t) == pair and spread(t) <= 0.05 for t in times):
                break
    finally:
        fixture.close()
    report.update(summary=summarize(args.scenario, results, variants), completed=True)
    write_report(args.output, report)
    print(json.dumps(report['summary'], ensure_ascii=False))


if __name__ == '__main__':
    main()
