import 'dart:math';
import 'dart:convert';
import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import '../../../../api/api.dart' as api;
import '../../../../api/http_recovery_api.dart';
import '../../../../api/model/http_recovery.dart';
import '../../../../api/model/task.dart';

/// Reuses the existing error task action. Only an explicit click can start a
/// new action; retained actions are query-only, including after local stop.
class HttpTaskRetryButton extends StatefulWidget {
  final Task task;
  final Future<void> Function() legacyContinue, refresh;
  final void Function(String) notice;
  final Color? color;
  final bool compact;
  final HttpRetrySession? session;
  const HttpTaskRetryButton(
      {super.key,
      required this.task,
      required this.legacyContinue,
      required this.refresh,
      required this.notice,
      this.color,
      this.session,
      this.compact = false});
  @override
  State<HttpTaskRetryButton> createState() => _HttpTaskRetryButtonState();
}

class _Attempt {
  final HttpRecoveryApi api;
  final HttpRetryAction action;
  _Attempt(this.api, this.action);
}

/// Application-session RAM only. Widget disposal never removes an action.
/// Keep the original client epoch as well as route/dataset/task association;
/// neither a new button nor an API reinit grants another POST for that entry.
class HttpRetrySession extends ChangeNotifier {
  final _attempts = <String, _Attempt>{};
  static const _capacity = 2048;
  String? _route(api.ActiveApiSnapshot? snapshot) {
    final route = snapshot?.taskBatchRoute;
    if (route?.supported != true) return null;
    return jsonEncode(
        [route!.product, route.identityProtocol, route.routeScope]);
  }

  _Attempt? _forTask(api.ActiveApiSnapshot? snapshot, String taskId) {
    final route = _route(snapshot);
    if (route == null) return null;
    for (final attempt in _attempts.values) {
      if (attempt.api.taskId == taskId &&
          _route(attempt.api.snapshot) == route) {
        // Also retain a different dataset's reference. The caller must hold it,
        // never reinterpret it as permission to send in the new dataset.
        return attempt;
      }
    }
    return null;
  }

  _Attempt _retain(_Attempt candidate) {
    final existing = _forTask(candidate.api.snapshot, candidate.api.taskId);
    if (existing != null) return existing;
    if (_attempts.length >= _capacity) throw StateError('retry_session_full');
    final key = jsonEncode([
      _route(candidate.api.snapshot),
      candidate.api.identity.datasetId,
      candidate.api.taskId
    ]);
    _attempts[key] = candidate;
    notifyListeners();
    return candidate;
  }
}

final _applicationRetrySession = HttpRetrySession();

class _HttpTaskRetryButtonState extends State<HttpTaskRetryButton> {
  int _epoch = 0;
  bool _busy = false;
  _Attempt? _attempt;
  CancelToken? _cancel;
  HttpRetrySession get _session => widget.session ?? _applicationRetrySession;
  _Attempt? get _retained =>
      _attempt ?? _session._forTask(api.activeApiSnapshot, widget.task.id);
  void _sessionChanged() {
    if (mounted) setState(() {});
  }

  @override
  void initState() {
    super.initState();
    _session.addListener(_sessionChanged);
  }

  @override
  void didUpdateWidget(covariant HttpTaskRetryButton oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.session != widget.session) {
      (oldWidget.session ?? _applicationRetrySession)
          .removeListener(_sessionChanged);
      _session.addListener(_sessionChanged);
      _stop();
      _attempt = null;
    }
    if (oldWidget.task.id != widget.task.id) {
      _stop();
      _attempt = null;
    }
  }

  @override
  void dispose() {
    ++_epoch;
    _cancel?.cancel();
    _session.removeListener(_sessionChanged);
    super.dispose();
  }

  void _stop() {
    ++_epoch;
    _cancel?.cancel();
    _cancel = null;
    _busy = false;
  }

  bool _current(int epoch, api.ActiveApiSnapshot snapshot, String taskId) =>
      mounted &&
      epoch == _epoch &&
      snapshot.isCurrent &&
      widget.task.id == taskId &&
      widget.task.protocol == Protocol.http &&
      widget.task.handoff?.blocked != true;
  void _message(HttpRecoveryReply reply) {
    final r = reply.recovery;
    final state = r?.receipt?.state;
    if (state == 'accepted' || state == 'replayed') {
      widget.notice('重试已受理；下载进度以任务状态为准。');
    } else if (state == 'stale') {
      widget.notice('原重试状态已变化，未重新发送。');
    } else if (_attempt != null) {
      widget.notice('重试结果尚未确认，请查询原重试。');
    } else {
      widget.notice(r?.management == 'held'
          ? '任务恢复状态受保护，暂不能重试。'
          : '当前内核未确认可安全重试，请检查任务与内核状态。');
    }
  }

  Future<void> _queryOriginal(_Attempt retained, api.ActiveApiSnapshot snapshot,
      int epoch, String taskId, CancelToken cancel) async {
    _attempt = retained;
    if (!identical(retained.api.snapshot, snapshot) ||
        !await retained.api.sameDataset(cancelToken: cancel)) {
      if (_current(epoch, snapshot, taskId)) {
        widget.notice('连接或数据域已变化，原重试仅保留，未向新数据域发送。');
      }
      return;
    }
    if (!_current(epoch, snapshot, taskId)) return;
    final reply =
        await retained.api.query(action: retained.action, cancelToken: cancel);
    if (!_current(epoch, snapshot, taskId)) return;
    _message(reply);
    await widget.refresh();
  }

  Future<void> _run() async {
    if (_busy) {
      setState(_stop);
      return;
    }
    final snapshot = api.activeApiSnapshot;
    if (snapshot == null || snapshot.taskBatchRoute?.supported != true) {
      widget.notice('当前连接尚不能确认可安全重试。');
      return;
    }
    final taskId = widget.task.id;
    if (widget.task.protocol != Protocol.http ||
        widget.task.handoff?.blocked == true) {
      return;
    }
    final epoch = ++_epoch;
    final cancel = CancelToken();
    _cancel = cancel;
    setState(() => _busy = true);
    try {
      final retained = _retained;
      if (retained != null) {
        await _queryOriginal(retained, snapshot, epoch, taskId, cancel);
        return;
      }
      if (widget.task.status != Status.error) return;
      final identity =
          await HttpRecoveryApi.readIdentity(snapshot, cancelToken: cancel);
      if (!_current(epoch, snapshot, taskId)) return;
      if (identity == null) {
        widget.notice('当前内核未确认可安全重试。');
        return;
      }
      final duringIdentity = _retained;
      if (duringIdentity != null) {
        await _queryOriginal(duringIdentity, snapshot, epoch, taskId, cancel);
        return;
      }
      final client = HttpRecoveryApi(
          snapshot: snapshot, identity: identity, taskId: taskId);
      final read = await client.query(cancelToken: cancel);
      if (!_current(epoch, snapshot, taskId)) return;
      final state = read.recovery;
      if (state == null ||
          state.status != 'error' ||
          widget.task.status != Status.error ||
          !await client.sameDataset(cancelToken: cancel)) {
        if (_current(epoch, snapshot, taskId)) _message(read);
        return;
      }
      if (!_current(epoch, snapshot, taskId)) return;
      if (widget.task.status != Status.error) return;
      final duringPreflight = _retained;
      if (duringPreflight != null) {
        await _queryOriginal(duringPreflight, snapshot, epoch, taskId, cancel);
        return;
      }
      if (state.management == 'legacy') {
        await widget.legacyContinue();
        if (_current(epoch, snapshot, taskId)) await widget.refresh();
        return;
      }
      if (state.management != 'managed' || !state.retryAllowed) {
        _message(read);
        return;
      }
      final random = Random.secure();
      final id = List.generate(
              16, (_) => random.nextInt(256).toRadixString(16).padLeft(2, '0'))
          .join();
      final action =
          HttpRetryAction('user-$id', state.episode!, state.sourceRevision!);
      // Synchronous consumption before the only POST. Cancellation, late ACK,
      // repeated click or a not_recorded query cannot recreate this permission.
      final candidate = _Attempt(client, action);
      final selected = _session._retain(candidate);
      _attempt = selected;
      if (!identical(selected, candidate)) {
        await _queryOriginal(selected, snapshot, epoch, taskId, cancel);
        return;
      }
      final reply = await client.retry(action, cancelToken: cancel);
      if (!_current(epoch, snapshot, taskId)) return;
      _message(reply);
      await widget.refresh();
    } catch (_) {
      if (_current(epoch, snapshot, taskId)) {
        widget.notice(
            _attempt == null ? '暂时无法核对重试状态，未提交重试。' : '重试结果尚未确认，请查询原重试。');
      }
    } finally {
      if (mounted && epoch == _epoch) {
        setState(() {
          _busy = false;
          _cancel = null;
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) => IconButton(
      tooltip: _busy
          ? '停止等待'
          : _retained == null
              ? '重试'
              : '查询原重试',
      color: widget.color,
      constraints: widget.compact
          ? const BoxConstraints.tightFor(width: 24, height: 28)
          : null,
      padding: widget.compact ? EdgeInsets.zero : null,
      icon: Icon(
          _busy
              ? Icons.stop_circle_outlined
              : _retained == null
                  ? Icons.refresh
                  : Icons.manage_search,
          size: widget.compact ? 16 : 18),
      onPressed: _run);
}
