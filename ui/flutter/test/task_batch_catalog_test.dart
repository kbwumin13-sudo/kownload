import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';

Map<String, dynamic> page(
        {bool more = true,
        String? cursor = 'opaque-one',
        List<dynamic> items = const []}) =>
    {
      'version': 1,
      'namespace': TaskBatchOperationRef.namespaceV1,
      'datasetId': 'a' * 32,
      'persistent': true,
      'consistency': 'live-keyset',
      'filter': 'pending',
      'items': items,
      'nextCursor': cursor,
      'hasMore': more,
      'scanned': 1000,
    };
Map<String, dynamic> item(String state) => {
      'keyHash': 'b' * 64,
      'operationId': 'op-one',
      'state': state,
      if (state == 'unknown') 'error': 'operation_record_invalid',
      if (state != 'unknown') 'requestHash': 'c' * 64,
      if (state != 'unknown')
        'counts': {
          'members': 2,
          'created': 1,
          'failed': state == 'settled' ? 1 : 0,
          'pending': state == 'pending' ? 1 : 0
        },
    };
void main() {
  const identity = DatasetIdentity(
      version: 1,
      datasetId: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
      persistent: true);
  test('dataset identity keeps nonpersistent and unknown version distinct', () {
    expect(
        DatasetIdentity.fromJson(
                {'version': 1, 'datasetId': 'a' * 32, 'persistent': false})
            .persistent,
        isFalse);
    expect(
        DatasetIdentity.fromJson(
                {'version': 2, 'datasetId': 'a' * 32, 'persistent': true})
            .supported,
        isFalse);
    expect(
        DatasetIdentity.fromJson(
                {'version': 1, 'datasetId': 'A' * 32, 'persistent': true})
            .supported,
        isFalse);
    expect(
        () => DatasetIdentity.fromJson({'version': 1, 'datasetId': 'a' * 32}),
        throwsA(isA<TaskBatchCatalogException>()));
  });
  test('pending filter preserves unknown placeholders without guessed counts',
      () {
    final unknown = item('unknown')..remove('operationId');
    final value =
        TaskBatchCatalogPage.fromJson(page(items: [item('pending'), unknown]));
    expect(value.items, hasLength(2));
    expect(value.items[0].counts!.pending, 1);
    expect(value.items[0].requestHash, 'c' * 64);
    expect(value.items[1].counts, isNull);
    expect(value.items[1].operationId, isNull);
    expect(value.items[1].error, 'operation_record_invalid');
    expect(() => value.items.clear(), throwsUnsupportedError);
  });
  test(
      'empty live page with hasMore advances and never implies global snapshot',
      () {
    final scan = TaskBatchCatalogScan(identity: identity, filter: 'pending');
    expect(scan.accept(page()), isTrue);
    expect(scan.items, isEmpty);
    expect(scan.reachedLiveEnd, isFalse);
    expect(scan.nextCursor, 'opaque-one');
    expect(
        scan.accept(page(more: false, cursor: null, items: [item('pending')])),
        isTrue);
    expect(scan.reachedLiveEnd, isTrue);
    expect(scan.items.single.operationId, 'op-one');
  });
  test('repeated or cyclic cursor stops without discarding earlier items', () {
    final scan = TaskBatchCatalogScan(
        identity: identity, filter: 'pending', initialCursor: 'initial');
    expect(scan.accept(page(items: [item('pending')])), isTrue);
    expect(scan.accept(page(cursor: 'opaque-two')), isTrue);
    expect(scan.accept(page(cursor: 'initial')), isFalse);
    expect(scan.error, 'catalog_cursor_not_advancing');
    expect(scan.items, hasLength(1));
    expect(scan.reachedLiveEnd, isFalse);
  });
  for (final change in [
    'dataset',
    'filter',
    'persistent',
    'namespace',
    'version',
    'consistency'
  ]) {
    test('$change change isolates page and retains earlier references', () {
      final scan = TaskBatchCatalogScan(identity: identity, filter: 'pending');
      expect(scan.accept(page(items: [item('pending')])), isTrue);
      final changed = page(cursor: 'second');
      switch (change) {
        case 'dataset':
          changed['datasetId'] = 'd' * 32;
        case 'filter':
          changed['filter'] = 'all';
        case 'persistent':
          changed['persistent'] = false;
        case 'namespace':
          changed['namespace'] = 'another';
        case 'version':
          changed['version'] = 2;
        case 'consistency':
          changed['consistency'] = 'snapshot';
      }
      expect(scan.accept(changed), isFalse);
      expect(scan.items, hasLength(1));
      expect(scan.nextCursor, 'opaque-one');
    });
  }
  test(
      'malformed counts, cursor and unknown rows are rejected without guessed rows',
      () {
    final invalid = [
      page()..['nextCursor'] = null,
      page()..['scanned'] = 1001,
      page(items: [
        item('pending')
          ..['counts'] = {'members': 2, 'created': 2, 'failed': 0, 'pending': 1}
      ]),
      page(items: [
        item('unknown')
          ..['counts'] = {'members': 0, 'created': 0, 'failed': 0, 'pending': 0}
      ]),
      page(items: [item('settled')..['operationId'] = '../bad'])
    ];
    for (final json in invalid) {
      expect(() => TaskBatchCatalogPage.fromJson(json),
          throwsA(isA<TaskBatchCatalogException>()));
    }
  });
  test('settled means only creation has no pending members', () {
    final value = TaskBatchCatalogPage.fromJson(
        page(more: false, cursor: null, items: [item('settled')]));
    expect(value.items.single.state, 'settled');
    expect(value.items.single.counts!.created, 1);
    expect(value.items.single.counts!.failed, 1);
  });
}
