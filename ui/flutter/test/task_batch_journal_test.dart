import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/database/task_batch_journal.dart';

TaskBatchJournalScope scope(
        {String route = 'a',
        String dataset = 'b',
        bool persistent = true,
        int version = 1}) =>
    TaskBatchJournalScope(
        route: JournalRouteRef(
            product: 'kownload', identityProtocol: 1, routeScope: route * 64),
        dataset: DatasetIdentity(
            version: version, datasetId: dataset * 32, persistent: persistent));
TaskBatchOperationRef op(String id) => TaskBatchOperationRef(
    namespace: TaskBatchOperationRef.namespaceV1, operationId: id);
void main() {
  test('schema stores exactly stable references and query-required state', () {
    final ref = TaskBatchJournalRef.forQuery(scope(), op('first'));
    final journal = TaskBatchJournal([ref]);
    final json = jsonDecode(journal.encode()) as Map;
    expect(json.keys.toSet(), {'journalVersion', 'refs'});
    expect((json['refs'] as List).single.keys.toSet(),
        {'route', 'datasetId', 'namespace', 'operationId', 'state'});
    expect(json['refs'][0]['route'],
        {'product': 'kownload', 'identityProtocol': 1, 'routeScope': 'a' * 64});
    final reloaded = TaskBatchJournal.decode(journal.encode());
    expect(reloaded.refs.single.state, 'query_required');
    expect(reloaded.refs.single.key, ref.key);
    expect(() => reloaded.refs.clear(), throwsUnsupportedError);
  });
  test(
      'empty old key is compatible but unknown versions and extra fields isolate',
      () {
    expect(TaskBatchJournal.decode(null).refs, isEmpty);
    final json = jsonDecode(
        TaskBatchJournal([TaskBatchJournalRef.forQuery(scope(), op('one'))])
            .encode());
    final bad = [
      42,
      {'journalVersion': 1, 'refs': []},
      '{bad',
      jsonEncode({...json, 'journalVersion': 2}),
      jsonEncode({...json, 'unrecognized': true})
    ];
    for (final raw in bad) {
      expect(() => TaskBatchJournal.decode(raw),
          throwsA(isA<TaskBatchJournalException>()));
    }
  });
  test(
      'unknown row/route/state and credential-like extra keys are not normalized away',
      () {
    final ref = TaskBatchJournalRef.forQuery(scope(), op('one')).toJson();
    for (final changed in [
      {...ref, 'state': 'created'},
      {...ref, 'operationId': '../bad'},
      {...ref, 'auth': 'fixture-only'},
      {...ref, 'datasetId': 'B' * 32},
      {
        ...ref,
        'route': {...ref['route'], 'identityProtocol': 2}
      },
      {
        ...ref,
        'route': {...ref['route'], 'build': 'not-stable'}
      },
    ]) {
      expect(
          () => TaskBatchJournal.decode(jsonEncode({
                'journalVersion': 1,
                'refs': [changed]
              })),
          throwsA(isA<TaskBatchJournalException>()));
    }
  });
  test(
      'dataset and route are independent; same namespace/op never proves same scope',
      () {
    final first = TaskBatchJournalRef.forQuery(scope(), op('shared'));
    final otherDataset =
        TaskBatchJournalRef.forQuery(scope(dataset: 'c'), op('shared'));
    final otherRoute =
        TaskBatchJournalRef.forQuery(scope(route: 'd'), op('shared'));
    final journal = TaskBatchJournal([first, otherDataset, otherRoute]);
    expect(journal.queryable(scope()).single.key, first.key);
    expect(journal.isolated(scope()), hasLength(2));
    expect(first.key, isNot(otherDataset.key));
    expect(first.key, isNot(otherRoute.key));
  });
  test(
      'Mem/unknown identity cannot become persistent references or query scope',
      () {
    final ref = TaskBatchJournalRef.forQuery(scope(), op('one'));
    for (final current in [
      scope(persistent: false),
      scope(version: 2),
      scope(dataset: 'B')
    ]) {
      expect(() => TaskBatchJournalRef.forQuery(current, op('two')),
          throwsA(isA<TaskBatchJournalException>()));
      expect(ref.matches(current), isFalse);
    }
  });
  test(
      'duplicate exact reference is quarantined rather than lossy deduplicated',
      () {
    final ref = TaskBatchJournalRef.forQuery(scope(), op('one'));
    expect(() => TaskBatchJournal([ref, ref]),
        throwsA(isA<TaskBatchJournalException>()));
  });
}
