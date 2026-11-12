import 'dart:convert';
import 'package:dio/dio.dart';
import 'api.dart' as api;
import 'model/create_task_batch.dart';
import 'model/http_path_create.dart';
import 'model/task_batch_operation.dart';
import 'model/task_batch_scope.dart';
import 'task_batch_api.dart';

Object? _json(Object? value) {
  if (value is! String) return value;
  try {
    return jsonDecode(value);
  } on FormatException {
    return value;
  }
}

bool _sameCaps(Object? value, List<String> expected) =>
    value is List &&
    value.length == expected.length &&
    List.generate(expected.length, (i) => value[i] == expected[i])
        .every((v) => v);

class HttpPathApi extends ScopedTaskBatchApi {
  final HttpPathCreateRequest request;
  HttpPathApi(
      {required super.snapshot,
      required super.identity,
      required this.request});
  String get _root => '/api/v1/datasets/${identity.datasetId}/http-path/v1';
  Future<bool> supported({CancelToken? cancelToken}) async {
    final response = await api.scopedTaskBatchRequest(
        snapshot, 'GET', '$_root/capability',
        queryParameters:
            request.expectedSHA256 == null ? null : {'integrity': 'sha256'},
        cancelToken: cancelToken);
    final body = _json(response.data);
    if (!snapshot.isCurrent ||
        response.statusCode != 200 ||
        body is! Map ||
        body['code'] != 0 ||
        body['data'] is! Map) return false;
    final data = body['data'] as Map;
    final methods = data['methods'];
    return data['scopeVersion'] is int &&
        data['scopeVersion'] == 1 &&
        data['datasetId'] == identity.datasetId &&
        data['kind'] == httpPathCreateKind &&
        data['requestVersion'] is int &&
        data['requestVersion'] == 1 &&
        data['supported'] == true &&
        _sameCaps(data['requiredCapabilities'], request.requiredCapabilities) &&
        methods is Map &&
        methods['create'] == 'POST' &&
        methods['query'] == 'GET' &&
        methods['cancel'] == 'POST' &&
        methods['preview'] == 'POST';
  }

  Future<HttpPathPreview> preview(TaskBatchOperationRef operation,
      {CancelToken? cancelToken}) async {
    final response = await api.scopedTaskBatchRequest(
        snapshot, 'POST', '$_root/preview',
        data: request.toJson(operation.operationId), cancelToken: cancelToken);
    final body = _json(response.data);
    if (!snapshot.isCurrent ||
        response.statusCode != 200 ||
        body is! Map ||
        body['code'] != 0 ||
        body['data'] is! Map) {
      throw const FormatException('path_preview_unavailable');
    }
    return HttpPathPreview.fromJson(Map<String, dynamic>.from(body['data']));
  }

  @override
  Future<ScopedTaskBatchReply> create(
      TaskBatchOperationRef operation, CreateTaskBatch batch) {
    if (!request.matchesBatch(batch))
      throw ArgumentError('path_request_changed');
    return _send(operation, TaskBatchAction.create, 'POST', '$_root/operations',
        data: request.toJson(operation.operationId));
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
  Future<ScopedTaskBatchReply> _send(TaskBatchOperationRef operation,
      TaskBatchAction action, String method, String path,
      {Map<String, dynamic>? data}) async {
    try {
      final response =
          await api.scopedTaskBatchRequest(snapshot, method, path, data: data);
      return HttpPathReply(
          datasetId: identity.datasetId,
          operation: operation,
          action: action,
          requiredCapabilities: request.requiredCapabilities,
          statusCode: response.statusCode,
          body: _json(response.data),
          transportFailure: snapshot.isCurrent ? null : 'route_changed');
    } on DioException catch (error) {
      return HttpPathReply(
          datasetId: identity.datasetId,
          operation: operation,
          action: action,
          requiredCapabilities: request.requiredCapabilities,
          transportFailure: error.type.name);
    }
  }
}

class HttpPathReply extends ScopedTaskBatchReply {
  final List<String> requiredCapabilities;
  HttpPathReply(
      {required super.datasetId,
      required super.operation,
      required super.action,
      required this.requiredCapabilities,
      super.statusCode,
      super.body,
      super.transportFailure});
  @override
  bool get scopeVerified {
    if (!super.scopeVerified) return false;
    final data = envelope.envelope!['data'] as Map;
    if (data['kind'] != httpPathCreateKind) return false;
    if (_sameCaps(data['requiredCapabilities'], requiredCapabilities))
      return true;
    // No R exists yet. This narrowly validates a read-only absence response;
    // it grants neither first-send permission nor acceptance of any intent.
    if (envelope.action != TaskBatchAction.query ||
        envelope.statusCode != 200 ||
        envelope.code != 0 ||
        !_sameCaps(data['requiredCapabilities'], const [])) return false;
    final receipt =
        TaskBatchReceipt.fromJson(Map<String, dynamic>.from(data['operation']));
    return receipt.hasV1Snapshot &&
        receipt.operationId == envelope.operation.operationId &&
        receipt.state == 'unknown' &&
        receipt.members.isEmpty &&
        receipt.taskIds.isEmpty &&
        receipt.createdCount == 0 &&
        receipt.failedCount == 0 &&
        receipt.pendingCount == 0;
  }
}

/// A display-only preview. This never becomes a caller-supplied F or permit.
class HttpPathPreview {
  final String root, name;
  const HttpPathPreview._(this.root, this.name);
  factory HttpPathPreview.fromJson(Map<String, dynamic> json) {
    final decision = json['decision'], layout = json['layout'];
    final name = json['name'], hash = json['layoutHash'];
    if (json['version'] is! int ||
        json['version'] != 1 ||
        decision is! Map ||
        decision['version'] is! int ||
        decision['version'] != 1 ||
        decision['root'] is! String ||
        (decision['root'] as String).isEmpty ||
        !const {
          'default',
          'user_selected',
          'manual_category',
          'api_explicit',
          'source_suggested',
          'unknown',
          'rule'
        }.contains(decision['origin']) ||
        decision['ruleId'] is! String ||
        (decision['ruleVersion'] != null &&
            decision['ruleVersion'] is! String) ||
        name is! String ||
        name.isEmpty ||
        name.contains(RegExp(r'[/\\\x00]')) ||
        hash is! String ||
        RegExp(r'^[0-9a-f]{64}$').stringMatch(hash) != hash ||
        layout is! Map ||
        layout['version'] is! int ||
        layout['version'] != 1 ||
        layout['parentRoot'] != decision['root'] ||
        layout['members'] is! List) {
      throw const FormatException('invalid_path_preview');
    }
    final members = layout['members'] as List;
    if (members.length != 1 ||
        members.single is! Map ||
        members.single['index'] is! int ||
        members.single['index'] != 0 ||
        members.single['selected'] != true ||
        members.single['relativePath'] != name) {
      throw const FormatException('invalid_path_preview_layout');
    }
    return HttpPathPreview._(decision['root'], name);
  }
}
