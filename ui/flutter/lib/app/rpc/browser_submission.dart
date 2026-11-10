import 'dart:async';
import 'dart:convert';
import 'dart:math';

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
  // Local UI detail only. Never serialized into native messaging receipts.
  final String? panelError;
  const BrowserRpcReply(this.status, this.body, {this.panelError});
  factory BrowserRpcReply.failure(
    String? id,
    String kind, {
    int status = 500,
    int code = 1000,
    String? panelError,
  }) =>
      BrowserRpcReply(
          status,
          {
            'code': code,
            'msg': '请求未完成（$kind）',
            'data': null,
            if (id != null && _operationId.hasMatch(id)) 'operationId': id,
            'error': kind,
          },
          panelError: panelError);
}

class _CreateReceipt {
  final DateTime expires;
  late Future<BrowserRpcReply> reply;
  bool complete = false;
  Map<String, dynamic>? confirmation;
  bool browserCapture = false;
  String? captureClientId, captureBrowser;
  Completer<bool>? gate;
  String? confirmToken;
  _CreateReceipt(this.expires);
}

/// Product submission semantics shared by the RPC routes and isolated fixtures.
/// Only operation IDs and final receipts are retained; request bodies are not.
class BrowserSubmissionRpc {
  final BrowserForward send;
  final void Function()? beginBrowserCapture;
  final void Function(String)? onBrowserCaptureCompleted;
  final Future<void> Function(Map<String, dynamic>) showCreate;
  final void Function(Map<String, dynamic>) log;
  final Future<void> Function(List<String> taskIds, String? error)? showResult;
  final Future<void> Function(Map<String, dynamic>, String?, bool)?
      showConfirmation;
  final Map<String, bool>? Function(String, String)? capturePreferences;
  final _creates = <String, _CreateReceipt>{};
  BrowserSubmissionRpc({
    required this.send,
    required this.showCreate,
    required this.log,
    this.showResult,
    this.showConfirmation,
    this.capturePreferences,
    this.beginBrowserCapture,
    this.onBrowserCaptureCompleted,
  });

  Map<String, dynamic> confirmationStatus(String id) {
    final value = _creates[id]?.confirmation;
    return value == null
        ? {'state': 'unknown', 'taskIds': <String>[]}
        : {...value, 'taskIds': List<String>.from(value['taskIds'])};
  }

  /// A passive observation may finish only this client's uncommitted capture.
  /// It never grants submission or treats fully buffered bytes as completion.
  Map<String, dynamic> observeBrowserState(
      String id, String clientId, String browser, Map browserState) {
    final receipt = _creates[id];
    if (receipt == null ||
        !receipt.browserCapture ||
        receipt.captureClientId != clientId ||
        receipt.captureBrowser != browser ||
        receipt.confirmation?['state'] != 'accepted' ||
        receipt.confirmation?['commitStarted'] == true ||
        browserState['state'] != 'complete') {
      return confirmationStatus(id);
    }
    receipt.confirmation = {
      'state': 'browser_completed',
      'taskIds': <String>[]
    };
    receipt.reply = Future.value(_browserCompletedReply(id));
    final gate = receipt.gate;
    if (gate != null && !gate.isCompleted) gate.complete(false);
    try {
      onBrowserCaptureCompleted?.call(id);
    } catch (_) {
      log({'stage': 'flutter_panel_failed'});
    }
    return confirmationStatus(id);
  }

  BrowserRpcReply _browserCompletedReply(String id) => BrowserRpcReply(202, {
        'code': 0,
        'msg': '',
        'data': null,
        'operationId': id,
        'submission': {'state': 'browser_completed', 'taskIds': <String>[]},
      });

  Future<bool> requestCommit(String id) async {
    final receipt = _creates[id];
    if (receipt == null || receipt.confirmation?['state'] != 'accepted')
      return false;
    if (!receipt.browserCapture) return true;
    if (receipt.gate != null) return receipt.gate!.future;
    final token = base64UrlEncode(
            List<int>.generate(24, (_) => Random.secure().nextInt(256)))
        .replaceAll('=', '');
    final gate = receipt.gate = Completer<bool>();
    receipt.confirmToken = token;
    receipt.confirmation!
        .addAll({'commitRequested': true, 'confirmToken': token});
    try {
      return await gate.future.timeout(const Duration(seconds: 30));
    } on TimeoutException {
      if (!gate.isCompleted) gate.complete(false);
      if (identical(receipt.gate, gate)) {
        receipt.gate = null;
        receipt.confirmToken = null;
        receipt.confirmation!.remove('confirmToken');
        receipt.confirmation!.remove('commitRequested');
      }
      return false;
    }
  }

  Map<String, dynamic> decideConfirmation(
      String id, String token, String decision, Map browserState) {
    final receipt = _creates[id];
    final gate = receipt?.gate;
    if (receipt == null ||
        gate == null ||
        gate.isCompleted ||
        token != receipt.confirmToken ||
        receipt.confirmation?['state'] != 'accepted') {
      return confirmationStatus(id);
    }
    if (decision == 'commit') {
      if (browserState['state'] != 'in_progress' ||
          browserState['paused'] != true) {
        throw const FormatException('invalid_request');
      }
      receipt.confirmation!['commitRequested'] = false;
      receipt.confirmation!['commitStarted'] = true;
      gate.complete(true);
    } else if (decision == 'browser_completed' ||
        decision == 'browser_retained') {
      if (decision == 'browser_completed' &&
          browserState['state'] != 'complete') {
        throw const FormatException('invalid_request');
      }
      receipt.confirmation = {'state': decision, 'taskIds': <String>[]};
      gate.complete(false);
    } else {
      throw const FormatException('invalid_request');
    }
    return confirmationStatus(id);
  }

  /// Called by the existing create UI, never by a second download pipeline.
  void completeConfirmation(String id,
      {List<String> taskIds = const [], bool cancelled = false}) {
    final receipt = _creates[id];
    if (receipt == null || receipt.confirmation == null) return;
    if (taskIds.isNotEmpty &&
        taskIds.every((value) => value.trim().isNotEmpty)) {
      final ids = <String>{
        ...List<String>.from(receipt.confirmation!['taskIds']),
        ...taskIds
      }.toList();
      receipt.confirmation = {'state': 'created', 'taskIds': ids};
      receipt.reply = Future.value(_confirmedReply(id, ids));
    } else if (cancelled &&
        receipt.confirmation!['state'] == 'accepted' &&
        receipt.confirmation!['commitStarted'] != true) {
      receipt.confirmation = {'state': 'cancelled', 'taskIds': <String>[]};
      if (receipt.gate != null && !receipt.gate!.isCompleted)
        receipt.gate!.complete(false);
    }
  }

  BrowserRpcReply _confirmedReply(String id, List<String> ids) =>
      BrowserRpcReply(200, {
        'code': 0,
        'msg': '',
        'operationId': id,
        'data': ids.length == 1 ? ids.single : ids,
        'submission': {'state': 'created', 'taskIds': ids},
      });

  static bool _creating(Map<String, dynamic> body) =>
      body['method'] == 'POST' &&
      const ['/api/v1/tasks', '/api/v1/tasks/batch']
          .contains(Uri.tryParse(body['path'] as String? ?? '')?.path);

  Future<BrowserRpcReply> _present(BrowserRpcReply reply) async {
    final submission = reply.body['submission'];
    final ids = submission is Map && submission['state'] == 'created'
        ? (submission['taskIds'] as List).cast<String>()
        : <String>[];
    try {
      await showResult?.call(
          ids,
          ids.isEmpty
              ? reply.panelError ?? '添加下载失败：${reply.body['msg']}'
              : null);
    } catch (_) {
      // A presentation failure must not change an authoritative receipt.
      log({'stage': 'flutter_panel_failed'});
    }
    return reply;
  }

  static String? _localError(dynamic value) {
    final message = value is Map ? value['msg'] : null;
    if (message is! String || message.trim().isEmpty) return null;
    // Backend errors can contain signed URLs or request headers. Show the
    // cause locally without copying credentials into the visible panel.
    final safe = message
        .replaceAll(RegExp(r'https?://[^\s\x22\x27]+'), '[链接]')
        .replaceAll(
            RegExp(r'(cookie|authorization)\s*[:=][^\r\n]*',
                caseSensitive: false),
            '[凭据]');
    return '添加下载失败：${safe.length > 240 ? safe.substring(0, 240) : safe}';
  }

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
    bool browserCapture = false,
    String? captureClientId,
    String? captureBrowser,
  }) async {
    final watch = Stopwatch()..start();
    var domain = '';
    try {
      _validateId(operationId);
      if (browserCapture && operationId == null) {
        throw const FormatException('invalid_request');
      }
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
        if (browserCapture) beginBrowserCapture?.call();
        final receipt = _CreateReceipt(now.add(const Duration(hours: 24)));
        receipt.browserCapture = browserCapture;
        receipt.captureClientId = captureClientId;
        receipt.captureBrowser = captureBrowser;
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
      final reply = BrowserRpcReply.failure(
        operationId,
        'invalid_request',
        status: 400,
        panelError: '添加下载失败：下载请求格式无效。',
      );
      return confirm ? reply : await _present(reply);
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
        if (id != null)
          _creates[id]?.confirmation = {
            'state': 'accepted',
            'taskIds': <String>[]
          };
        if (showConfirmation != null) {
          await showConfirmation!(
              params, id, id != null && _creates[id]?.browserCapture == true);
        } else {
          await showCreate(params);
        }
        final result = id == null ? null : _creates[id]?.confirmation;
        if (result?['state'] == 'created') {
          return _confirmedReply(id!, List<String>.from(result!['taskIds']));
        }
        if (result?['state'] == 'browser_completed')
          return _browserCompletedReply(id!);
        _trace(id, 'flutter_accepted', watch, domain);
        return BrowserRpcReply(202, {
          'code': 0,
          'msg': '',
          'data': null,
          if (id != null) 'operationId': id,
          'submission': {'state': 'accepted', 'taskIds': <String>[]},
        });
      }
      final reply = await _forward(
        {'method': 'POST', 'path': '/api/v1/tasks', 'data': params},
        id,
        const Duration(seconds: 10),
        watch,
        domain,
      );
      // Only the strictly validated REST result can publish created. While
      // awaiting it, an automatic request has no accepted confirmation.
      final submission = reply.body['submission'];
      if (id != null &&
          reply.body['error'] == null &&
          submission is Map &&
          submission['state'] == 'created') {
        _creates[id]?.confirmation = {
          'state': 'created',
          'taskIds': List<String>.from(submission['taskIds']),
        };
      }
      return await _present(reply);
    } catch (_) {
      _trace(id, 'flutter_create_failed', watch, domain, 'request_failed');
      final reply = BrowserRpcReply.failure(id, 'request_failed',
          panelError: '添加下载失败：无法连接下载服务，请稍后重试。');
      return confirm ? reply : await _present(reply);
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
      final reply = await _forward(
        body,
        operationId,
        const Duration(seconds: 60),
        watch,
        domain,
      );
      return _creating(body) ? await _present(reply) : reply;
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
      final reply = BrowserRpcReply.failure(operationId, 'request_failed',
          panelError: '添加下载失败：无法连接下载服务，请稍后重试。');
      return _creating(body) ? await _present(reply) : reply;
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
        panelError: _localError(value),
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
    bool forceCaptureConfirmation = false,
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
        final capture = meta['source'] == 'browser-capture';
        Map<String, bool>? preferences;
        if (capture && capturePreferences != null) {
          if (meta['clientId'] is! String || meta['browser'] is! String) {
            throw const FormatException('invalid_request');
          }
          preferences = capturePreferences!(meta['clientId'], meta['browser']);
          if (preferences?['enabled'] != true) {
            final rejected =
                BrowserRpcReply.failure(id, 'business_error', status: 409);
            await ctx.writeJSON(rejected.body, statusCode: rejected.status);
            return;
          }
        }
        reply = await this.create(
          await ctx.readText(),
          operationId: id,
          confirm: (capture ? forceCaptureConfirmation : forceConfirmation) ||
              (preferences == null
                  ? meta['silent'] != true
                  : preferences['confirm'] != false),
          browserCapture: capture,
          captureClientId: capture ? meta['clientId'] as String? : null,
          captureBrowser: capture ? meta['browser'] as String? : null,
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
