import 'dart:async';

import 'package:hive/hive.dart';

import '../api/model/task_batch_operation.dart';
import 'task_batch_journal.dart';

enum JournalRecordStatus { confirmed, failed, ackUnknown, queryOnly, blocked }

class JournalRecordResult {
  final JournalRecordStatus status;
  final String? reason;
  const JournalRecordResult(this.status, [this.reason]);
  // Only this newly recorded, acknowledged call qualifies. Never a recovered ref.
  bool get confirmed => status == JournalRecordStatus.confirmed;
}

class JournalLoadResult {
  final TaskBatchJournal? journal;
  final String? isolationReason;
  const JournalLoadResult(this.journal, [this.isolationReason]);
  bool get quarantined => journal == null;
}

class _BoxFence {
  Future<void> tail = Future.value();
  // Failed/uncertain writes may have reached storage. Never grant a second
  // first-submit confirmation for an attempted reference in this Box lifetime.
  final attempted = <String, TaskBatchJournalRef>{};
  Future<T> run<T>(Future<T> Function() action) {
    final real = tail.then((_) => action());
    tail = real.then<void>((_) {}, onError: (Object _, StackTrace __) {});
    return real;
  }
}

/// Pure reference persistence. No submit, query, cancel, eviction or delete API.
class TaskBatchJournalStore {
  static const storageKey = 'task_batch_journal_v1';
  static final _fences = Expando<_BoxFence>();
  final Box box;
  final int capacity;
  late final _BoxFence _fence = _fences[box] ??= _BoxFence();
  TaskBatchJournalStore({required this.box, required this.capacity}) {
    if (capacity < 1) throw ArgumentError('invalid_journal_capacity');
  }

  JournalLoadResult load() {
    try {
      final stored = TaskBatchJournal.decode(box.get(storageKey));
      final refs = {for (final ref in stored.refs) ref.key: ref};
      for (final ref in _fence.attempted.values) {
        refs.putIfAbsent(ref.key, () => ref);
      }
      return JournalLoadResult(TaskBatchJournal(refs.values));
    } on TaskBatchJournalException catch (error) {
      return JournalLoadResult(null, error.reason);
    } catch (_) {
      return const JournalLoadResult(null, 'journal_read_failed');
    }
  }

  Future<JournalRecordResult> recordForQuery(
      TaskBatchJournalScope scope, TaskBatchOperationRef operation,
      {Duration? acknowledgementTimeout}) {
    if (!scope.usable) {
      return Future.value(const JournalRecordResult(
          JournalRecordStatus.blocked, 'untrusted_or_nonpersistent_scope'));
    }
    if (acknowledgementTimeout != null &&
        acknowledgementTimeout <= Duration.zero) {
      throw ArgumentError('invalid_journal_ack_timeout');
    }
    final ref = TaskBatchJournalRef.forQuery(scope, operation);
    final real = _fence.run(() async {
      final loaded = load();
      if (loaded.quarantined) {
        return JournalRecordResult(
            JournalRecordStatus.blocked, loaded.isolationReason);
      }
      final refs = loaded.journal!.refs;
      if (refs.any((existing) => existing.key == ref.key)) {
        return const JournalRecordResult(JournalRecordStatus.queryOnly);
      }
      if (refs.any((existing) =>
          existing.namespace == ref.namespace &&
          existing.operationId == ref.operationId)) {
        return const JournalRecordResult(
            JournalRecordStatus.blocked, 'journal_scope_mismatch');
      }
      if (refs.length >= capacity) {
        return const JournalRecordResult(
            JournalRecordStatus.blocked, 'journal_capacity');
      }
      _fence.attempted[ref.key] = ref;
      try {
        await box.put(storageKey, TaskBatchJournal([...refs, ref]).encode());
        return const JournalRecordResult(JournalRecordStatus.confirmed);
      } catch (_) {
        // A failed Future is not proof that storage did not commit.
        return const JournalRecordResult(
            JournalRecordStatus.failed, 'journal_write_failed');
      }
    });
    // Only the caller's wait is bounded. The shared fence remains attached to
    // `real`, so a late put can never be overtaken by another store's write.
    return acknowledgementTimeout == null
        ? real
        : real.timeout(acknowledgementTimeout,
            onTimeout: () => const JournalRecordResult(
                JournalRecordStatus.ackUnknown, 'journal_ack_unknown'));
  }
}
