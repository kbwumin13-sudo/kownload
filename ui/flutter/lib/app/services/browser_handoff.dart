import 'dart:convert';
import 'package:crypto/crypto.dart';
import '../../api/model/browser_handoff.dart';
import '../../api/model/create_task.dart';

class _PendingHandoff {
  final BrowserHandoffContext context;
  final String fingerprint;
  String? url;
  bool attempted = false, notified = false, commitStarted = false;
  String? ended, taskId;
  Future<BrowserHandoffResult>? preparing;
  _PendingHandoff(this.context, this.fingerprint, this.url);
}

/// UI coordination only. Durable phases and permission to run belong to core.
class BrowserHandoff {
  final Future<Map<String, dynamic>> Function(String, Map<String, dynamic>)
      request;
  final Map<String, bool>? Function(String, String) preferences;
  final Future<void> Function(CreateTask) confirm;
  final void Function()? begin;
  final void Function(String, String)? ended;
  final _pending = <String, _PendingHandoff>{};
  BrowserHandoff(
      {required this.request,
      required this.preferences,
      required this.confirm,
      this.begin,
      this.ended});

  Map<String, dynamic> _identity(Map value, {bool pending = false}) {
    if (!validHandoffId(value['clientId']) ||
        !const ['chrome', 'edge'].contains(value['browser']) ||
        (!pending && !validHandoffId(value['operationId'])))
      throw const FormatException('invalid_handoff_identity');
    return {
      'clientId': value['clientId'],
      'browser': value['browser'],
      if (!pending) 'operationId': value['operationId']
    };
  }

  Future<Map<String, dynamic>> capabilities() async {
    final value = await request('GET', {});
    if (value['version'] != 1)
      throw const FormatException('unsupported_handoff');
    return {'version': 1};
  }

  _PendingHandoff? _matching(String id, Map identity) {
    final entry = _pending[id];
    if (entry != null &&
        (entry.context.clientId != identity['clientId'] ||
            entry.context.browser != identity['browser'])) {
      throw const FormatException('handoff_identity_conflict');
    }
    return entry;
  }

  Future<BrowserHandoffResult> _query(Map<String, dynamic> identity) async =>
      BrowserHandoffResult.fromJson(
          await request('POST', {'action': 'query', ...identity}),
          operationId: identity['operationId']);
  void _notify(String id, _PendingHandoff? entry, String reason) {
    if (entry?.notified == true) return;
    if (entry != null) {
      entry.notified = true;
      entry.ended = reason;
      entry.url = null;
    }
    ended?.call(id, reason);
  }

  Map<String, dynamic> _accepted(String id) => {
        'version': 1,
        'operationId': id,
        'state': 'accepted',
        'taskIds': <String>[]
      };
  BrowserHandoffResult _localAbort(String id, String reason) =>
      BrowserHandoffResult(id, 'aborted', const [], reason: reason);

  Future<Map<String, dynamic>> command(Map<String, dynamic> value) async {
    final action = value['action'];
    if (action == 'handoffCapabilities') return capabilities();
    final listing = action == 'handoffQuery' &&
        value['pending'] == true &&
        value['operationId'] == null;
    final identity = _identity(value, pending: listing);
    if (listing) {
      final result = await request(
          'POST', {'action': 'query', ...identity, 'pending': true});
      if (result['version'] != 1 || result['items'] is! List)
        throw const FormatException('invalid_handoff_result');
      return {
        'version': 1,
        'items': [
          for (final item in result['items'])
            BrowserHandoffResult.fromJson(item).toJson()
        ]
      };
    }
    final id = identity['operationId'] as String;
    var entry = _matching(id, identity);
    if (action == 'handoffQuery') {
      var result = await _query(identity);
      if (result.state == 'prepared' && entry?.ended != null) {
        result =
            await _abortCore(identity, entry!.ended!, result.taskIds.single);
      }
      if (result.committed && entry != null) entry.commitStarted = true;
      if (result.state == 'aborted')
        _notify(id, entry, result.reason ?? 'browser_retained');
      if (result.state != 'unknown') return result.toJson();
      if (entry?.ended != null) return _localAbort(id, entry!.ended!).toJson();
      if (entry != null && !entry.attempted) return _accepted(id);
      return result.toJson();
    }
    if (action == 'handoffAbort') {
      final reason = value['reason'];
      if (!const [
        'browser_completed',
        'user_cancelled',
        'browser_retained'
      ].contains(reason)) throw const FormatException('invalid_handoff_reason');
      // Block a late UI click even when core has not received prepare yet.
      if (entry != null && !entry.commitStarted) _notify(id, entry, reason);
      final result = await _abortCore(identity, reason, value['taskId']);
      if (result.state == 'aborted')
        _notify(id, entry, result.reason ?? reason);
      return result.toJson();
    }
    if (action == 'handoffCommit') {
      if (entry?.ended != null)
        return (await _abortCore(identity, entry!.ended!, entry.taskId))
            .toJson();
      if (value['taskId'] is! String ||
          value['download'] is! Map ||
          value['browserState'] is! Map ||
          value['cancelConfirmed'] != true ||
          value['browserState']['state'] != 'interrupted' ||
          value['browserState']['error'] != 'USER_CANCELED')
        throw const FormatException('invalid_handoff_commit');
      final download = BrowserDownloadIdentity.fromJson(value['download']);
      if (entry != null && !entry.context.download.same(download))
        throw const FormatException('handoff_identity_conflict');
      if (entry != null) entry.commitStarted = true;
      final result = BrowserHandoffResult.fromJson(
          await request('POST', {
            'action': 'commit',
            ...identity,
            'taskId': value['taskId'],
            'download': download.toJson(),
            'browserState': value['browserState'],
            'cancelConfirmed': true
          }),
          operationId: id);
      return result.toJson();
    }
    if (action != 'handoffPrepare')
      throw const FormatException('invalid_request');
    final context = BrowserHandoffContext.fromJson(
        {'version': 1, ...identity, 'download': value['download']});
    final task =
        CreateTask.fromJson({'req': value['req'], 'opts': value['opts']});
    final url = task.req?.url;
    if (url == null ||
        !const ['http', 'https'].contains(Uri.tryParse(url)?.scheme))
      throw const FormatException('invalid_handoff_request');
    final fingerprint = sha256
        .convert(utf8.encode(jsonEncode(_ordered(task.toJson()))))
        .toString();
    if (entry != null &&
        (!entry.context.same(context) || entry.fingerprint != fingerprint))
      throw const FormatException('handoff_request_conflict');
    final existing = await _query(identity);
    if (existing.download != null && !context.download.same(existing.download!))
      throw const FormatException('handoff_identity_conflict');
    if (existing.state != 'unknown') return existing.toJson();
    if (entry?.ended != null) return _localAbort(id, entry!.ended!).toJson();
    if (entry?.attempted == true) return existing.toJson();
    if (entry != null) return _accepted(id);
    await capabilities();
    final raced = _matching(id, identity);
    if (raced != null) {
      if (!raced.context.same(context) || raced.fingerprint != fingerprint)
        throw const FormatException('handoff_request_conflict');
      if (raced.ended != null) return _localAbort(id, raced.ended!).toJson();
      return raced.attempted
          ? (await _query(identity)).toJson()
          : _accepted(id);
    }
    final prefs = preferences(context.clientId, context.browser);
    if (prefs?['enabled'] != true) throw StateError('浏览器接管已关闭，请保留原浏览器下载。');
    begin?.call();
    entry = _PendingHandoff(context, fingerprint, url);
    _pending[id] = entry;
    task
      ..browserCapture = true
      ..browserOperationId = id
      ..browserHandoff = context;
    if (prefs?['confirm'] != false) {
      await confirm(task);
      return entry.ended == null
          ? _accepted(id)
          : _localAbort(id, entry.ended!).toJson();
    }
    return (await prepareConfirmed(id, context, task)).toJson();
  }

  Future<BrowserHandoffResult> _abortCore(
          Map<String, dynamic> identity, String reason, dynamic taskId) async =>
      BrowserHandoffResult.fromJson(
          await request('POST', {
            'action': 'abort',
            ...identity,
            'reason': reason,
            if (taskId is String && taskId.isNotEmpty) 'taskId': taskId
          }),
          operationId: identity['operationId']);

  Future<BrowserHandoffResult> prepareConfirmed(
      String id, BrowserHandoffContext context, CreateTask task) async {
    final identity = {
      'operationId': id,
      'clientId': context.clientId,
      'browser': context.browser
    };
    final entry = _matching(id, identity);
    if (entry == null ||
        !entry.context.same(context) ||
        entry.url != null && entry.url != task.req?.url)
      throw StateError('交接上下文已失效，请检查原下载。');
    if (entry.ended != null) return _localAbort(id, entry.ended!);
    final running = entry.preparing;
    if (running != null) return running;
    final work = () async {
      final current = await _query(identity);
      if (current.download != null && !context.download.same(current.download!))
        throw const FormatException('handoff_identity_conflict');
      if (current.state != 'unknown') return current;
      if (entry.ended != null) return _localAbort(id, entry.ended!);
      if (entry.attempted) return current;
      if (preferences(context.clientId, context.browser)?['enabled'] != true) {
        _notify(id, entry, 'browser_retained');
        return _localAbort(id, 'browser_retained');
      }
      entry.attempted = true;
      final result = BrowserHandoffResult.fromJson(
          await request('POST', {
            'action': 'prepare',
            ...identity,
            'req': task.req!.toJson(),
            'opts': task.opts?.toJson() ?? <String, dynamic>{},
            'download': context.download.toJson()
          }),
          operationId: id);
      if (result.taskIds.isNotEmpty) entry.taskId = result.taskIds.single;
      if (entry.ended != null && result.state == 'prepared')
        return _abortCore(identity, entry.ended!, entry.taskId);
      if (result.state == 'aborted')
        _notify(id, entry, result.reason ?? 'browser_retained');
      return result;
    }();
    entry.preparing = work;
    try {
      return await work;
    } finally {
      entry.preparing = null;
      if (entry.attempted) entry.url = null;
    }
  }
}

dynamic _ordered(dynamic value) => value is Map
    ? {
        for (final key
            in (value.keys.map((k) => k.toString()).toList()..sort()))
          key: _ordered(value[key])
      }
    : value is List
        ? value.map(_ordered).toList()
        : value;
