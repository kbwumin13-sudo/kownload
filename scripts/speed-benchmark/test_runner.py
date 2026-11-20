import hashlib
import json
import pathlib
import tempfile
import unittest
from unittest.mock import Mock, patch

import bench
import trial


class FixtureDrainTests(unittest.TestCase):
    def test_detailed_snapshot_is_read_only_after_zero_and_rechecked(self):
        fixture = trial.FixtureClient('http://127.0.0.1:1')
        fixture.get = Mock(side_effect=[
            b'{"inflight":1}', b'{"inflight":0}', b'{"inflight":1}',
            b'{"inflight":0}', b'{"inflight":0,"bytesSent":4}',
        ])
        with patch.object(trial.time, 'sleep'):
            server, reason = fixture.drain(1, detailed=True)
        self.assertIsNone(reason)
        self.assertEqual(server, {'inflight': 0, 'bytesSent': 4})
        self.assertEqual([call.args[0] for call in fixture.get.call_args_list],
                         ['/stats?summary=1', '/stats?summary=1', '/stats',
                          '/stats?summary=1', '/stats'])

    def test_unfinished_request_is_not_reported_as_drained(self):
        fixture = trial.FixtureClient('http://127.0.0.1:1')
        fixture.get = Mock(return_value=b'{"inflight":1}')
        with patch.object(trial.time, 'monotonic', side_effect=[0, 0, 1, 1]), \
                patch.object(trial.time, 'sleep'):
            server, reason = fixture.drain(1)
        self.assertEqual(server, {'inflight': 1})
        self.assertEqual(reason, 'fixture_drain_timeout')


class TrialEvidenceTests(unittest.TestCase):
    def test_keeps_ramp_probe_without_exporting_raw_fields(self):
        with tempfile.TemporaryDirectory() as tmp:
            trace = pathlib.Path(tmp) / 'trace.jsonl'
            raw = {'kind': 'rampProbe', 'port': 1, 'atNs': 42, 'secret': 'discard'}
            trace.with_suffix('.jsonl.budget').write_text(json.dumps(raw) + '\n')
            self.assertEqual(trial.read_budget_timeline(trace),
                             [{'kind': 'rampProbe', 'port': 1, 'atNs': 42}])

    def test_captures_outcome_before_stop_and_marks_drain_timeout(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = pathlib.Path(tmp)
            trace = work / 'trace.jsonl'
            payload = b'ready'
            spec = {'connections': 4, 'timeout': 2,
                    'tasks': [{'name': 'x.bin', 'url': 'http://127.0.0.1/f/x.bin',
                               'source': 'plain', 'size': len(payload),
                               'sha': hashlib.sha256(payload).hexdigest()}]}
            order = []
            core = Mock(app=False)
            core.start.side_effect = lambda: order.append('start')
            core.stop.side_effect = lambda: order.append('stop')

            def api(path, data=None, method='GET'):
                if path == 'info':
                    return {'version': 'test'}
                if path == 'config' and method == 'GET':
                    return {'protocolConfig': {}}
                if path == 'config':
                    return None
                if path == 'tasks/batch':
                    destination = pathlib.Path(data['opts']['path'])
                    (destination / 'x.bin').write_bytes(payload)
                    trace.write_text(json.dumps({'name': 'x.bin', 'status': 'done',
                                                 'retries': 2, 'deferrals': 1}) + '\n')
                    return ['unit-task']
                if path == 'tasks':
                    order.append('task_status')
                    return [{'id': 'unit-task', 'status': 'done'}]
                if path == 'tasks/unit-task/stats':
                    order.append('task_stats')
                    return {'lastError': 'HTTP 429 URL http://user:password@invalid'}
                raise AssertionError(path)

            def drain(timeout, detailed=False):
                order.append('final_drain' if detailed else 'initial_drain')
                if detailed:
                    return {'inflight': 1}, 'fixture_drain_timeout'
                return {'inflight': 0}, None

            core.api.side_effect = api
            fixture = Mock()
            fixture.get.side_effect = lambda *args: order.append('reset')
            fixture.drain.side_effect = drain
            result = trial.execute(core, trace, spec, work, fixture)
        self.assertEqual(order, ['initial_drain', 'reset', 'start', 'task_status',
                                 'task_stats', 'stop', 'final_drain'])
        self.assertTrue(result['statsIncomplete'])
        self.assertEqual(result['statsIncompleteReason'], 'fixture_drain_timeout')
        task, = result['tasks']
        self.assertTrue(task['success'])
        self.assertEqual((task['lastError'], task['retries'], task['deferrals']), ('HTTP 429', 2, 1))
        self.assertNotIn('password', json.dumps(result))


def sample(done, incomplete=False):
    return {'ok': all(done), 'elapsed': 1, 'timeoutSeconds': 5,
            'statsIncomplete': incomplete, 'error': 'fixture_drain_timeout' if incomplete else '',
            'taskCount': len(done),
            'tasks': [dict(source='plain', success=success, failureReason=None if success else 'limit',
                           startupSeconds=None, throughputPhaseSeconds=None,
                           tailSeconds=None, deliverySeconds=None) for success in done]}


class BenchmarkSummaryTests(unittest.TestCase):
    def test_task_and_batch_denominators_and_paired_eligibility(self):
        rows = [dict(pair=pair, variant=variant, result=result) for pair, variant, result in (
            (0, 'base', sample([True, False])), (0, 'app', sample([True, True])),
            (1, 'app', sample([True, True], incomplete=True)), (1, 'base', sample([True, True])),
        )]
        summary = bench.summarize('test', rows, [{'name': 'base'}, {'name': 'app'}])
        base, candidate = (summary['variants'][k] for k in ('base', 'app'))
        self.assertEqual(base['taskSuccessRate'], .75)
        self.assertEqual(base['batchSuccessRate'], .5)
        self.assertEqual(base['failureReasons'], {'limit': 1})
        self.assertEqual(candidate['taskSuccessRate'], 1)
        self.assertEqual(candidate['batchSuccessRate'], 1)
        self.assertEqual(candidate['statsIncompleteRuns'], 1)
        self.assertLess(candidate['taskSuccessRate95CI'][0], 1)
        paired = summary['paired']
        self.assertEqual(paired['excludedPairs'], [1])
        self.assertEqual(paired['taskSuccessRateDifference']['differences'], [.5])
        self.assertEqual(paired['deadlineScoredSecondsDifference']['differences'], [-4])


if __name__ == '__main__':
    unittest.main()
