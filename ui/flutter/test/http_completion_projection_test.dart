import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/http_completion_projection.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/modules/task/views/http_completion_status.dart';

Map<String, dynamic> projection(
        {String phase = 'verified', Object? match = true}) =>
    {
      'version': 1,
      'scopeVersion': 1,
      'datasetId': 'a' * 32,
      'taskId': 'task',
      'completionId': 'completion',
      'available': true,
      'phase': phase,
      'recordedMatch': match,
      'errorCategory': null
    };
void main() {
  test(
      'projection needs matching actual dataset and current client, not just done',
      () {
    final p = HttpCompletionProjection.fromJson(projection(), taskId: 'task');
    expect(p.available, false);
    p.bindToDataset('b' * 32, isCurrent: () => true);
    expect(p.available, false);
    var current = true;
    p.bindToDataset('a' * 32, isCurrent: () => current);
    expect(p.available, true);
    expect(HttpCompletionView.fromTaskProjection(p).labelFor(Status.done),
        '校验完成状态未确认');
    current = false;
    expect(p.available, false);
  });
  for (final phase in [
    'prepared',
    'waiting_verify',
    'verifying',
    'verified',
    'published_pending_commit',
    'committed'
  ]) {
    test('$phase validates recordedMatch and never accepts the opposite value',
        () {
      final matched =
          ['verified', 'published_pending_commit', 'committed'].contains(phase);
      final raw = projection(phase: phase, match: matched ? true : null);
      final p = HttpCompletionProjection.fromJson(raw, taskId: 'task')
        ..bindToDataset('a' * 32, isCurrent: () => true);
      expect(p.available, true);
      raw['recordedMatch'] = matched ? null : true;
      final bad = HttpCompletionProjection.fromJson(raw, taskId: 'task')
        ..bindToDataset('a' * 32, isCurrent: () => true);
      expect(bad.available, false);
    });
  }
  test(
      'unknown schema, wrong task, bad linkage/phase/category stay unavailable',
      () {
    for (final change in [
      'version',
      'scope',
      'dataset',
      'task',
      'completion',
      'phase',
      'error',
      'available',
      'match'
    ]) {
      final raw = projection();
      switch (change) {
        case 'version':
          raw['version'] = 2;
        case 'scope':
          raw['scopeVersion'] = 1.0;
        case 'dataset':
          raw['datasetId'] = 'X' * 32;
        case 'task':
          raw['taskId'] = 'other';
        case 'completion':
          raw['completionId'] = '';
        case 'phase':
          raw['phase'] = 'done';
        case 'error':
          raw['errorCategory'] = 'raw /private/path';
        case 'available':
          raw['available'] = false;
        case 'match':
          raw['recordedMatch'] = false;
      }
      final p = HttpCompletionProjection.fromJson(raw, taskId: 'task')
        ..bindToDataset('a' * 32, isCurrent: () => true);
      expect(p.available, false, reason: change);
      expect(HttpCompletionView.fromTaskProjection(p).labelFor(Status.done),
          '校验完成状态未确认');
    }
  });
  test(
      'only same-domain typed errors are displayed; raw errors never interpreted',
      () {
    final p = HttpCompletionProjection.fromJson({
      ...projection(phase: 'verifying', match: null),
      'errorCategory': 'integrity_mismatch'
    }, taskId: 'task');
    expect(HttpCompletionView.fromTaskProjection(p).labelFor(Status.error),
        '校验完成状态未确认');
    p.bindToDataset('a' * 32, isCurrent: () => true);
    expect(HttpCompletionView.fromTaskProjection(p).labelFor(Status.error),
        'SHA-256 不匹配');
    expect(HttpCompletionView.fromTaskProjection(p).labelFor(Status.done),
        '校验完成状态未确认');
  });
  test('unavailable DTO carries typed category only, never a successful phase',
      () {
    final p = HttpCompletionProjection.fromJson({
      ...projection(),
      'available': false,
      'completionId': null,
      'phase': null,
      'recordedMatch': null,
      'errorCategory': 'completion_unavailable'
    }, taskId: 'task')
      ..bindToDataset('a' * 32, isCurrent: () => true);
    expect(p.available, false);
    expect(p.phase, isNull);
    expect(p.errorCategory, 'completion_unavailable');
  });
}
