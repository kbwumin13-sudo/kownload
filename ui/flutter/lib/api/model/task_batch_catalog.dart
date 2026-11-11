import 'task_batch_operation.dart';

bool _hex(String value, int length) =>
    value.length == length && RegExp(r'^[0-9a-f]+$').hasMatch(value);

class TaskBatchCatalogException implements Exception {
  final String reason;
  const TaskBatchCatalogException(this.reason);
  @override
  String toString() => 'TaskBatchCatalogException($reason)';
}

class DatasetIdentity {
  final int version;
  final String datasetId;
  final bool persistent;
  const DatasetIdentity(
      {required this.version,
      required this.datasetId,
      required this.persistent});
  bool get supported => version == 1 && _hex(datasetId, 32);
  factory DatasetIdentity.fromJson(Map<String, dynamic> json) {
    if (json['version'] is! int ||
        json['datasetId'] is! String ||
        json['persistent'] is! bool) {
      throw const TaskBatchCatalogException('invalid_dataset_identity');
    }
    return DatasetIdentity(
        version: json['version'],
        datasetId: json['datasetId'],
        persistent: json['persistent']);
  }
}

class TaskBatchCatalogCounts {
  final int members, created, failed, pending;
  const TaskBatchCatalogCounts(
      this.members, this.created, this.failed, this.pending);
  factory TaskBatchCatalogCounts.fromJson(Map json) {
    final values = ['members', 'created', 'failed', 'pending']
        .map((key) => json[key])
        .toList();
    if (values.any((value) => value is! int || value < 0) ||
        values[0] < 1 ||
        values[0] != values[1] + values[2] + values[3]) {
      throw const TaskBatchCatalogException('invalid_catalog_counts');
    }
    return TaskBatchCatalogCounts(values[0], values[1], values[2], values[3]);
  }
}

class TaskBatchCatalogItem {
  final String keyHash, state;
  final String? operationId, requestHash, error;
  final TaskBatchCatalogCounts? counts;
  const TaskBatchCatalogItem._(this.keyHash, this.state, this.operationId,
      this.requestHash, this.error, this.counts);
  factory TaskBatchCatalogItem.fromJson(Map json) {
    final keyHash = json['keyHash'], state = json['state'];
    final op = json['operationId'],
        hash = json['requestHash'],
        error = json['error'];
    if (keyHash is! String ||
        !_hex(keyHash, 64) ||
        !const ['pending', 'settled', 'unknown'].contains(state) ||
        (op != null && op is! String) ||
        (hash != null && (hash is! String || !_hex(hash, 64))) ||
        (error != null && error is! String)) {
      throw const TaskBatchCatalogException('invalid_catalog_item');
    }
    if (state == 'unknown') {
      if (json['counts'] != null || error == null || error.isEmpty) {
        throw const TaskBatchCatalogException('invalid_unknown_item');
      }
      return TaskBatchCatalogItem._(keyHash, state, op, hash, error, null);
    }
    try {
      TaskBatchOperationRef(
          namespace: TaskBatchOperationRef.namespaceV1,
          operationId: op as String);
    } catch (_) {
      throw const TaskBatchCatalogException('invalid_catalog_operation');
    }
    if (json['counts'] is! Map) {
      throw const TaskBatchCatalogException('missing_catalog_counts');
    }
    final counts = TaskBatchCatalogCounts.fromJson(json['counts']);
    if ((state == 'pending') != (counts.pending > 0)) {
      throw const TaskBatchCatalogException('inconsistent_catalog_state');
    }
    return TaskBatchCatalogItem._(keyHash, state, op, hash, error, counts);
  }
}

class TaskBatchCatalogPage {
  final DatasetIdentity identity;
  final String namespace, consistency, filter;
  final List<TaskBatchCatalogItem> items;
  final String? nextCursor;
  final bool hasMore;
  final int scanned;
  const TaskBatchCatalogPage._(this.identity, this.namespace, this.consistency,
      this.filter, this.items, this.nextCursor, this.hasMore, this.scanned);
  factory TaskBatchCatalogPage.fromJson(Map<String, dynamic> json) {
    final identity = DatasetIdentity.fromJson(json);
    if (!identity.supported ||
        json['namespace'] != TaskBatchOperationRef.namespaceV1 ||
        json['consistency'] != 'live-keyset' ||
        !const ['all', 'pending'].contains(json['filter']) ||
        json['items'] is! List ||
        (json['items'] as List).length > 100 ||
        json['hasMore'] is! bool ||
        json['scanned'] is! int ||
        json['scanned'] < 0 ||
        json['scanned'] > 1000 ||
        (json['nextCursor'] != null &&
            (json['nextCursor'] is! String ||
                (json['nextCursor'] as String).isEmpty))) {
      throw const TaskBatchCatalogException('unsupported_catalog_page');
    }
    final items = <TaskBatchCatalogItem>[];
    for (final value in json['items']) {
      if (value is! Map) {
        throw const TaskBatchCatalogException('invalid_catalog_item');
      }
      items.add(TaskBatchCatalogItem.fromJson(value));
    }
    if (items.length > json['scanned'] ||
        (json['hasMore'] == true && json['nextCursor'] == null)) {
      throw const TaskBatchCatalogException('invalid_catalog_cursor');
    }
    return TaskBatchCatalogPage._(
        identity,
        json['namespace'],
        json['consistency'],
        json['filter'],
        List.unmodifiable(items),
        json['nextCursor'],
        json['hasMore'],
        json['scanned']);
  }
}

/// Pure live-page accumulator. It never sends a request or mutates a journal.
class TaskBatchCatalogScan {
  final DatasetIdentity identity;
  final String filter;
  final _items = <TaskBatchCatalogItem>[];
  final _cursors = <String>{};
  String? _nextCursor, _error;
  bool _reachedLiveEnd = false;
  String? get nextCursor => _nextCursor;
  String? get error => _error;
  // This is the end of the current live traversal, never a snapshot proof.
  bool get reachedLiveEnd => _reachedLiveEnd;
  TaskBatchCatalogScan(
      {required this.identity, required this.filter, String? initialCursor}) {
    if (initialCursor != null) {
      if (initialCursor.isEmpty) {
        throw const TaskBatchCatalogException('invalid_catalog_cursor');
      }
      _nextCursor = initialCursor;
      _cursors.add(initialCursor);
    }
    if (!identity.supported || !const ['all', 'pending'].contains(filter)) {
      throw const TaskBatchCatalogException('unsupported_catalog_scan');
    }
  }
  List<TaskBatchCatalogItem> get items => List.unmodifiable(_items);
  bool accept(Map<String, dynamic> json) {
    if (error != null || reachedLiveEnd) return false;
    try {
      final page = TaskBatchCatalogPage.fromJson(json);
      if (page.identity.datasetId != identity.datasetId ||
          page.identity.persistent != identity.persistent ||
          page.filter != filter) {
        throw const TaskBatchCatalogException('catalog_scope_changed');
      }
      if (page.hasMore && !_cursors.add(page.nextCursor!)) {
        throw const TaskBatchCatalogException('catalog_cursor_not_advancing');
      }
      _items.addAll(page.items);
      _nextCursor = page.nextCursor;
      _reachedLiveEnd = !page.hasMore;
      return true;
    } on TaskBatchCatalogException catch (failure) {
      _error = failure.reason;
      return false;
    }
  }
}
