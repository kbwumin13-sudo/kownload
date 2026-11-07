import 'dart:convert';

import 'package:dio/dio.dart';

import 'rpc.dart';

const operationHeader = 'X-Kownload-Operation-Id';
final _operationId = RegExp(r'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$');
typedef BrowserForward = Future<Response<dynamic>> Function(
  Map<String, dynamic> body,
  String? operationId,
  Duration budget,
);

class BrowserRpcReply {
  final int status;
  final Map<String, dynamic> body;
  const BrowserRpcReply(this.status, this.body);
  factory BrowserRpcReply.failure(
    String? id,
    String kind, {
    int status = 500,
    int code = 1000,
  }) =>
      BrowserRpcReply(status, {
        'code': code,
        'msg': '请求未完成（$kind）',
        'data': null,
        if (id != null && _operationId.hasMatch(id)) 'operationId': id,
        'error': kind,
      });
}

class _CreateReceipt {
  final DateTime expires;
  late Future<BrowserRpcReply> reply;
  bool complete = false;
  _CreateReceipt(this.expires);
}

/// Product submission semantics shared by the RPC routes and isolated fixtures.
/// Only operation IDs and final receipts are retained; request bodies are not.
class BrowserSubmissionRpc {
  final BrowserForward send;
  final Future<void> Function(Map<String, dynamic>) showCreate;
  final void Function(Map<String, dynamic>) log;
  final _creates = <String, _CreateReceipt>{};
  BrowserSubmissionRpc({
    required this.send,
    required this.showCreate,
    required this.log,
  });

  void _trace(
    String? id,
    String stage,
    Stopwatch watch,
    String domain, [
    String error = '',
  ]) {
    log({
      'operationId': id ?? '',
      'stage': stage,
      'errorCategory': error,
      'elapsedMs': watch.elapsedMilliseconds,
      'domain': domain,
    });
  }

  static Map<String, dynamic> decodeCreate(String text) {
    dynamic value;
    try {
      value = jsonDecode(text);
    } on FormatException {
      value = text;
    }
    if (value is String) {
      value = jsonDecode(
        utf8.decode(base64Decode(base64.normalize(value.replaceAll(' ', '+')))),
      );
    }
    if (value is! Map) throw const FormatException('invalid_request');
    return Map<String, dynamic>.from(value);
  }

  static String _domain(Map<String, dynamic> body) {
    final req = body['req'];
    return req is Map && req['url'] is String
        ? Uri.tryParse(req['url'])?.host ?? ''
        : '';
  }

  static void _validateId(String? id) {
    if (id != null && !_operationId.hasMatch(id)) {
      throw const FormatException('invalid_request');
    }
  }

  Future<BrowserRpcReply> create(
    String text, {
    String? operationId,
    required bool confirm,
  }) async {
    final watch = Stopwatch()..start();
    var domain = '';
    try {
      _validateId(operationId);
      final params = decodeCreate(text);
      domain = _domain(params);
      _trace(operationId, 'flutter_create_received', watch, domain);
      if (operationId != null) {
        final now = DateTime.now();
        _creates.removeWhere((_, e) => e.complete && e.expires.isBefore(now));
        final existing = _creates[operationId];
        if (existing != null) return existing.reply;
        if (_creates.length >= 512) {
          final finished =
              _creates.keys.where((id) => _creates[id]!.complete).firstOrNull;
          if (finished == null) {
            return BrowserRpcReply.failure(
              operationId,
              'request_failed',
              status: 503,
            );
          }
          _creates.remove(finished);
        }
        final receipt = _CreateReceipt(now.add(const Duration(hours: 24)));
        _creates[operationId] = receipt;
        receipt.reply = _create(
          params,
          operationId,
          confirm,
          watch,
          domain,
        ).then((reply) {
          // Retry unknown outcomes against the authoritative REST ledger with
          // the same operation ID, rather than caching a bridge failure.
          if (reply.body['error'] != null &&
              identical(_creates[operationId], receipt)) {
            _creates.remove(operationId);
          }
          return reply;
        }).whenComplete(() => receipt.complete = true);
        return receipt.reply;
      }
      return _create(params, operationId, confirm, watch, domain);
    } on FormatException {
      return BrowserRpcReply.failure(
        operationId,
        'invalid_request',
        status: 400,
      );
    } catch (_) {
      _trace(
        operationId,
        'flutter_create_failed',
        watch,
        domain,
        'request_failed',
      );
      return BrowserRpcReply.failure(operationId, 'request_failed');
    }
  }

  Future<BrowserRpcReply> _create(
    Map<String, dynamic> params,
    String? id,
    bool confirm,
    Stopwatch watch,
    String domain,
  ) async {
    try {
      if (confirm) {
        await showCreate(params);
        _trace(id, 'flutter_accepted', watch, domain);
        return BrowserRpcReply(202, {
          'code': 0,
          'msg': '',
          'data': null,
          if (id != null) 'operationId': id,
          'submission': {'state': 'accepted', 'taskIds': <String>[]},
        });
      }
      return await _forward(
        {'method': 'POST', 'path': '/api/v1/tasks', 'data': params},
        id,
        const Duration(seconds: 10),
        watch,
        domain,
      );
    } catch (_) {
      _trace(id, 'flutter_create_failed', watch, domain, 'request_failed');
      return BrowserRpcReply.failure(id, 'request_failed');
    }
  }

  Future<BrowserRpcReply> forward(
    Map<String, dynamic> body, {
    String? operationId,
  }) async {
    final watch = Stopwatch()..start();
    final data = body['data'];
    final domain = data is Map ? _domain(Map<String, dynamic>.from(data)) : '';
    try {
      _validateId(operationId);
      final target = body['path'];
      final uri = target is String ? Uri.tryParse(target) : null;
      if (uri == null ||
          uri.hasScheme ||
          uri.hasAuthority ||
          uri.hasFragment ||
          !uri.path.startsWith('/api/v1/') ||
          uri.path.contains('//') ||
          uri.path.contains('\\') ||
          uri.pathSegments.any((s) => s == '.' || s == '..') ||
          !const [
            'GET',
            'POST',
            'PUT',
            'PATCH',
            'DELETE',
            'HEAD',
            'OPTIONS',
          ].contains(body['method'])) {
        throw const FormatException('invalid_request');
      }
      _trace(operationId, 'flutter_forward_received', watch, domain);
      return await _forward(
        body,
        operationId,
        const Duration(seconds: 60),
        watch,
        domain,
      );
    } on FormatException {
      return BrowserRpcReply.failure(
        operationId,
        'invalid_request',
        status: 400,
      );
    } catch (_) {
      _trace(
        operationId,
        'flutter_forward_failed',
        watch,
        domain,
        'request_failed',
      );
      return BrowserRpcReply.failure(operationId, 'request_failed');
    }
  }

  Future<BrowserRpcReply> _forward(
    Map<String, dynamic> body,
    String? id,
    Duration budget,
    Stopwatch watch,
    String domain,
  ) async {
    Response<dynamic> response;
    try {
      response = await send(body, id, budget);
    } on DioException catch (e) {
      if (e.response == null) rethrow;
      response = e.response!;
    }
    final status = response.statusCode ?? 502;
    final value = response.data;
    final code =
        value is Map && value['code'] is int ? value['code'] as int : null;
    String? kind;
    if (status < 200 || status >= 300) {
      kind = 'http_error';
    } else if (code != null && code != 0) {
      kind = 'business_error';
    } else if (code == null || value is! Map) {
      kind = 'invalid_response';
    }
    if (kind == null &&
        id != null &&
        value['operationId'] != null &&
        value['operationId'] != id) {
      kind = 'invalid_response';
    }
    final path = Uri.tryParse(body['path'] as String? ?? '')?.path;
    final creating = body['method'] == 'POST' &&
        const ['/api/v1/tasks', '/api/v1/tasks/batch'].contains(path);
    if (kind == null && creating) {
      final submission = value['submission'];
      final ids = submission is Map ? submission['taskIds'] : null;
      if (submission is! Map ||
          submission['state'] != 'created' ||
          ids is! List ||
          ids.isEmpty ||
          ids.any((id) => id is! String || id.trim().isEmpty)) {
        kind = 'invalid_response';
      }
    }
    if (kind == null && creating) {
      final ids = value['submission']['taskIds'] as List;
      final raw = value['data'];
      final dataIds = raw is List ? raw : [raw];
      if (dataIds.length != ids.length ||
          List.generate(ids.length, (i) => dataIds[i] == ids[i])
              .contains(false)) {
        kind = 'invalid_response';
      }
    }
    if (kind != null) {
      _trace(id, 'flutter_reply_failed', watch, domain, kind);
      return BrowserRpcReply.failure(
        id,
        kind,
        status: status,
        code: code ?? 1000,
      );
    }
    final envelope = Map<String, dynamic>.from(value);
    if (id != null) envelope['operationId'] = id;
    _trace(
      id,
      creating ? 'flutter_created' : 'flutter_forwarded',
      watch,
      domain,
    );
    return BrowserRpcReply(status, envelope);
  }

  Future<void> handle(
    RpcContext ctx, {
    required bool create,
    bool forceConfirmation = false,
  }) async {
    String? id;
    BrowserRpcReply reply;
    try {
      final ids = ctx.request.headers[operationHeader];
      if (ids != null && ids.length != 1) {
        throw const FormatException('invalid_request');
      }
      id = ids?.single;
      if (create) {
        final metaText =
            ctx.request.headers['X-Kownload-Host-Meta']?.firstOrNull ??
                ctx.request.headers['X-Gopeed-Host-Meta']?.firstOrNull ??
                '{}';
        final meta = jsonDecode(metaText);
        if (meta is! Map || meta['silent'] != null && meta['silent'] is! bool) {
          throw const FormatException('invalid_request');
        }
        reply = await this.create(
          await ctx.readText(),
          operationId: id,
          confirm: forceConfirmation || meta['silent'] != true,
        );
      } else {
        reply = await forward(await ctx.readJSON(), operationId: id);
      }
    } on FormatException {
      reply = BrowserRpcReply.failure(id, 'invalid_request', status: 400);
    } catch (_) {
      reply = BrowserRpcReply.failure(id, 'request_failed');
    }
    await ctx.writeJSON(reply.body, statusCode: reply.status);
  }
}
