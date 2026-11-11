import 'task_batch_operation.dart';

/// Preserves the outer HTTP/business envelope; only a matching, versioned
/// dataset wrapper can authorize reading the nested operation's historical IDs.
class ScopedTaskBatchReply {
  final String datasetId;
  final TaskBatchReply envelope;
  ScopedTaskBatchReply(
      {required this.datasetId,
      required TaskBatchOperationRef operation,
      required TaskBatchAction action,
      int? statusCode,
      Object? body,
      String? transportFailure})
      : envelope = TaskBatchReply(
            operation: operation,
            action: action,
            statusCode: statusCode,
            body: body,
            transportFailure: transportFailure);
  Map? get _scope =>
      envelope.envelope?['data'] is Map ? envelope.envelope!['data'] : null;
  bool get scopeVerified =>
      envelope.transportFailure == null &&
      const [200, 409, 503].contains(envelope.statusCode) &&
      envelope.message != 'dataset_mismatch' &&
      RegExp(r'^[0-9a-f]{32}$').stringMatch(datasetId) == datasetId &&
      _scope?['scopeVersion'] is int &&
      _scope?['scopeVersion'] == 1 &&
      _scope?['datasetId'] == datasetId &&
      _scope?['namespace'] == TaskBatchOperationRef.namespaceV1 &&
      _scope?['operation'] is Map;
  late final TaskBatchReply? operation = scopeVerified
      ? TaskBatchReply(
          operation: envelope.operation,
          action: envelope.action,
          statusCode: envelope.statusCode,
          body: {
              'code': envelope.code,
              'msg': envelope.message,
              'data': _scope!['operation']
            })
      : null;
  bool get hasMatchingSnapshot => operation?.hasMatchingSnapshot == true;
  bool get requiresQuery => !scopeVerified || operation!.requiresQuery;
  List<String> get createdTaskIds => operation?.createdTaskIds ?? const [];
  TaskBatchReceipt? get receipt =>
      hasMatchingSnapshot ? operation!.receipt : null;
}
