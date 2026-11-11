/// Response-only projection. Never serialize the private completion record or
/// persist this view with Task. A current dataset read must bind it for display.
class HttpCompletionProjection {
  static const phases = {
    'prepared',
    'waiting_verify',
    'verifying',
    'verified',
    'published_pending_commit',
    'committed'
  };
  static const errors = {
    'completion_unavailable',
    'completion_state_invalid',
    'integrity_mismatch',
    'publish_conflict',
    'cancelled',
    'run_changed',
    'io_error',
    'unsupported_option'
  };
  final String? datasetId, taskId, completionId, phase, errorCategory;
  final bool? recordedMatch;
  final bool _valid;
  bool Function()? _scopeCurrent;
  HttpCompletionProjection._(this.datasetId, this.taskId, this.completionId,
      this.phase, this.recordedMatch, this.errorCategory, this._valid);
  factory HttpCompletionProjection.fromJson(Object? raw,
      {required String taskId}) {
    if (raw is! Map) {
      return HttpCompletionProjection._(
          null, taskId, null, null, null, null, false);
    }
    final dataset = raw['datasetId'];
    final completion = raw['completionId'];
    final phase = raw['phase'];
    final error = raw['errorCategory'];
    final match = raw['recordedMatch'];
    final linked = raw['version'] is int &&
        raw['version'] == 1 &&
        raw['scopeVersion'] is int &&
        raw['scopeVersion'] == 1 &&
        dataset is String &&
        RegExp(r'^[0-9a-f]{32}$').stringMatch(dataset) == dataset &&
        raw['taskId'] == taskId &&
        taskId.isNotEmpty;
    final valid = linked &&
        raw['available'] == true &&
        completion is String &&
        completion.isNotEmpty &&
        phases.contains(phase) &&
        (error == null || errors.contains(error)) &&
        (const {'prepared', 'waiting_verify', 'verifying'}.contains(phase)
            ? match == null
            : match == true);
    final unavailable = linked &&
        raw['available'] == false &&
        completion == null &&
        phase == null &&
        match == null &&
        (error == null || errors.contains(error));
    return HttpCompletionProjection._(
        dataset is String ? dataset : null,
        taskId,
        valid ? completion : null,
        valid ? phase : null,
        valid ? match as bool? : null,
        (valid || unavailable) && errors.contains(error)
            ? error as String
            : null,
        valid);
  }
  bool get available => _valid && _scopeCurrent?.call() == true;
  String? get trustedErrorCategory =>
      _scopeCurrent?.call() == true ? errorCategory : null;
  void bindToDataset(String datasetId, {required bool Function() isCurrent}) {
    _scopeCurrent = this.datasetId == datasetId ? isCurrent : null;
  }
}
