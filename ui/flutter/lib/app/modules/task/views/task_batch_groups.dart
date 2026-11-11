import 'package:flutter/material.dart';

import 'http_completion_status.dart';

import '../../../../api/api.dart' as api;
import '../../../../api/model/task.dart';
import '../../../../api/model/task_batch_catalog.dart';
import '../../../../api/model/task_batch_operation.dart';
import '../../../../api/model/task_batch_scope.dart';
import '../../../../api/task_batch_api.dart';
import '../../../../database/database.dart';
import '../../../../database/task_batch_journal.dart';
import '../../../../database/task_batch_journal_store.dart';

/// A read-only projection beside the existing flat task table. No timer,
/// persistence, create/cancel calls or task actions belong to this widget.
class TaskBatchGroups extends StatefulWidget {
  static final _changes = ValueNotifier<int>(0);

  /// UI invalidation only; the journal/Core remain the source of truth.
  static void referencesChanged() {
    _changes.value++;
  }

  final List<Task> tasks;
  final TaskBatchJournalStore? journal;
  const TaskBatchGroups({super.key, required this.tasks, this.journal});
  @override
  State<TaskBatchGroups> createState() => _TaskBatchGroupsState();
}

class _TaskBatchGroupsState extends State<TaskBatchGroups> {
  final _operations = <String, TaskBatchOperationRef>{};
  final _replies = <String, ScopedTaskBatchReply>{};
  final _unknown = <String>{};
  final _cursors = <String>{};
  api.ActiveApiSnapshot? _snapshot;
  String? _dataset, _nextCursor, _notice;
  int _epoch = 0, _isolated = 0, _visible = 20;
  bool _busy = false, _hasMore = false, _refreshPending = false;
  void _requestRefresh() {
    if (_busy) {
      _refreshPending = true;
    } else {
      _refresh();
    }
  }

  @override
  void initState() {
    super.initState();
    TaskBatchGroups._changes.addListener(_requestRefresh);
    _refresh();
  }

  @override
  void dispose() {
    ++_epoch;
    TaskBatchGroups._changes.removeListener(_requestRefresh);
    super.dispose();
  }

  Future<void> _refresh({bool more = false}) async {
    if (_busy) return;
    final snapshot = api.activeApiSnapshot;
    if (snapshot?.taskBatchRoute?.supported != true) return;
    _snapshot = snapshot;
    final epoch = ++_epoch;
    setState(() => _busy = true);
    try {
      final identity = await ScopedTaskBatchApi.readIdentity(snapshot!);
      final bound = ScopedTaskBatchApi(snapshot: snapshot, identity: identity);
      if (!mounted || epoch != _epoch || !snapshot.isCurrent) return;
      if (!more || identity.datasetId != _dataset) {
        _operations.clear();
        _replies.clear();
        _unknown.clear();
        _cursors.clear();
        _nextCursor = null;
        _visible = 20;
      }
      _dataset = identity.datasetId;
      final journal = (widget.journal ??
              TaskBatchJournalStore(
                  box: Database.instance.box, capacity: 10000))
          .load();
      if (journal.quarantined) {
        _notice = '本地批次引用格式未知，已保留原值；未修改或重建。';
      } else {
        final scope = TaskBatchJournalScope(
            route: snapshot.taskBatchRoute!, dataset: identity);
        _isolated = journal.journal!.isolated(scope).length;
        for (final ref in journal.journal!.queryable(scope)) {
          _operations[ref.operationId] = TaskBatchOperationRef(
              namespace: ref.namespace, operationId: ref.operationId);
        }
        _notice = null;
      }
      if (!more || _hasMore) {
        final page = await bound.catalog(limit: 20, cursor: _nextCursor);
        if (page.hasMore && !_cursors.add(page.nextCursor!)) {
          throw const TaskBatchCatalogException('catalog_cursor_not_advancing');
        }
        _hasMore = page.hasMore;
        _nextCursor = page.nextCursor;
        for (final item in page.items) {
          if (item.operationId == null) {
            _unknown.add(item.keyHash);
            continue;
          }
          try {
            _operations[item.operationId!] = TaskBatchOperationRef(
                namespace: TaskBatchOperationRef.namespaceV1,
                operationId: item.operationId!);
          } catch (_) {
            _unknown.add(item.keyHash);
          }
        }
      }
      if (more) _visible += 20;
      if (!await bound.stillSameDataset()) {
        throw const TaskBatchCatalogException('catalog_scope_changed');
      }
      for (final op in _operations.values.take(_visible)) {
        final reply = await bound.query(op);
        if (!mounted || epoch != _epoch || !snapshot.isCurrent) return;
        if (reply.envelope.message == 'dataset_mismatch') {
          throw const TaskBatchCatalogException('catalog_scope_changed');
        }
        _replies[op.operationId] = reply;
      }
    } catch (_) {
      if (mounted && epoch == _epoch) {
        _replies.clear();
        _notice = '批次数据域或查询结果尚未确认，请在原连接刷新；引用已保留。';
      }
    } finally {
      if (mounted && epoch == _epoch) {
        setState(() => _busy = false);
        if (_refreshPending) {
          _refreshPending = false;
          _refresh();
        }
      }
    }
  }

  Widget _member(TaskBatchMemberReceipt member) {
    final tasks = {for (final task in widget.tasks) task.id: task};
    final task = member.state == 'created' &&
            member.taskPresent == true &&
            member.error == null
        ? tasks[member.taskId]
        : null;
    String detail;
    if (member.state == 'pending') {
      detail = '待创建 · 大小未知';
    } else if (member.state == 'failed') {
      detail = '创建失败${member.error == null ? '' : ' · ${member.error}'}';
    } else if (member.error != null) {
      detail = '创建关联尚未确认，未采用当前任务状态';
    } else if (member.taskPresent == false) {
      detail = '已创建，任务已移除';
    } else if (task == null) {
      detail = '已创建，任务状态尚未读取';
    } else {
      const states = {
        Status.ready: '准备中',
        Status.running: '下载中',
        Status.pause: '已暂停',
        Status.wait: '等待中',
        Status.error: '任务出错',
        Status.done: '已完成'
      };
      final completion =
          HttpCompletionView.forTask(task, expectedDataset: _dataset);
      final state = completion?.labelFor(task.status) ?? states[task.status];
      detail = '$state · 已下载 ${task.progress.downloaded} 字节 · '
          '${(task.meta.res?.size ?? 0) <= 0 ? '大小未知' : '共 ${task.meta.res!.size} 字节'}';
    }
    return ListTile(
        dense: true,
        title: Text(task?.name ?? '成员 ${(member.index ?? 0) + 1}'),
        subtitle: Text(detail));
  }

  @override
  Widget build(BuildContext context) {
    if (_snapshot?.isCurrent == false) {
      return ListTile(
          title: const Text('连接已变化，原批次引用已保留。'),
          trailing: TextButton(
              onPressed: _busy ? null : _refresh, child: const Text('刷新批次')));
    }
    if (_operations.isEmpty &&
        _unknown.isEmpty &&
        _notice == null &&
        !_busy &&
        _isolated == 0 &&
        !_hasMore) {
      if (api.activeApiSnapshot?.taskBatchRoute?.supported != true) {
        return const SizedBox.shrink();
      }
      return Align(
          alignment: Alignment.centerRight,
          child: TextButton(
              key: const ValueKey('task-batch-refresh'),
              onPressed: _refresh,
              child: const Text('刷新批次')));
    }
    return SizedBox(
        key: const ValueKey('task-batch-groups'),
        height: 220,
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Row(children: [
            const Expanded(child: Text('批次（创建状态）')),
            TextButton(
                key: const ValueKey('task-batch-refresh'),
                onPressed: _busy ? null : _refresh,
                child: Text(_busy ? '正在查询…' : '刷新批次'))
          ]),
          if (_notice != null) Text(_notice!, maxLines: 2),
          if (_isolated > 0) Text('另有 $_isolated 个原连接引用已隔离保留。'),
          Expanded(
              child: ListView(children: [
            for (final op in _operations.values.take(_visible))
              ExpansionTile(
                  key: ValueKey('batch-${op.operationId}'),
                  title: Text('批次 ${op.operationId}'),
                  subtitle: Text(_replies[op.operationId]?.receipt == null ||
                          _replies[op.operationId]?.receipt?.state == 'unknown'
                      ? '结果未知，仅可查询'
                      : '已创建 ${_replies[op.operationId]!.receipt!.createdCount} · '
                          '失败 ${_replies[op.operationId]!.receipt!.failedCount} · '
                          '待创建 ${_replies[op.operationId]!.receipt!.pendingCount}'),
                  children: [
                    for (final member
                        in _replies[op.operationId]?.receipt?.members ??
                            <TaskBatchMemberReceipt>[])
                      _member(member)
                  ]),
            for (final key in _unknown)
              ListTile(
                  key: ValueKey('unknown-$key'),
                  title: const Text('未知批次记录'),
                  subtitle: const Text('记录不可识别，未猜测成员或重建。')),
            if (_hasMore || _operations.length > _visible)
              TextButton(
                  onPressed: _busy ? null : () => _refresh(more: true),
                  child: const Text('显示更多批次')),
          ])),
        ]));
  }
}
