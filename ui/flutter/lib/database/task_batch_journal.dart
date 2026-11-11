import 'dart:convert';

import '../api/model/task_batch_catalog.dart';
import '../api/model/task_batch_operation.dart';

class TaskBatchJournalException implements Exception {
  final String reason;
  const TaskBatchJournalException(this.reason);
  @override
  String toString() => 'TaskBatchJournalException($reason)';
}

void _keys(Map value, Set<String> expected) {
  if (value.length != expected.length || !value.keys.every(expected.contains)) {
    throw const TaskBatchJournalException('unknown_journal_schema');
  }
}

/// Existing App route reference, not authentication or a database identity.
class JournalRouteRef {
  final String product, routeScope;
  final int identityProtocol;
  const JournalRouteRef(
      {required this.product,
      required this.identityProtocol,
      required this.routeScope});
  bool get supported =>
      product == 'kownload' &&
      identityProtocol == 1 &&
      routeScope.length == 64 &&
      RegExp(r'^[0-9a-f]+$').hasMatch(routeScope);
  factory JournalRouteRef.fromJson(Map json) {
    _keys(json, {'product', 'identityProtocol', 'routeScope'});
    if (json['product'] is! String ||
        json['identityProtocol'] is! int ||
        json['routeScope'] is! String) {
      throw const TaskBatchJournalException('unknown_route_reference');
    }
    final route = JournalRouteRef(
        product: json['product'],
        identityProtocol: json['identityProtocol'],
        routeScope: json['routeScope']);
    if (!route.supported) {
      throw const TaskBatchJournalException('unknown_route_reference');
    }
    return route;
  }
  Map<String, dynamic> toJson() => {
        'product': product,
        'identityProtocol': identityProtocol,
        'routeScope': routeScope
      };
  bool sameRoute(JournalRouteRef other) =>
      product == other.product &&
      identityProtocol == other.identityProtocol &&
      routeScope == other.routeScope;
}

/// Supplied from the bound App route and live Core identity. Shape checks here
/// do not authenticate either source or prove a copied dataset has not rolled back.
class TaskBatchJournalScope {
  final JournalRouteRef route;
  final DatasetIdentity dataset;
  const TaskBatchJournalScope({required this.route, required this.dataset});
  bool get usable => route.supported && dataset.supported && dataset.persistent;
}

class TaskBatchJournalRef {
  final JournalRouteRef route;
  final String datasetId, namespace, operationId;
  const TaskBatchJournalRef._(
      this.route, this.datasetId, this.namespace, this.operationId);
  factory TaskBatchJournalRef.forQuery(
      TaskBatchJournalScope scope, TaskBatchOperationRef operation) {
    if (!scope.usable) {
      throw const TaskBatchJournalException('untrusted_or_nonpersistent_scope');
    }
    return TaskBatchJournalRef._(scope.route, scope.dataset.datasetId,
        operation.namespace, operation.operationId);
  }
  factory TaskBatchJournalRef.fromJson(Map json) {
    _keys(json, {'route', 'datasetId', 'namespace', 'operationId', 'state'});
    if (json['route'] is! Map ||
        json['datasetId'] is! String ||
        json['namespace'] is! String ||
        json['operationId'] is! String ||
        json['state'] != 'query_required') {
      throw const TaskBatchJournalException('unknown_journal_reference');
    }
    final route = JournalRouteRef.fromJson(json['route']);
    final dataset = DatasetIdentity(
        version: 1, datasetId: json['datasetId'], persistent: true);
    if (!dataset.supported) {
      throw const TaskBatchJournalException('unknown_journal_dataset');
    }
    try {
      final operation = TaskBatchOperationRef(
          namespace: json['namespace'], operationId: json['operationId']);
      return TaskBatchJournalRef._(
          route, dataset.datasetId, operation.namespace, operation.operationId);
    } catch (_) {
      throw const TaskBatchJournalException('unknown_journal_operation');
    }
  }
  String get state => 'query_required';
  Map<String, dynamic> toJson() => {
        'route': route.toJson(),
        'datasetId': datasetId,
        'namespace': namespace,
        'operationId': operationId,
        'state': state
      };
  String get key => jsonEncode([
        route.product,
        route.identityProtocol,
        route.routeScope,
        datasetId,
        namespace,
        operationId
      ]);
  bool matches(TaskBatchJournalScope scope) =>
      scope.usable &&
      route.sameRoute(scope.route) &&
      datasetId == scope.dataset.datasetId;
}

class TaskBatchJournal {
  final List<TaskBatchJournalRef> refs;
  TaskBatchJournal(Iterable<TaskBatchJournalRef> refs)
      : refs = List.unmodifiable(refs) {
    if (this.refs.map((ref) => ref.key).toSet().length != this.refs.length) {
      throw const TaskBatchJournalException('duplicate_journal_reference');
    }
  }
  factory TaskBatchJournal.decode(Object? raw) {
    if (raw == null) return TaskBatchJournal(const []);
    if (raw is! String) {
      throw const TaskBatchJournalException('unknown_journal_storage');
    }
    try {
      final json = jsonDecode(raw);
      if (json is! Map) {
        throw const TaskBatchJournalException('unknown_journal_schema');
      }
      _keys(json, {'journalVersion', 'refs'});
      if (json['journalVersion'] != 1 ||
          json['journalVersion'] is! int ||
          json['refs'] is! List) {
        throw const TaskBatchJournalException('unknown_journal_schema');
      }
      final refs = <TaskBatchJournalRef>[];
      for (final row in json['refs']) {
        if (row is! Map) {
          throw const TaskBatchJournalException('unknown_journal_reference');
        }
        refs.add(TaskBatchJournalRef.fromJson(row));
      }
      return TaskBatchJournal(refs);
    } on FormatException {
      throw const TaskBatchJournalException('invalid_journal_json');
    }
  }
  String encode() => jsonEncode(
      {'journalVersion': 1, 'refs': refs.map((ref) => ref.toJson()).toList()});
  List<TaskBatchJournalRef> queryable(TaskBatchJournalScope scope) =>
      List.unmodifiable(refs.where((ref) => ref.matches(scope)));
  List<TaskBatchJournalRef> isolated(TaskBatchJournalScope scope) =>
      List.unmodifiable(refs.where((ref) => !ref.matches(scope)));
}
