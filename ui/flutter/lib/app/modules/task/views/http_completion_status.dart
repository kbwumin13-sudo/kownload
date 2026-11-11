import '../../../../api/model/task.dart';

enum HttpCompletionPhase {
  unknown,
  prepared,
  waitingVerify,
  verifying,
  verified,
  publishedPendingCommit,
  committed,
}

/// Read-only view input. A future wire adapter must validate its own scope and
/// Task association; private filesystem completion records are not a UI DTO.
class HttpCompletionView {
  static const capability = 'http-sha256-completion/v1';
  final HttpCompletionPhase phase;

  const HttpCompletionView(this.phase);

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

  String labelFor(Status status) {
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
