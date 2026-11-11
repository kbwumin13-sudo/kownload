import '../../../../api/model/task.dart';
import '../../../../api/model/http_completion_projection.dart';

enum HttpCompletionPhase {
  unknown,
  prepared,
  waitingVerify,
  verifying,
  verified,
  publishedPendingCommit,
  committed,
}

/// Read-only display input; wire projections require current scope and Task
/// association. Private filesystem completion records are not a UI DTO.
class HttpCompletionView {
  static const capability = 'http-sha256-completion/v1';
  final HttpCompletionPhase phase;
  final String? errorCategory;

  const HttpCompletionView(this.phase, {this.errorCategory});

  factory HttpCompletionView.fromProjection({
    required Object? version,
    required Object? capability,
    required Object? phase,
  }) {
    if (version is! int ||
        version != 1 ||
        capability != HttpCompletionView.capability) {
      return const HttpCompletionView(HttpCompletionPhase.unknown);
    }
    return HttpCompletionView(switch (phase) {
      'prepared' => HttpCompletionPhase.prepared,
      'waiting_verify' => HttpCompletionPhase.waitingVerify,
      'verifying' => HttpCompletionPhase.verifying,
      'verified' => HttpCompletionPhase.verified,
      'published_pending_commit' => HttpCompletionPhase.publishedPendingCommit,
      'committed' => HttpCompletionPhase.committed,
      _ => HttpCompletionPhase.unknown,
    });
  }

  factory HttpCompletionView.fromTaskProjection(
      HttpCompletionProjection projection) {
    final phase = projection.available
        ? HttpCompletionView.fromProjection(
                version: 1, capability: capability, phase: projection.phase)
            .phase
        : HttpCompletionPhase.unknown;
    return HttpCompletionView(phase,
        errorCategory: projection.trustedErrorCategory);
  }

  /// All Task surfaces use this response-only, current-scope gate. A group
  /// additionally knows the dataset of its own receipt and must match it.
  static HttpCompletionView? forTask(Task task, {String? expectedDataset}) {
    final projection = task.completionView;
    if (task.protocol != Protocol.http || projection == null) return null;
    if (expectedDataset != null && projection.datasetId != expectedDataset) {
      return const HttpCompletionView(HttpCompletionPhase.unknown);
    }
    return HttpCompletionView.fromTaskProjection(projection);
  }

  bool isConfirmedDoneFor(Status status) =>
      status == Status.done &&
      phase == HttpCompletionPhase.committed &&
      errorCategory == null;

  String labelFor(Status status) {
    if (errorCategory != null) {
      if (status == Status.done) return '校验完成状态未确认';
      return switch (errorCategory) {
        'integrity_mismatch' => 'SHA-256 不匹配',
        'publish_conflict' => '目标文件已存在，未覆盖',
        'cancelled' => '校验任务已取消',
        'run_changed' => '校验运行状态已变化',
        'io_error' => '校验或保存文件失败',
        'unsupported_option' => '当前校验能力不支持此下载选项',
        _ => '校验完成状态未确认',
      };
    }
    if (phase == HttpCompletionPhase.unknown) return '校验完成状态未确认';
    if (status == Status.done && phase != HttpCompletionPhase.committed) {
      return '校验完成状态未确认';
    }
    if (phase != HttpCompletionPhase.committed) {
      if (status == Status.error) return '校验任务失败';
      if (status == Status.pause) return '校验任务已暂停';
      if (status != Status.running &&
          phase != HttpCompletionPhase.prepared &&
          phase != HttpCompletionPhase.waitingVerify) {
        return '校验完成状态未确认';
      }
    }
    return switch (phase) {
      HttpCompletionPhase.prepared => '尚未校验',
      HttpCompletionPhase.waitingVerify => '等待校验',
      HttpCompletionPhase.verifying => '正在校验 SHA-256',
      HttpCompletionPhase.verified => '校验通过，等待保存',
      HttpCompletionPhase.publishedPendingCommit => '正在保存完成状态',
      HttpCompletionPhase.committed =>
        status == Status.done ? '校验并保存完成' : '校验完成状态未确认',
      HttpCompletionPhase.unknown => '校验完成状态未确认',
    };
  }
}
