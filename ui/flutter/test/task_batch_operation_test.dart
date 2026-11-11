import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';

Map<String, dynamic> snapshot(
        {String id = 'op-one', String state = 'pending'}) =>
    {
      'version': 1,
      'operationId': id,
      'state': state,
      'members': [
        {
          'index': 0,
          'taskId': '_created',
          'state': 'created',
          'taskPresent': false
        },
        {
          'index': 1,
          'taskId': '-failed',
          'state': 'failed',
          'error': 'invalid_request'
        },
        if (state == 'pending')
          {'index': 2, 'taskId': 'reserved', 'state': 'pending'},
      ],
      'taskIds': ['_created'],
      'createdCount': 1,
      'failedCount': 1,
      'pendingCount': state == 'pending' ? 1 : 0,
    };

void main() {
  final operation = TaskBatchOperationRef(
      namespace: TaskBatchOperationRef.namespaceV1, operationId: 'op-one');
  TaskBatchReply reply(Map<String, dynamic> data,
          {int status = 200, int code = 0}) =>
      TaskBatchReply(
          operation: operation,
          action: TaskBatchAction.create,
          statusCode: status,
          body: {'code': code, 'msg': 'fixed_category', 'data': data});

  test('caller supplies fixed namespace and valid stable operation id', () {
    for (final id in [
      '',
      '_leading',
      '../one',
      'a/b',
      'a?b',
      'a%3Ab',
      'a\n',
      'a' * 129
    ]) {
      expect(
          () => TaskBatchOperationRef(
              namespace: TaskBatchOperationRef.namespaceV1, operationId: id),
          throwsArgumentError);
    }
    expect(
        () => TaskBatchOperationRef(
            namespace: 'browser/v1', operationId: 'valid'),
        throwsArgumentError);
    expect(
        TaskBatchOperationRef(
                namespace: TaskBatchOperationRef.namespaceV1,
                operationId: 'a:._-1')
            .operationId,
        'a:._-1');
  });
  test(
      'partial receipts distinguish reserved, failed and historical created IDs',
      () {
    final result = reply(snapshot());
    expect(result.hasMatchingSnapshot, isTrue);
    expect(result.requiresQuery, isTrue);
    expect(result.createdTaskIds, ['_created']);
    expect(result.receipt!.members[0].taskPresent, isFalse);
    expect(result.receipt!.members[1].error, 'invalid_request');
    expect(result.receipt!.members[2].taskPresent, isNull);
    expect(result.receipt!.members[2].taskId, 'reserved');
    expect([
      result.receipt!.createdCount,
      result.receipt!.failedCount,
      result.receipt!.pendingCount
    ], [
      1,
      1,
      1
    ]);
  });
  test('settled mixed creation does not mean all members created', () {
    final result = reply(snapshot(state: 'settled'));
    expect(result.requiresQuery, isFalse);
    expect(result.receipt!.failedCount, 1);
    expect(result.createdTaskIds, ['_created']);
  });
  for (final status in [409, 503]) {
    test('HTTP $status and business failure retain partial receipt', () {
      final result =
          reply(snapshot(state: 'settled'), status: status, code: 1000);
      expect(result.statusCode, status);
      expect(result.code, 1000);
      expect(result.message, 'fixed_category');
      expect(result.createdTaskIds, ['_created']);
      expect(result.receipt!.members, hasLength(2));
      expect(result.requiresQuery, isTrue);
    });
  }
  test('short queryRequired receipt never fabricates settled or zero counts',
      () {
    final result = reply(
        {'version': 1, 'operationId': 'op-one', 'queryRequired': true},
        status: 503, code: 1000);
    expect(result.requiresQuery, isTrue);
    expect(result.hasMatchingSnapshot, isFalse);
    expect(result.receipt!.state, isNull);
    expect(result.receipt!.createdCount, isNull);
    expect(result.createdTaskIds, isEmpty);
    expect(result.receipt!.raw['queryRequired'], isTrue);
  });
  test('unknown record is not permission to create another operation', () {
    final result = reply({
      'version': 1,
      'operationId': 'op-one',
      'state': 'unknown',
      'members': [],
      'taskIds': [],
      'createdCount': 0,
      'failedCount': 0,
      'pendingCount': 0
    });
    expect(result.hasMatchingSnapshot, isTrue);
    expect(result.requiresQuery, isTrue);
  });
  test(
      'future version or other operation preserves raw evidence without confirming IDs',
      () {
    for (final data in [
      snapshot()..['version'] = 2,
      snapshot(id: 'other-op')
    ]) {
      final result = reply(data);
      expect(result.requiresQuery, isTrue);
      expect(result.hasMatchingSnapshot, isFalse);
      expect(result.createdTaskIds, isEmpty);
      expect(result.receipt!.taskIds, ['_created']);
    }
  });
  test(
      'unknown states and inconsistent or malformed receipts cannot confirm IDs',
      () {
    for (final data in [
      snapshot()..['state'] = 'future',
      snapshot()..['createdCount'] = 0,
      snapshot()..['members'] = [1],
      snapshot()..['taskIds'] = ['reserved'],
      snapshot()
        ..['members'] = [
          {'index': 0, 'taskId': 'x', 'state': 'future'}
        ]
    ]) {
      final result = reply(data);
      expect(result.requiresQuery, isTrue);
      expect(result.createdTaskIds, isEmpty);
      expect(result.receipt!.raw, data);
    }
  });
  test('unknown nested values are copied and retained without mutation', () {
    final data = snapshot()
      ..['futureProvider'] = {
        'nested': [null, true, 9007199254740993]
      };
    final result = reply(data);
    (data['futureProvider']['nested'] as List)[2] = 0;
    expect(result.receipt!.raw['futureProvider']['nested'],
        [null, true, 9007199254740993]);
    expect(
        () => (result.receipt!.raw['futureProvider']['nested'] as List)
            .add('bad'),
        throwsUnsupportedError);
    expect(() => result.createdTaskIds.add('bad'), throwsUnsupportedError);
  });
  test('redirect body cannot be an authoritative receipt', () {
    final result = reply(snapshot(state: 'settled'), status: 307);
    expect(result.requiresQuery, isTrue);
    expect(result.createdTaskIds, isEmpty);
    expect(result.receipt!.taskIds, ['_created']);
  });
}
