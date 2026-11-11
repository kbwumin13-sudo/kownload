import 'dart:convert';

import 'package:dio/dio.dart';

import 'api.dart' as api;
import 'model/create_task_batch.dart';
import 'model/task_batch_operation.dart';

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
