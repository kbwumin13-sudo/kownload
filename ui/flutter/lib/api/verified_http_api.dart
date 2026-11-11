import 'dart:convert';
import 'package:dio/dio.dart';
import 'api.dart' as api;
import 'model/create_task_batch.dart';
import 'model/task_batch_operation.dart';
import 'model/task_batch_scope.dart';
import 'task_batch_api.dart';

const verifiedHttpCapability = 'http-sha256-completion/v1';

/// Capability checking grants no send permission. The caller must first obtain
/// a new confirmed journal reference and consume its single first-send permit.
class VerifiedHttpApi extends ScopedTaskBatchApi {
  final String expectedSHA256;
  VerifiedHttpApi(
      {required super.snapshot,
      required super.identity,
      required this.expectedSHA256}) {
    if (RegExp(r'^[0-9a-f]{64}$').stringMatch(expectedSHA256) !=
        expectedSHA256) {
      throw ArgumentError('invalid_expected_sha256');
    }
  }
  String get _root => '/api/v1/datasets/${identity.datasetId}/verified-http/v1';
  Future<bool> supported() async {
    final r =
        await api.scopedTaskBatchRequest(snapshot, 'GET', '$_root/capability');
    final b = _json(r.data);
    if (!snapshot.isCurrent ||
        r.statusCode != 200 ||
        b is! Map ||
        b['code'] != 0) {
      return false;
    }
    final d = b['data'];
    if (d is! Map) return false;
    final methods = d['methods'];
    return d['scopeVersion'] is int &&
        d['scopeVersion'] == 1 &&
        d['datasetId'] == identity.datasetId &&
        d['capability'] == verifiedHttpCapability &&
        d['requestVersion'] is int &&
        d['requestVersion'] == 1 &&
        d['projectionVersion'] is int &&
        d['projectionVersion'] == 1 &&
        d['supported'] == true &&
        methods is Map &&
        methods['create'] == 'POST' &&
        methods['query'] == 'GET' &&
        methods['cancel'] == 'POST';
  }

  /// Pure local restrictions. Call before recording a new reference or
  /// consuming its first-send permission; null options still inherit in Core.
  static void validateRequest(CreateTaskBatch batch) {
    _validatedRequest(batch);
  }

  static Map<String, dynamic> _validatedRequest(CreateTaskBatch batch) {
    if (batch.reqs?.length != 1 || batch.opts != null) {
      throw ArgumentError('verified_http_single_request_required');
    }
    final item = batch.reqs!.single;
    if (item.opts?.selectFiles.isNotEmpty == true) {
      throw ArgumentError('verified_http_single_request_required');
    }
    final req = item.req;
    final uri = Uri.tryParse(req?.url ?? '');
    if (req == null ||
        uri == null ||
        !const {'http', 'https'}.contains(uri.scheme) ||
        uri.host.isEmpty ||
        (req.protocol != null && req.protocol != 'http')) {
      throw ArgumentError('verified_http_direct_request_required');
    }
    // Keep null inheritance intact; Core resolves and freezes effective options
    // before R. Never silently suppress a user or global postprocessing choice.
    final request =
        Map<String, dynamic>.from(jsonDecode(jsonEncode(item.toJson())));
    final extra = (request['opts'] as Map?)?['extra'];
    if (extra is Map &&
        (extra['autoTorrent'] == true || extra['autoExtract'] == true)) {
      throw ArgumentError('unsupported_option');
    }
    return request;
  }

  @override
  Future<ScopedTaskBatchReply> create(
      TaskBatchOperationRef operation, CreateTaskBatch batch) {
    final request = _validatedRequest(batch);
    return _send(operation, TaskBatchAction.create, 'POST', '$_root/operations',
        data: {
          'version': 1,
          'requiredCapability': verifiedHttpCapability,
          'operationId': operation.operationId,
          'request': request,
          'integrity': {'algorithm': 'sha256', 'expectedSHA256': expectedSHA256}
        });
  }

  @override
  Future<ScopedTaskBatchReply> query(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.query,
      'GET',
      '$_root/operations/${Uri.encodeComponent(operation.operationId)}');
  @override
  Future<ScopedTaskBatchReply> cancel(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.cancel,
      'POST',
      '$_root/operations/${Uri.encodeComponent(operation.operationId)}/cancel');
  Future<ScopedTaskBatchReply> _send(TaskBatchOperationRef op,
      TaskBatchAction action, String method, String path,
      {Map<String, dynamic>? data}) async {
    try {
      final response =
          await api.scopedTaskBatchRequest(snapshot, method, path, data: data);
      return VerifiedHttpReply(
          datasetId: identity.datasetId,
          operation: op,
          action: action,
          statusCode: response.statusCode,
          body: _json(response.data),
          transportFailure: snapshot.isCurrent ? null : 'route_changed');
    } on DioException catch (error) {
      return VerifiedHttpReply(
          datasetId: identity.datasetId,
          operation: op,
          action: action,
          transportFailure: error.type.name);
    }
  }

  static Object? _json(Object? body) {
    if (body is String) {
      try {
        return jsonDecode(body);
      } on FormatException {
        return body;
      }
    }
    return body;
  }
}

/// A valid S2 wrapper alone cannot authorize a verified receipt.
class VerifiedHttpReply extends ScopedTaskBatchReply {
  VerifiedHttpReply(
      {required super.datasetId,
      required super.operation,
      required super.action,
      super.statusCode,
      super.body,
      super.transportFailure});
  @override
  bool get scopeVerified =>
      super.scopeVerified &&
      (envelope.envelope?['data'] as Map)['capability'] ==
          verifiedHttpCapability;
}
