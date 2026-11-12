import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import '../../api/model/http_path_create.dart';
import '../../api/model/task_batch_operation.dart';

Map<String, dynamic> _ownedMediaMap(Map value) =>
    Map<String, dynamic>.from(jsonDecode(jsonEncode(value)));
Object? _orderedMediaValue(Object? value) {
  if (value is Map) {
    final keys = value.keys.cast<String>().toList()..sort();
    return {for (final key in keys) key: _orderedMediaValue(value[key])};
  }
  if (value is List) return value.map(_orderedMediaValue).toList();
  return value;
}

class _PathConfirmation {
  final BrowserMediaSession session;
  final HttpPathIntent intent;
  final Map<String, dynamic> options;
  String? operationId, requestFingerprint;
  _PathConfirmation(this.session, HttpPathIntent intent, Map options)
      : intent = HttpPathIntent(path: intent.path, origin: intent.origin),
        options = _ownedMediaMap(options);
}

class BrowserMediaSession {
  final String id;
  final String clientId;
  final String view;
  final String site;
  List<Map<String, dynamic>> items;
  bool closed = false;
  int? pathFreezeVersion;
  final commands = <Map<String, dynamic>>[];
  BrowserMediaSession(this.id, this.clientId, this.view, this.site, this.items,
      {this.pathFreezeVersion});
}

/// In-memory presentation/command queue. The extension retains the original
/// tab/document/player bindings and performs every download validation.
class BrowserMedia extends ChangeNotifier {
  final Future<void> Function() show;
  final _sessions = <String, BrowserMediaSession>{};
  final _pending = <String, (String, Completer<dynamic>, _PathConfirmation?)>{};
  int _sequence = 0;
  BrowserMedia({required this.show});
  BrowserMediaSession? get active =>
      _sessions.values.where((s) => !s.closed).firstOrNull;
  bool get hasPendingPathCreate => _pending.values.any((p) => p.$3 != null);

  /// Local App confirmation only. No intent supplied by the page/extension is
  /// trusted. Repeated claims retain the original operation and full intent.
  Map<String, dynamic> claimPathCreate(
      {required String sessionId,
      required String commandId,
      required String operationId,
      required Map request}) {
    final pending = _pending[commandId];
    final confirmation = pending?.$3;
    final session = _sessions[sessionId];
    if (pending == null ||
        pending.$1 != sessionId ||
        confirmation == null ||
        session == null ||
        session.closed ||
        session.pathFreezeVersion != 1 ||
        !identical(session, confirmation.session)) {
      throw StateError('media_path_confirmation_unavailable');
    }
    TaskBatchOperationRef(
        namespace: TaskBatchOperationRef.namespaceV1, operationId: operationId);
    final req = request['req'], opts = request['opts'];
    if (req is! Map ||
        req['protocol'] != 'media' ||
        opts is! Map ||
        opts['path'] != confirmation.intent.path ||
        opts['path'] != confirmation.options['path'] ||
        opts['name'] != confirmation.options['name']) {
      throw StateError('media_path_confirmation_changed');
    }
    final canonical = jsonEncode(_orderedMediaValue(_ownedMediaMap(request)));
    if (confirmation.operationId != null &&
        (confirmation.operationId != operationId ||
            confirmation.requestFingerprint != canonical)) {
      throw StateError('media_path_confirmation_changed');
    }
    confirmation.operationId = operationId;
    confirmation.requestFingerprint = canonical;
    return confirmation.intent.toJson();
  }

  static List<Map<String, dynamic>> _items(dynamic value) {
    if (value is! List) throw const FormatException('invalid_request');
    return [
      for (final item in value)
        if (item is Map && item['id'] is String) Map<String, dynamic>.from(item)
    ];
  }

  Future<Map<String, dynamic>> handle(Map<String, dynamic> value) async {
    final id = value['sessionId'];
    if (id is! String || id.isEmpty || id.length > 128)
      throw const FormatException('invalid_request');
    switch (value['action']) {
      case 'mediaOpen':
        if (!const ['single', 'identify', 'all'].contains(value['view']) ||
            value['clientId'] is! String) {
          throw const FormatException('invalid_request');
        }
        final current = _sessions[id];
        for (final old in _sessions.values.toList()) {
          if (old.id != id && !old.closed) close(old);
        }
        if (current != null && !current.closed) {
          current.items = _items(value['items']);
          current.pathFreezeVersion = value['pathFreezeVersion'] is int
              ? value['pathFreezeVersion']
              : null;
        } else {
          _sessions[id] = BrowserMediaSession(
              id,
              value['clientId'],
              value['view'],
              value['site'] is String ? value['site'] : '',
              _items(value['items']),
              pathFreezeVersion: value['pathFreezeVersion'] is int
                  ? value['pathFreezeVersion']
                  : null);
        }
        notifyListeners();
        await show();
        return {'opened': true};
      case 'mediaPoll':
        final session = _sessions[id];
        if (session == null) return {'command': null, 'closed': true};
        final command =
            session.commands.isEmpty ? null : session.commands.removeAt(0);
        if (session.closed && session.commands.isEmpty) _sessions.remove(id);
        return {'command': command, 'closed': session.closed};
      case 'mediaResult':
        if (value['closed'] == true) {
          final session = _sessions[id];
          if (session != null) close(session);
          return {'received': session != null};
        }
        final commandId = value['commandId'];
        final pending = _pending[commandId];
        if (pending == null || pending.$1 != id) return {'received': false};
        _pending.remove(commandId);
        if (value['ok'] == true) {
          pending.$2.complete(value['data']);
        } else {
          pending.$2.completeError(StateError(value['error'] is String
              ? value['error']
              : '资源操作未完成，请回到来源页面重试。'));
        }
        return {'received': true};
      default:
        throw const FormatException('invalid_request');
    }
  }

  Future<dynamic> send(BrowserMediaSession session, String type,
      {String? itemId,
      Map<String, dynamic> options = const {},
      HttpPathIntent? pathIntent}) async {
    if (session.closed || !_sessions.containsKey(session.id))
      throw StateError('资源页面已关闭，请重新捕获。');
    if (!const [
      'LIST',
      'INSPECT',
      'CREATE',
      'RECOVERABLE',
      'UPDATE',
      'DEEP_SEARCH'
    ].contains(type)) {
      throw const FormatException('invalid_request');
    }
    if (pathIntent != null &&
        (type != 'CREATE' ||
            session.pathFreezeVersion != 1 ||
            options.containsKey('taskId') ||
            itemId == null ||
            !session.items.any((item) => item['id'] == itemId) ||
            options['path'] != pathIntent.path)) {
      throw StateError('media_path_carrier_unsupported');
    }
    final id = 'media-${++_sequence}';
    final result = Completer<dynamic>();
    _pending[id] = (
      session.id,
      result,
      pathIntent == null
          ? null
          : _PathConfirmation(session, pathIntent, options)
    );
    session.commands.add({
      'id': id,
      'type': type,
      if (itemId != null) 'itemId': itemId,
      'options': pathIntent == null ? options : _ownedMediaMap(options),
      if (pathIntent != null) 'pathFreeze': true,
    });
    try {
      final data = await result.future.timeout(const Duration(seconds: 60));
      if (type == 'LIST' && !session.closed) {
        session.items = _items(data);
        notifyListeners();
      }
      return data;
    } on TimeoutException {
      throw StateError(type == 'CREATE' || type == 'UPDATE'
          ? '尚未确认操作结果，请先检查下载任务，避免重复添加。'
          : '助手没有回应，请回到来源页面重新打开。');
    } finally {
      _pending.remove(id);
      session.commands.removeWhere((c) => c['id'] == id);
    }
  }

  void close(BrowserMediaSession session) {
    if (session.closed) return;
    session.closed = true;
    session.items = [];
    session.commands.clear();
    session.commands.add({
      'id': 'media-${++_sequence}',
      'type': 'CLOSE',
      'options': <String, dynamic>{}
    });
    for (final entry in _pending.entries.toList()) {
      if (entry.value.$1 == session.id) {
        _pending.remove(entry.key);
        entry.value.$2.completeError(StateError('资源页面已关闭。'));
      }
    }
    notifyListeners();
  }
}
