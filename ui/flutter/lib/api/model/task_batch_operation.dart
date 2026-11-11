/// Local reference to Core's fixed scope; namespace is not a wire selector.
class TaskBatchOperationRef {
  static const namespaceV1 = 'task-batch.create/v1';
  static final _idPattern = RegExp(r'^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$');
  final String namespace;
  final String operationId;

  TaskBatchOperationRef({required this.namespace, required this.operationId}) {
    if (namespace != namespaceV1 ||
        _idPattern.stringMatch(operationId) != operationId) {
      throw ArgumentError('invalid_task_batch_reference');
    }
  }
}

enum TaskBatchAction { create, query, cancel }

Object? _freeze(Object? value) {
  if (value is Map) {
    return Map.unmodifiable(
        value.map((key, value) => MapEntry(key, _freeze(value))));
  }
  if (value is List) return List.unmodifiable(value.map(_freeze));
  return value;
}

Map<String, dynamic>? _object(Object? value) =>
    value is Map && value.keys.every((key) => key is String)
        ? Map<String, dynamic>.unmodifiable(Map<String, dynamic>.from(value))
        : null;

class TaskBatchMemberReceipt {
  final Map<String, dynamic> raw;
  TaskBatchMemberReceipt.fromJson(Map<String, dynamic> value)
      : raw = _object(_freeze(value))!;
  int? get index => raw['index'] is int ? raw['index'] : null;
  String? get taskId => raw['taskId'] is String ? raw['taskId'] : null;
  String? get state => raw['state'] is String ? raw['state'] : null;
  String? get error => raw['error'] is String ? raw['error'] : null;
  bool? get taskPresent =>
      raw['taskPresent'] is bool ? raw['taskPresent'] : null;

  bool get _valid =>
      index != null &&
      index! >= 0 &&
      taskId != null &&
      taskId!.isNotEmpty &&
      const ['pending', 'created', 'failed'].contains(state) &&
      (raw['error'] == null || raw['error'] is String) &&
      (raw['taskPresent'] == null || raw['taskPresent'] is bool);
}

/// Creation receipts, not Task download status. Unknown fields stay available.
class TaskBatchReceipt {
  final Map<String, dynamic> raw;
  TaskBatchReceipt.fromJson(Map<String, dynamic> value)
      : raw = _object(_freeze(value))!;
  int? get version => raw['version'] is int ? raw['version'] : null;
  String? get operationId =>
      raw['operationId'] is String ? raw['operationId'] : null;
  String? get state => raw['state'] is String ? raw['state'] : null;
  bool get queryRequired => raw['queryRequired'] == true;
  int? get createdCount =>
      raw['createdCount'] is int ? raw['createdCount'] : null;
  int? get failedCount => raw['failedCount'] is int ? raw['failedCount'] : null;
  int? get pendingCount =>
      raw['pendingCount'] is int ? raw['pendingCount'] : null;
  late final List<TaskBatchMemberReceipt> members = List.unmodifiable(
      (raw['members'] is List ? raw['members'] as List : const [])
          .map(_object)
          .whereType<Map<String, dynamic>>()
          .map(TaskBatchMemberReceipt.fromJson));
  late final List<String> taskIds = List.unmodifiable(
      (raw['taskIds'] is List ? raw['taskIds'] as List : const [])
          .whereType<String>());
  late final bool hasV1Snapshot = _validate();

  bool _validate() {
    if (version != 1 ||
        !const ['pending', 'settled', 'unknown'].contains(state) ||
        raw['members'] is! List ||
        raw['taskIds'] is! List ||
        (raw['members'] as List).length != members.length ||
        (raw['taskIds'] as List).length != taskIds.length ||
        members.any((member) => !member._valid)) {
      return false;
    }
    final created =
        members.where((member) => member.state == 'created').toList();
    final failed = members.where((member) => member.state == 'failed').length;
    final pending = members.where((member) => member.state == 'pending').length;
    if (createdCount != created.length ||
        failedCount != failed ||
        pendingCount != pending ||
        taskIds.length != created.length ||
        members.map((member) => member.taskId).toSet().length !=
            members.length) {
      return false;
    }
    for (var i = 0; i < members.length; i++) {
      if (members[i].index != i) return false;
    }
    for (var i = 0; i < created.length; i++) {
      if (created[i].taskId != taskIds[i]) return false;
    }
    if (state == 'unknown') return members.isEmpty;
    return members.isNotEmpty && state == (pending > 0 ? 'pending' : 'settled');
  }
}

/// Keeps the HTTP/business envelope even when Core returns partial failure.
/// A lost acknowledgement never grants permission to create a new operation.
class TaskBatchReply {
  final TaskBatchOperationRef operation;
  final TaskBatchAction action;
  final int? statusCode;
  final Object? rawBody;
  final String? transportFailure;
  late final Map<String, dynamic>? envelope = _object(rawBody);
  late final TaskBatchReceipt? receipt = _object(envelope?['data']) == null
      ? null
      : TaskBatchReceipt.fromJson(_object(envelope?['data'])!);
  int? get code => envelope?['code'] is int ? envelope!['code'] : null;
  String? get message => envelope?['msg'] is String ? envelope!['msg'] : null;

  TaskBatchReply(
      {required this.operation,
      required this.action,
      this.statusCode,
      Object? body,
      this.transportFailure})
      : rawBody = _freeze(body);

  bool get hasMatchingSnapshot =>
      transportFailure == null &&
      statusCode != null &&
      statusCode! >= 200 &&
      statusCode! < 600 &&
      !(statusCode! >= 300 && statusCode! < 400) &&
      code != null &&
      receipt?.operationId == operation.operationId &&
      receipt!.hasV1Snapshot;

  /// Historical created IDs only; a pending reserved ID is never included.
  /// taskPresent=false still describes a created Task that has since been deleted.
  /// A conflict can carry the prior operation: these IDs do not acknowledge the
  /// conflicting payload. Inspect statusCode/code/message as well.
  List<String> get createdTaskIds =>
      hasMatchingSnapshot ? receipt!.taskIds : const [];
  bool get requiresQuery =>
      !hasMatchingSnapshot ||
      receipt!.queryRequired ||
      receipt!.state != 'settled';
}
