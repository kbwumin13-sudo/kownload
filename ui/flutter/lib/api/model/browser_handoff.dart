/// Browser handoff metadata contains identity and phase only, never credentials.
class BrowserDownloadIdentity {
  final int id;
  final String startTime, urlHash;
  const BrowserDownloadIdentity(this.id, this.startTime, this.urlHash);
  factory BrowserDownloadIdentity.fromJson(Map value) {
    if (value['id'] is! int ||
        value['id'] < 0 ||
        value['startTime'] is! String ||
        DateTime.tryParse(value['startTime']) == null ||
        value['urlHash'] is! String ||
        !RegExp(r'^[a-f0-9]{64}$').hasMatch(value['urlHash'])) {
      throw const FormatException('invalid_handoff_download');
    }
    return BrowserDownloadIdentity(
        value['id'], value['startTime'], value['urlHash']);
  }
  Map<String, dynamic> toJson() =>
      {'id': id, 'startTime': startTime, 'urlHash': urlHash};
  bool same(BrowserDownloadIdentity other) =>
      id == other.id &&
      startTime == other.startTime &&
      urlHash == other.urlHash;
}

class BrowserHandoffContext {
  final String clientId, browser;
  final BrowserDownloadIdentity download;
  const BrowserHandoffContext(this.clientId, this.browser, this.download);
  factory BrowserHandoffContext.fromJson(Map value) {
    if (value['version'] != 1 ||
        !validHandoffId(value['clientId']) ||
        !const ['chrome', 'edge'].contains(value['browser']) ||
        value['download'] is! Map) {
      throw const FormatException('invalid_handoff_context');
    }
    return BrowserHandoffContext(value['clientId'], value['browser'],
        BrowserDownloadIdentity.fromJson(value['download']));
  }
  Map<String, dynamic> toJson() => {
        'version': 1,
        'clientId': clientId,
        'browser': browser,
        'download': download.toJson()
      };
  bool same(BrowserHandoffContext other) =>
      clientId == other.clientId &&
      browser == other.browser &&
      download.same(other.download);
}

bool validHandoffId(dynamic value) =>
    value is String &&
    RegExp(r'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$').hasMatch(value);

class BrowserHandoffResult {
  final String operationId, state;
  final List<String> taskIds;
  final String? reason, taskStatus;
  final BrowserDownloadIdentity? download;
  const BrowserHandoffResult(this.operationId, this.state, this.taskIds,
      {this.reason, this.taskStatus, this.download});
  factory BrowserHandoffResult.fromJson(Map value,
      {String? operationId, bool allowLocalAbort = false}) {
    final state = value['state'], ids = value['taskIds'];
    if (value['version'] != 1 ||
        !validHandoffId(value['operationId']) ||
        operationId != null && value['operationId'] != operationId ||
        !const ['prepared', 'committed', 'aborted', 'unknown']
            .contains(state) ||
        ids is! List ||
        ids.any((id) => id is! String || id.isEmpty) ||
        ids.length > 1 ||
        const ['prepared', 'committed'].contains(state) && ids.length != 1 ||
        state == 'unknown' && ids.isNotEmpty ||
        state == 'aborted' && ids.isEmpty && !allowLocalAbort ||
        value['taskId'] != null &&
            (ids.length != 1 || value['taskId'] != ids.single)) {
      throw const FormatException('invalid_handoff_result');
    }
    return BrowserHandoffResult(
        value['operationId'], state, List<String>.unmodifiable(ids),
        reason: value['reason'] is String ? value['reason'] : null,
        taskStatus: value['taskStatus'] is String ? value['taskStatus'] : null,
        download: value['download'] is Map
            ? BrowserDownloadIdentity.fromJson(value['download'])
            : null);
  }
  Map<String, dynamic> toJson() => {
        'version': 1,
        'operationId': operationId,
        'state': state,
        'taskIds': taskIds,
        if (taskIds.isNotEmpty) 'taskId': taskIds.single,
        if (reason != null) 'reason': reason,
        if (taskStatus != null) 'taskStatus': taskStatus,
        if (download != null) 'download': download!.toJson()
      };
  bool get committed => state == 'committed';
  String get message => state == 'prepared'
      ? '已准备，等待浏览器交接'
      : state == 'committed'
          ? '已交给 kownload 下载'
          : state == 'aborted'
              ? handoffEndMessage(reason)
              : '交接结果未知，请检查原下载和已有任务';
}

String handoffEndMessage(String? reason) => switch (reason) {
      'browser_completed' => '浏览器已完成下载，未启动 kownload 任务。',
      'user_cancelled' => '已取消交接，未启动 kownload 任务。',
      'browser_retained' => '此下载仍由浏览器处理，未启动 kownload 任务。',
      _ => '交接已终止，未启动 kownload 任务。',
    };

/// Optional task field. Unrecognized metadata remains blocked and visible,
/// rather than failing the entire old task list or enabling ordinary Continue.
class TaskHandoff {
  final int version;
  final String operationId, clientId, browser, phase, requestHash;
  final String? reason;
  final BrowserDownloadIdentity? download;
  const TaskHandoff(
      {required this.version,
      required this.operationId,
      required this.clientId,
      required this.browser,
      required this.phase,
      required this.requestHash,
      this.reason,
      this.download});
  factory TaskHandoff.fromJson(Map value) {
    BrowserDownloadIdentity? download;
    try {
      if (value['download'] is Map)
        download = BrowserDownloadIdentity.fromJson(value['download']);
    } on FormatException {
      /* Display unsupported state without granting Continue. */
    }
    String text(String key) => value[key] is String ? value[key] : '';
    return TaskHandoff(
        version: value['version'] is int ? value['version'] : 0,
        operationId: text('operationId'),
        clientId: text('clientId'),
        browser: text('browser'),
        phase: text('phase'),
        requestHash: text('requestHash'),
        reason: value['reason'] is String ? value['reason'] : null,
        download: download);
  }
  bool get blocked => version != 1 || phase != 'committed';
  String get label => version != 1
      ? '交接状态未知，请检查'
      : phase == 'prepared'
          ? '待浏览器交接'
          : phase == 'aborted'
              ? handoffEndMessage(reason)
              : phase == 'committed'
                  ? ''
                  : '交接状态未知，请检查';
  Map<String, dynamic> toJson() => {
        'version': version,
        'operationId': operationId,
        'clientId': clientId,
        'browser': browser,
        'phase': phase,
        'requestHash': requestHash,
        if (reason != null) 'reason': reason,
        if (download != null) 'download': download!.toJson()
      };
}
