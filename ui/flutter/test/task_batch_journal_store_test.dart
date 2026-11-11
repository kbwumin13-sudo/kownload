import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/database/task_batch_journal.dart';
import 'package:gopeed/database/task_batch_journal_store.dart';

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

class ControlledBox extends Fake implements Box {
  Object? raw;
  Completer<void>? firstGate;
  final firstStarted = Completer<void>();
  bool failFirst = false, commitOnFailure = false;
  int puts = 0, deletes = 0;
  final writtenKeys = <dynamic>[];
  ControlledBox(
      {this.raw,
      this.firstGate,
      this.failFirst = false,
      this.commitOnFailure = false});
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) => raw ?? defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    writtenKeys.add(key);
    final index = puts++;
    if (index == 0) {
      firstStarted.complete();
      try {
        await firstGate?.future;
        if (failFirst) throw StateError('fixture write failure');
      } catch (_) {
        if (commitOnFailure) raw = value;
        rethrow;
      }
    }
    raw = value;
  }

  @override
  Future<void> delete(dynamic key) async {
    deletes++;
    raw = null;
  }
}

class CommittedErrorBox extends Fake implements Box {
  final Box inner;
  CommittedErrorBox(this.inner);
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      inner.get(key, defaultValue: defaultValue);
  @override
  Future<void> put(dynamic key, dynamic value) async {
    await inner.put(key, value);
    throw StateError('fixture acknowledgement failed after commit');
  }
}

void main() {
  test(
      'only first confirmed write qualifies; duplicates across stores are queryOnly',
      () async {
    final box = ControlledBox();
    final one = TaskBatchJournalStore(box: box, capacity: 4);
    final two = TaskBatchJournalStore(box: box, capacity: 4);
    final results = await Future.wait([
      one.recordForQuery(scope(), op('one')),
      two.recordForQuery(scope(), op('one'))
    ]);
    expect(results.map((r) => r.status),
        [JournalRecordStatus.confirmed, JournalRecordStatus.queryOnly]);
    expect(box.puts, 1);
    expect(box.writtenKeys.single, TaskBatchJournalStore.storageKey);
    expect(two.load().journal!.refs.single.state, 'query_required');
    expect(box.deletes, 0);
  });

  for (final end in [
    'success',
    'failure-before-commit',
    'failure-after-commit'
  ]) {
    test(
        'A timeout then B on another store waits real A $end and retains both refs',
        () async {
      final gate = Completer<void>();
      final box = ControlledBox(
          firstGate: gate, commitOnFailure: end == 'failure-after-commit');
      final one = TaskBatchJournalStore(box: box, capacity: 4);
      final two = TaskBatchJournalStore(box: box, capacity: 4);
      final a = one.recordForQuery(scope(), op('A'),
          acknowledgementTimeout: const Duration(milliseconds: 1));
      await box.firstStarted.future;
      final original = await a;
      expect(original.status, JournalRecordStatus.ackUnknown);
      expect(original.confirmed, isFalse);
      final b = two.recordForQuery(scope(), op('B'));
      await Future<void>.delayed(Duration.zero);
      expect(box.puts, 1,
          reason: 'timeout must not release the shared real-put fence');
      if (end == 'success') {
        gate.complete();
      } else {
        gate.completeError(StateError('fixture late failure'));
      }
      expect((await b).status, JournalRecordStatus.confirmed);
      expect(original.status, JournalRecordStatus.ackUnknown);
      expect(
          TaskBatchJournal.decode(box.raw).refs.map((ref) => ref.operationId),
          ['A', 'B']);
      expect((await two.recordForQuery(scope(), op('A'))).status,
          JournalRecordStatus.queryOnly);
      expect(box.puts, 2);
      expect(box.deletes, 0);
    });
  }
  for (final committed in [false, true]) {
    test(
        'failed Future may have committed=$committed and never grants repeat confirmation',
        () async {
      final box = ControlledBox(failFirst: true, commitOnFailure: committed);
      final store = TaskBatchJournalStore(box: box, capacity: 4);
      final failed = await store.recordForQuery(scope(), op('one'));
      expect(failed.status, JournalRecordStatus.failed);
      expect(failed.confirmed, isFalse);
      expect(
          (await TaskBatchJournalStore(box: box, capacity: 4)
                  .recordForQuery(scope(), op('one')))
              .status,
          JournalRecordStatus.queryOnly);
      expect(box.puts, 1);
      expect(
          (await store.recordForQuery(scope(), op('two'))).confirmed, isTrue);
      expect(
          TaskBatchJournal.decode(box.raw).refs.map((ref) => ref.operationId),
          ['one', 'two']);
    });
  }
  test('delayed acknowledgement cannot complete confirmed before actual write',
      () async {
    final gate = Completer<void>();
    final box = ControlledBox(firstGate: gate);
    var complete = false;
    final pending = TaskBatchJournalStore(box: box, capacity: 3)
        .recordForQuery(scope(), op('one'))
      ..then((_) => complete = true);
    await box.firstStarted.future;
    await Future<void>.delayed(Duration.zero);
    expect(complete, isFalse);
    gate.complete();
    expect((await pending).confirmed, isTrue);
  });
  test(
      'capacity rejects new reference without evicting unresolved or foreign refs',
      () async {
    final box = ControlledBox();
    final store = TaskBatchJournalStore(box: box, capacity: 1);
    await store.recordForQuery(scope(), op('one'));
    final before = box.raw;
    final blocked = await store.recordForQuery(scope(), op('two'));
    expect(blocked.reason, 'journal_capacity');
    expect(blocked.confirmed, isFalse);
    expect(box.raw, before);
    expect(box.puts, 1);
    expect(box.deletes, 0);
  });
  test('unknown journal/version/extra schema stays byte-for-byte unchanged',
      () async {
    for (final raw in [
      '{bad',
      jsonEncode({'journalVersion': 2, 'refs': []}),
      jsonEncode({'journalVersion': 1, 'refs': [], 'req': 'fixture-private'}),
      jsonEncode({
        'journalVersion': 1,
        'refs': [42]
      }),
      42
    ]) {
      final box = ControlledBox(raw: raw);
      final store = TaskBatchJournalStore(box: box, capacity: 4);
      expect(store.load().quarantined, isTrue);
      expect(store.load().journal, isNull);
      final result = await store.recordForQuery(scope(), op('new'));
      expect(result.status, JournalRecordStatus.blocked);
      expect(box.raw, raw);
      expect(box.puts, 0);
      expect(box.deletes, 0);
    }
  });
  test(
      'changed dataset/route never migrates same ns/op; Mem and unknown scopes write zero',
      () async {
    final box = ControlledBox();
    final store = TaskBatchJournalStore(box: box, capacity: 4);
    await store.recordForQuery(scope(), op('one'));
    final before = box.raw;
    for (final current in [
      scope(dataset: 'c'),
      scope(route: 'd'),
      scope(persistent: false),
      scope(version: 2)
    ]) {
      final result = await store.recordForQuery(current, op('one'));
      expect(result.status, JournalRecordStatus.blocked);
      expect(store.load().journal!.queryable(current), isEmpty);
      expect(store.load().journal!.isolated(current), hasLength(1));
      expect(box.raw, before);
    }
    expect(box.puts, 1);
  });
  test('new intent in different scope preserves old isolated references',
      () async {
    final box = ControlledBox();
    final store = TaskBatchJournalStore(box: box, capacity: 4);
    await store.recordForQuery(scope(), op('old'));
    await store.recordForQuery(scope(dataset: 'c'), op('new'));
    expect(
        store.load().journal!.queryable(scope(dataset: 'c')).single.operationId,
        'new');
    expect(
        store.load().journal!.isolated(scope(dataset: 'c')).single.operationId,
        'old');
  });
  test('bad catalog/cursor never clears persisted references', () async {
    final box = ControlledBox();
    final store = TaskBatchJournalStore(box: box, capacity: 4);
    await store.recordForQuery(scope(), op('one'));
    final before = box.raw;
    final scan =
        TaskBatchCatalogScan(identity: scope().dataset, filter: 'pending');
    expect(scan.accept({'version': 99}), isFalse);
    expect(box.raw, before);
    expect(store.load().journal!.refs.single.operationId, 'one');
    expect(box.puts, 1);
  });

  for (final afterCommitError in [false, true]) {
    test(
        'real Hive close/reopen retains only query references, error-after-commit=$afterCommitError',
        () async {
      final root = Directory('/tmp').createTempSync('journal-hive-');
      final name = 'refs_${root.path.hashCode.abs()}';
      Box? box;
      try {
        box = await Hive.openBox(name, path: root.path);
        await box.put('unrelated', 'fixture-original-setting');
        final store = TaskBatchJournalStore(
            box: afterCommitError ? CommittedErrorBox(box) : box, capacity: 4);
        expect(
            (await store.recordForQuery(scope(), op('one'))).status,
            afterCommitError
                ? JournalRecordStatus.failed
                : JournalRecordStatus.confirmed);
        final encoded = box.get(TaskBatchJournalStore.storageKey);
        await box.close();
        box = await Hive.openBox(name, path: root.path);
        final reopened = TaskBatchJournalStore(box: box, capacity: 4);
        expect(reopened.load().journal!.refs.single.state, 'query_required');
        expect((await reopened.recordForQuery(scope(), op('one'))).status,
            JournalRecordStatus.queryOnly);
        expect(box.get(TaskBatchJournalStore.storageKey), encoded);
        expect(box.get('unrelated'), 'fixture-original-setting');
      } finally {
        if (box?.isOpen == true) await box!.close();
        root.deleteSync(recursive: true);
      }
    });
  }
  test(
      'copied journal keeps logical ref but another route isolates it, not a physical proof',
      () async {
    final root = Directory('/tmp').createTempSync('journal-copy-');
    final copy = Directory('/tmp').createTempSync('journal-pair-');
    final name = 'refs_${root.path.hashCode.abs()}';
    Box? box;
    try {
      box = await Hive.openBox(name, path: root.path);
      await TaskBatchJournalStore(box: box, capacity: 4)
          .recordForQuery(scope(), op('one'));
      await box.close();
      File('${root.path}/$name.hive').copySync('${copy.path}/$name.hive');
      box = await Hive.openBox(name, path: copy.path);
      final copied = TaskBatchJournalStore(box: box, capacity: 4);
      expect(
          copied.load().journal!.queryable(scope()).single.operationId, 'one');
      expect((await copied.recordForQuery(scope(), op('one'))).status,
          JournalRecordStatus.queryOnly);
      expect(copied.load().journal!.queryable(scope(route: 'd')), isEmpty);
      expect(copied.load().journal!.isolated(scope(route: 'd')), hasLength(1));
    } finally {
      if (box?.isOpen == true) await box!.close();
      root.deleteSync(recursive: true);
      copy.deleteSync(recursive: true);
    }
  });
}
