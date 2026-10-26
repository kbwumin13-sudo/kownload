import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task_stats.dart';
import 'package:gopeed/app/views/buid_task_list_view.dart';

void main() {
  test('stats from cores without diagnostics still parse', () {
    final stats = TaskStats.fromJson({
      'connections': [
        {'downloaded': 10, 'completed': false, 'failed': false, 'retryTimes': 0},
      ],
    });
    expect(stats.phase, isNull);
    expect(stats.rangeLabel, isNull);
    expect(stats.runningStreams, 1);
    expect(stats.streamLimit, isNull);
    expect(streamsLabel(stats), '1 连接');
  });

  test('diagnostics explain streams, range decisions and rate limits', () {
    final stats = TaskStats.fromJson({
      'connections': [{}, {}],
      'phase': 'transferring',
      'rangeState': 'unsupported',
      'rangeReason': 'ignored',
      'activeStreams': 1,
      'configuredStreams': 4,
      'protocol': 'HTTP/2.0',
      'newConnections': 1,
      'reusedConnections': 3,
      'timings': {'queueMs': 1200, 'transferMs': 900, 'bogus': 'x'},
      'adaptive': {'phase': 'hold', 'target': 2},
    });
    expect(stats.phaseLabel, '传输中');
    expect(stats.rangeLabel, '不支持分段：服务器忽略了分段请求');
    expect(streamsLabel(stats), '1/4 连接');
    expect(stats.timings, {'queueMs': 1200, 'transferMs': 900});
    expect(stats.adaptiveTarget, 2);
    final cooling = TaskStats.fromJson({'phase': 'cooldown', 'activeStreams': 0, 'cooldownUntil': '2026-10-26T12:00:00Z'});
    expect(cooling.coolingDown, isTrue);
    expect(streamsLabel(cooling), '源站限流，等待中');
    expect(cooling.cooldownUntil, DateTime.utc(2026, 10, 26, 12));
  });

  test('netdisk stats keep the provider phase and nested transfer', () {
    final stats = TaskStats.fromJson({
      'phase': '等待下载',
      'transfers': [],
      'linkMs': 640,
      'queueMs': 15,
      'http': {'phase': 'transferring', 'activeStreams': 3, 'configuredStreams': 16, 'connections': [{}, {}, {}]},
    });
    expect(stats.netdiskPhase, '等待下载');
    expect(stats.linkMs, 640);
    expect(stats.timings['queueMs'], 15);
    expect(streamsLabel(stats), '3/16 连接');
    final before = TaskStats.fromJson({'phase': '解析网盘文件', 'transfers': []});
    expect(before.netdiskPhase, '解析网盘文件');
    expect(before.runningStreams, isNull);
  });

  test('unexpected payloads never throw', () {
    for (final payload in [null, 'text', 3, [], {'connections': 'x', 'activeStreams': 'two', 'timings': []}]) {
      expect(() => TaskStats.fromJson(payload), returnsNormally);
    }
    expect(streamsLabel(null), '');
  });
}
