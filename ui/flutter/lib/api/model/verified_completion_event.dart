import 'dart:convert';

/// Permission received only from the authenticated, live accepted-done stream.
/// It is neither persisted nor reconstructed from a Task's historical status.
class VerifiedCompletionEvent {
  final String datasetId, taskId, completionId;
  const VerifiedCompletionEvent._(
      this.datasetId, this.taskId, this.completionId);
  factory VerifiedCompletionEvent.fromJson(Object? raw) {
    const keys = {'version', 'datasetId', 'taskId', 'completionId'};
    if (raw is! Map ||
        raw.length != 4 ||
        !raw.keys.every(keys.contains) ||
        raw['version'] is! int ||
        raw['version'] != 1 ||
        raw['datasetId'] is! String ||
        RegExp(r'^[0-9a-f]{32}$').stringMatch(raw['datasetId']) !=
            raw['datasetId'] ||
        raw['taskId'] is! String ||
        !_validId(raw['taskId']) ||
        raw['completionId'] is! String ||
        !_validId(raw['completionId'])) {
      throw const FormatException('invalid_completion_event');
    }
    return VerifiedCompletionEvent._(
        raw['datasetId'], raw['taskId'], raw['completionId']);
  }
  static bool _validId(String value) {
    final bytes = utf8.encode(value);
    return bytes.isNotEmpty &&
        bytes.length <= 128 &&
        utf8.decode(bytes) == value;
  }
}
