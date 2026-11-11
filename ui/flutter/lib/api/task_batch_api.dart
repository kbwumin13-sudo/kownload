import 'dart:convert';

import 'package:dio/dio.dart';

import 'api.dart' as api;
import 'model/create_task_batch.dart';
import 'model/task_batch_operation.dart';
import 'model/task_batch_catalog.dart';
import 'model/task_batch_scope.dart';

typedef TaskBatchTransport = Future<Response<dynamic>>
    Function(String method, String path, {Map<String, dynamic>? data});

/// Dedicated S2 client. It never generates IDs, retries, falls back or persists.
class TaskBatchApi {
  static const _path = '/api/v1/task-batches/operations';
  final TaskBatchTransport _transport;
  TaskBatchApi({TaskBatchTransport? transport})
      : _transport = transport ?? api.taskBatchOperationRequest;

  Future<TaskBatchReply> create(
      TaskBatchOperationRef operation, CreateTaskBatch batch) {
    // Freeze this request's JSON before an async transport can observe edits.
    final data =
        Map<String, dynamic>.from(jsonDecode(jsonEncode(batch.toJson())) as Map)
          ..['version'] = 1
          ..['operationId'] = operation.operationId;
    return _send(operation, TaskBatchAction.create, 'POST', _path, data: data);
  }

  /// A query only reads the receipt; unknown/pending does not initiate creation.
  Future<TaskBatchReply> query(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.query,
      'GET',
      '$_path/${Uri.encodeComponent(operation.operationId)}');

  /// Settles pending creation members; does not cancel/pause any created Task.
  Future<TaskBatchReply> cancel(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.cancel,
      'POST',
      '$_path/${Uri.encodeComponent(operation.operationId)}/cancel');

  Future<TaskBatchReply> _send(TaskBatchOperationRef operation,
      TaskBatchAction action, String method, String path,
      {Map<String, dynamic>? data}) async {
    Response<dynamic> response;
    try {
      response = await _transport(method, path, data: data);
    } on DioException catch (error) {
      if (error.response == null) {
        return TaskBatchReply(
            operation: operation,
            action: action,
            transportFailure: error.type.name);
      }
      response = error.response!;
    }
    Object? body = response.data;
    if (body is String) {
      try {
        body = jsonDecode(body);
      } on FormatException {/* retain the raw reply */}
    }
    return TaskBatchReply(
        operation: operation,
        action: action,
        statusCode: response.statusCode,
        body: body);
  }
}

/// Bound to one active transport generation and one Core logical dataset.
class ScopedTaskBatchApi {
  final api.ActiveApiSnapshot snapshot;
  final DatasetIdentity identity;
  ScopedTaskBatchApi({required this.snapshot, required this.identity}) {
    if (!identity.supported ||
        !identity.persistent ||
        snapshot.taskBatchRoute?.supported != true) {
      throw ArgumentError('unsupported_batch_scope');
    }
  }
  String get _path =>
      '/api/v1/datasets/${identity.datasetId}/task-batches/v1/operations';
  static Future<DatasetIdentity> readIdentity(
      api.ActiveApiSnapshot snapshot) async {
    final response = await api.scopedTaskBatchRequest(
        snapshot, 'GET', '/api/v1/dataset/identity');
    final body = _json(response.data);
    if (!snapshot.isCurrent ||
        response.statusCode != 200 ||
        body is! Map ||
        body['code'] != 0 ||
        body['data'] is! Map) {
      throw const TaskBatchCatalogException('identity_unavailable');
    }
    final identity =
        DatasetIdentity.fromJson(Map<String, dynamic>.from(body['data']));
    if (!identity.supported || !identity.persistent) {
      throw const TaskBatchCatalogException(
          'nonpersistent_or_unsupported_identity');
    }
    return identity;
  }

  Future<bool> stillSameDataset() async {
    final current = await readIdentity(snapshot);
    return current.datasetId == identity.datasetId;
  }

  Future<TaskBatchCatalogPage> catalog(
      {String filter = 'all', int limit = 50, String? cursor}) async {
    final response = await api.scopedTaskBatchRequest(
        snapshot, 'GET', '/api/v1/task-batches/operations', queryParameters: {
      'filter': filter,
      'limit': limit,
      if (cursor != null) 'cursor': cursor
    });
    final body = _json(response.data);
    if (!snapshot.isCurrent ||
        response.statusCode != 200 ||
        body is! Map ||
        body['code'] != 0 ||
        body['data'] is! Map) {
      throw const TaskBatchCatalogException('catalog_unavailable');
    }
    final page =
        TaskBatchCatalogPage.fromJson(Map<String, dynamic>.from(body['data']));
    if (page.identity.datasetId != identity.datasetId ||
        !page.identity.persistent ||
        page.filter != filter) {
      throw const TaskBatchCatalogException('catalog_scope_changed');
    }
    return page;
  }

  Future<ScopedTaskBatchReply> create(
      TaskBatchOperationRef operation, CreateTaskBatch batch) {
    final data =
        Map<String, dynamic>.from(jsonDecode(jsonEncode(batch.toJson())))
          ..['version'] = 1
          ..['operationId'] = operation.operationId;
    return _send(operation, TaskBatchAction.create, 'POST', _path, data: data);
  }

  Future<ScopedTaskBatchReply> query(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.query,
      'GET',
      '$_path/${Uri.encodeComponent(operation.operationId)}');
  Future<ScopedTaskBatchReply> cancel(TaskBatchOperationRef operation) => _send(
      operation,
      TaskBatchAction.cancel,
      'POST',
      '$_path/${Uri.encodeComponent(operation.operationId)}/cancel');
  Future<ScopedTaskBatchReply> _send(TaskBatchOperationRef operation,
      TaskBatchAction action, String method, String path,
      {Map<String, dynamic>? data}) async {
    try {
      final response =
          await api.scopedTaskBatchRequest(snapshot, method, path, data: data);
      return ScopedTaskBatchReply(
          datasetId: identity.datasetId,
          operation: operation,
          action: action,
          statusCode: response.statusCode,
          body: _json(response.data));
    } on DioException catch (error) {
      return ScopedTaskBatchReply(
          datasetId: identity.datasetId,
          operation: operation,
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
