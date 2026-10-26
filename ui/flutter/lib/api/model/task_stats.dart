/// Download diagnostics from `/api/v1/tasks/{id}/stats`.
///
/// Cores before 1.9.3-kownload.5 only send `connections`, and netdisk tasks
/// nest their HTTP transfer under `http`, so every field is optional and
/// parsing never throws on missing or unexpected values.
class TaskStats {
  final String? phase;
  final String? rangeState;
  final String? rangeReason;
  final int? activeStreams;
  final int? configuredStreams;
  final int? targetStreams;
  final int streamCount;
  final String? protocol;
  final int? newConnections;
  final int? reusedConnections;
  final int? requests;
  final int? rateLimits;
  final DateTime? cooldownUntil;
  final String? lastError;
  final Map<String, int> timings;
  final String? adaptivePhase;
  final int? adaptiveTarget;

  /// Netdisk tasks: the provider phase text and the latest link resolution time.
  final String? netdiskPhase;
  final int? linkMs;

  const TaskStats({
    this.phase,
    this.rangeState,
    this.rangeReason,
    this.activeStreams,
    this.configuredStreams,
    this.targetStreams,
    this.streamCount = 0,
    this.protocol,
    this.newConnections,
    this.reusedConnections,
    this.requests,
    this.rateLimits,
    this.cooldownUntil,
    this.lastError,
    this.timings = const {},
    this.adaptivePhase,
    this.adaptiveTarget,
    this.netdiskPhase,
    this.linkMs,
  });

  static String? _string(dynamic v) => v is String && v.isNotEmpty ? v : null;

  static int? _int(dynamic v) => v is num ? v.toInt() : null;

  factory TaskStats.fromJson(dynamic json) {
    if (json is! Map) return const TaskStats();
    final nested = json['http'];
    if (nested is Map || json.containsKey('transfers')) {
      final http = TaskStats.fromJson(nested);
      final queue = _int(json['queueMs']);
      return http._withNetdisk(_string(json['phase']), _int(json['linkMs']), queue);
    }
    final connections = json['connections'];
    final timings = <String, int>{};
    final rawTimings = json['timings'];
    if (rawTimings is Map) {
      rawTimings.forEach((key, value) {
        if (key is String && value is num) timings[key] = value.toInt();
      });
    }
    final adaptive = json['adaptive'];
    final cooldown = _string(json['cooldownUntil']);
    return TaskStats(
      phase: _string(json['phase']),
      rangeState: _string(json['rangeState']),
      rangeReason: _string(json['rangeReason']),
      activeStreams: _int(json['activeStreams']),
      configuredStreams: _int(json['configuredStreams']),
      targetStreams: _int(json['targetStreams']),
      streamCount: connections is List ? connections.length : 0,
      protocol: _string(json['protocol']),
      newConnections: _int(json['newConnections']),
      reusedConnections: _int(json['reusedConnections']),
      requests: _int(json['requests']),
      rateLimits: _int(json['rateLimits']),
      cooldownUntil: cooldown == null ? null : DateTime.tryParse(cooldown),
      lastError: _string(json['lastError']),
      timings: timings,
      adaptivePhase: adaptive is Map ? _string(adaptive['phase']) : null,
      adaptiveTarget: adaptive is Map ? _int(adaptive['target']) : null,
    );
  }

  TaskStats _withNetdisk(String? phaseText, int? link, int? queueMs) {
    final merged = Map<String, int>.from(timings);
    if (queueMs != null) merged['queueMs'] = queueMs;
    return TaskStats(
      phase: phase,
      rangeState: rangeState,
      rangeReason: rangeReason,
      activeStreams: activeStreams,
      configuredStreams: configuredStreams,
      targetStreams: targetStreams,
      streamCount: streamCount,
      protocol: protocol,
      newConnections: newConnections,
      reusedConnections: reusedConnections,
      requests: requests,
      rateLimits: rateLimits,
      cooldownUntil: cooldownUntil,
      lastError: lastError,
      timings: merged,
      adaptivePhase: adaptivePhase,
      adaptiveTarget: adaptiveTarget,
      netdiskPhase: phaseText,
      linkMs: link,
    );
  }

  /// Streams running now; older cores only report the connections list.
  int? get runningStreams => activeStreams ?? (streamCount > 0 ? streamCount : null);

  /// The task's stream limit, or what the source currently grants it.
  int? get streamLimit => targetStreams ?? configuredStreams;

  bool get coolingDown => phase == 'cooldown' || phase == 'deferred';

  static const _phases = {
    'resolving': '解析中',
    'waiting': '等待开始',
    'connecting': '连接中',
    'transferring': '传输中',
    'cooldown': '源站限流，等待中',
    'deferred': '源站限流，稍后继续',
    'paused': '已暂停',
    'done': '已完成',
    'error': '出错',
  };

  static const _ranges = {
    'declared': '支持分段',
    'probed': '支持分段（探测确认）',
    'resumed': '支持分段（续传校验通过）',
    'unsupported': '不支持分段',
    'probeFailed': '分段探测失败',
    'notProbed': '未探测分段',
  };

  static const _reasons = {
    'singleStream': '连接数设为 1',
    'small': '文件小于 1 MiB',
    'unknownSize': '文件大小未知',
    'method': '非 GET 请求',
    'requestBody': '请求带有正文',
    'presetRange': '请求已指定范围',
    'status': '服务器返回异常状态',
    'declaredNone': '服务器声明不支持',
    'contentRange': '服务器返回的范围无效',
    'compressed': '响应经过压缩',
    'sourceBusy': '源站正在限流',
    'ignored': '服务器忽略了分段请求',
    'redirect': '跳转地址发生变化',
    'representation': '文件版本不一致',
    'body': '探测响应长度异常',
    'network': '网络错误',
    'timeout': '探测超时',
  };

  String? get phaseLabel => phase == null ? null : (_phases[phase] ?? phase);

  String? get rangeLabel {
    if (rangeState == null) return null;
    final label = _ranges[rangeState] ?? rangeState!;
    final reason = rangeReason == null ? null : (_reasons[rangeReason] ?? rangeReason);
    return reason == null ? label : '$label：$reason';
  }
}
