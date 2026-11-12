import 'dart:convert';

import '../../api/api.dart' as api;
import '../../api/model/task_batch_operation.dart';
import '../../api/task_batch_api.dart';
import '../rpc/browser_submission.dart';
import '../rpc/rpc.dart';

typedef MediaPathClaim = Map<String, dynamic> Function({
  required String sessionId,
  required String commandId,
  required String operationId,
  required Map request,
});

/// App-confirmed media requests retain the extension's original operation and
/// complete request. The browser supplies correlation, never path provenance.
class MediaPathSubmission {
  static const kind = 'media-path-create/v1';
  static const capability = 'media-path-freeze/v1';
  final MediaPathClaim claim;
  final bool Function() ready;
  final _submitted = <String, Future<TaskBatchReceipt>>{};

  MediaPathSubmission({required this.claim, required this.ready});

  Object? _json(Object? value) {
    try {
      return value is String ? jsonDecode(value) : value;
    } catch (_) {
      throw StateError('submission_unknown');
    }
  }

  Map _data(dynamic response) {
    final body = _json(response.data);
    if (response.statusCode != 200 || body is! Map || body['code'] != 0 ||
        body['data'] is! Map) throw StateError('submission_unknown');
    return body['data'] as Map;
  }

  TaskBatchReceipt _receipt(Map data, String dataset, String id) {
    final value = data['operation'];
    if (data['scopeVersion'] is! int || data['scopeVersion'] != 1 || data['datasetId'] != dataset ||
        data['namespace'] != TaskBatchOperationRef.namespaceV1 ||
        data['kind'] != kind || value is! Map) {
      throw StateError('submission_unknown');
    }
    late final TaskBatchReceipt receipt;
    try {
      receipt = TaskBatchReceipt.fromJson(Map<String, dynamic>.from(value));
    } catch (_) {
      throw StateError('submission_unknown');
    }
    if (!receipt.hasV1Snapshot || receipt.operationId != id) {
      throw StateError('submission_unknown');
    }
    final caps = data['requiredCapabilities'];
    if (caps is! List || (receipt.state == 'unknown'
        ? caps.isNotEmpty
        : caps.length != 1 || caps.single != capability)) {
      throw StateError('submission_unknown');
    }
    return receipt;
  }

  Future<TaskBatchReceipt> _query(api.ActiveApiSnapshot snapshot,
      String dataset, String id) async {
    final response = await api.scopedTaskBatchRequest(snapshot, 'GET',
        '/api/v1/datasets/$dataset/media-path/v1/operations/$id');
    if (!snapshot.isCurrent) throw StateError('submission_unknown');
    return _receipt(_data(response), dataset, id);
  }

  Future<TaskBatchReceipt> _create(api.ActiveApiSnapshot snapshot,
      String dataset, String id, Map params, Map meta) async {
    final old = await _query(snapshot, dataset, id);
    // Existing R1, another kind, pending and unknown ACK cannot turn into a
    // second create. Only a confirmed empty R2 query admits this first send.
    if (old.state != 'unknown') return old;
    final intent = claim(sessionId: meta['sessionId'],
        commandId: meta['commandId'], operationId: id, request: params);
    final path = (params['opts'] as Map?)?['path'];
    if (params.keys.any((k) => k != 'req' && k != 'opts') ||
        (params['req'] as Map?)?['protocol'] != 'media' ||
        path is! String || intent['version'] != 1 || intent['path'] != path ||
        !const {'default', 'user_selected', 'manual_category',
          'api_explicit', 'source_suggested', 'unknown'}.contains(intent['origin'])) {
      throw const FormatException('invalid_request');
    }
    final root = '/api/v1/datasets/$dataset/media-path/v1';
    final caps = _data(await api.scopedTaskBatchRequest(
        snapshot, 'GET', '$root/capability'));
    final required = caps['requiredCapabilities'], methods = caps['methods'];
    if (!snapshot.isCurrent || !ready() || caps['scopeVersion'] is! int || caps['scopeVersion'] != 1 ||
        caps['datasetId'] != dataset || caps['kind'] != kind ||
        caps['requestVersion'] is! int || caps['requestVersion'] != 1 || caps['supported'] != true ||
        required is! List || required.length != 1 ||
        required.single != capability || methods is! Map ||
        methods['create'] != 'POST' || methods['query'] != 'GET' ||
        methods['cancel'] != 'POST' || methods['preview'] != 'POST') {
      throw StateError('submission_unknown');
    }
    // Recheck the live App confirmation after the asynchronous capability read.
    // The service binds the same operation and complete canonical request.
    claim(sessionId: meta['sessionId'], commandId: meta['commandId'],
        operationId: id, request: params);
    try {
      final response = await api.scopedTaskBatchRequest(snapshot, 'POST',
          '$root/operations', data: {
        'version': 1, 'operationId': id,
        'requiredCapabilities': [capability], 'request': params,
        'pathIntent': intent,
      });
      if (!snapshot.isCurrent) throw StateError('submission_unknown');
      return _receipt(_data(response), dataset, id);
    } catch (_) {
      // No failure after sending can be reported as a definitely unsent 4xx.
      throw StateError('submission_unknown');
    }
  }

  Future<void> handle(RpcContext ctx, {required bool create}) async {
    String? id;
    try {
      final ids = ctx.request.headers[operationHeader];
      if (ids?.length != 1) throw const FormatException('invalid_request');
      id = ids!.single;
      TaskBatchOperationRef(namespace: TaskBatchOperationRef.namespaceV1,
          operationId: id);
      final params = await ctx.readJSON();
      if (!create && params.isNotEmpty) {
        throw const FormatException('invalid_request');
      }
      final snapshot = api.activeApiSnapshot;
      if (snapshot == null || !snapshot.isCurrent || !ready()) {
        throw StateError('submission_unknown');
      }
      final identity = await ScopedTaskBatchApi.readIdentity(snapshot);
      final dataset = identity.datasetId;
      TaskBatchReceipt receipt;
      if (create) {
        final values = ctx.request.headers['X-Kownload-Host-Meta'];
        if (values?.length != 1) throw const FormatException('invalid_request');
        final meta = jsonDecode(values!.single);
        if (meta is! Map || meta['sessionId'] is! String ||
            meta['commandId'] is! String) {
          throw const FormatException('invalid_request');
        }
        if (_submitted.length >= 512 && !_submitted.containsKey('$dataset/$id')) {
          throw StateError('submission_unknown');
        }
        // Keep an uncertain first send in memory too. A subsequent invocation
        // can only observe its original result; transport errors do not reset it.
        receipt = await _submitted.putIfAbsent('$dataset/$id',
            () => _create(snapshot, dataset, id!, params, meta));
      } else {
        receipt = await _query(snapshot, dataset, id);
      }
      if (receipt.state != 'settled' || receipt.queryRequired ||
          receipt.createdCount != 1 || receipt.failedCount != 0 ||
          receipt.pendingCount != 0 || receipt.taskIds.length != 1) {
        throw StateError('submission_unknown');
      }
      await ctx.writeJSON({'code': 0, 'msg': '', 'operationId': id,
        'data': receipt.taskIds,
        'submission': {'state': 'created', 'taskIds': receipt.taskIds}});
    } on FormatException {
      final reply = BrowserRpcReply.failure(id, 'invalid_request', status: 400);
      await ctx.writeJSON(reply.body, statusCode: reply.status);
    } on ArgumentError {
      final reply = BrowserRpcReply.failure(id, 'invalid_request', status: 400);
      await ctx.writeJSON(reply.body, statusCode: reply.status);
    } catch (_) {
      final reply = BrowserRpcReply.failure(id, 'submission_unknown', status: 503);
      await ctx.writeJSON(reply.body, statusCode: reply.status);
    }
  }
}
