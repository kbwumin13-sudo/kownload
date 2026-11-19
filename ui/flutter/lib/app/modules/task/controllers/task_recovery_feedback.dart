import 'package:get/get.dart';

import '../../../../api/api.dart';
import '../../../../api/model/task.dart';

enum RecoveryStage {
  checking('taskRecoveryChecking'),
  submitted('taskRecoverySubmitted'),
  unknown('taskRecoveryUnknown'),
  queryRetry('taskRecoveryQueryRetry'),
  rejected('taskRecoveryRejected'),
  unsafeLink('taskRecoveryUnsafeLink'),
  continueRejected('taskRecoveryContinueRejected'),
  cooldown('taskRecoveryCooldown'),
  stopped('taskRecoveryStopped'),
  failed('taskRecoveryFailed'),
  resumed('taskRecoveryResumed');

  final String _key;
  const RecoveryStage(this._key);
  String get label => _key.tr;
}

class TaskRecoveryAttempt {
  final ActiveApiSnapshot snapshot;
  final String taskId;
  final DateTime createdAt, updatedAt;
  final int downloaded;
  final bool linkUpdate;
  RecoveryStage stage = RecoveryStage.checking;

  TaskRecoveryAttempt(this.snapshot, Task task, this.linkUpdate)
      : taskId = task.id,
        createdAt = task.createdAt,
        updatedAt = task.updatedAt,
        downloaded = task.progress.downloaded;

  bool matches(Task task) =>
      identical(
          snapshot.httpRecoveryScope, activeApiSnapshot?.httpRecoveryScope) &&
      task.createdAt == createdAt;

  bool get blocksWrite =>
      stage == RecoveryStage.checking ||
      stage == RecoveryStage.submitted ||
      stage == RecoveryStage.unknown ||
      stage == RecoveryStage.queryRetry;
}

/// Observe the existing task-list stream. A successful command or a running
/// status alone does not establish that the original transfer advanced.
class TaskRecoveryFeedback {
  final _attempts = <String, TaskRecoveryAttempt>{}.obs;

  RecoveryStage? stage(Task task) {
    final attempt = _attempts[task.id];
    if (attempt == null || !attempt.matches(task)) return null;
    if (!attempt.snapshot.isCurrent &&
        attempt.blocksWrite &&
        attempt.stage != RecoveryStage.queryRetry) {
      return RecoveryStage.unknown;
    }
    return attempt.stage;
  }

  bool isLinkUpdate(Task task) {
    final attempt = _attempts[task.id];
    return attempt?.matches(task) == true && attempt!.linkUpdate;
  }

  bool blocksWrite(Task task) {
    final attempt = _attempts[task.id];
    return attempt?.matches(task) == true && attempt!.blocksWrite;
  }

  TaskRecoveryAttempt? begin(Task task,
      {bool queryRetry = false, bool linkUpdate = false}) {
    final snapshot = activeApiSnapshot;
    if (snapshot == null ||
        task.status != Status.error && task.status != Status.pause ||
        task.handoff?.blocked == true ||
        blocksWrite(task) &&
            !(queryRetry && stage(task) == RecoveryStage.queryRetry)) {
      return null;
    }
    final attempt = TaskRecoveryAttempt(snapshot, task, linkUpdate);
    _attempts[task.id] = attempt;
    return attempt;
  }

  void report(TaskRecoveryAttempt attempt, RecoveryStage stage) {
    if (!attempt.snapshot.isCurrent ||
        !identical(_attempts[attempt.taskId], attempt)) {
      return;
    }
    attempt.stage = stage;
    _attempts.refresh();
  }

  void observe(Iterable<Task> tasks, {required bool completeList}) {
    if (_attempts.isEmpty) return;
    final present = <String>{};
    for (final task in tasks) {
      present.add(task.id);
      final attempt = _attempts[task.id];
      if (attempt == null) continue;
      if (!attempt.matches(task)) {
        _attempts.remove(task.id);
      } else if (!attempt.snapshot.isCurrent) {
        // Keep an unresolved write from an older connection epoch blocked.
        if (!attempt.blocksWrite) _attempts.remove(task.id);
      } else if (attempt.stage == RecoveryStage.submitted ||
          attempt.stage == RecoveryStage.unknown) {
        if (task.updatedAt.isAfter(attempt.updatedAt) &&
            task.status == Status.error) {
          report(attempt, RecoveryStage.failed);
        } else if (task.updatedAt.isAfter(attempt.updatedAt) &&
            task.progress.downloaded > attempt.downloaded &&
            (task.status == Status.running || task.status == Status.done)) {
          report(attempt, RecoveryStage.resumed);
        } else if (task.updatedAt.isAfter(attempt.updatedAt) &&
            task.status == Status.pause) {
          report(attempt, RecoveryStage.stopped);
        }
      } else if (attempt.stage == RecoveryStage.resumed &&
          task.status == Status.error) {
        _attempts.remove(task.id);
      }
    }
    if (completeList) _attempts.removeWhere((id, _) => !present.contains(id));
  }
}
