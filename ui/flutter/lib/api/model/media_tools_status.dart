enum MediaToolState {
  ready,
  missing,
  notRunnable,
  timedOut,
  notConfigured,
  inactive,
  unrecognized,
  unavailable,
  unsupported,
  incompatible;

  static MediaToolState fromJson(Object? value) => switch (value) {
        'ready' => ready,
        'missing' => missing,
        'not_runnable' => notRunnable,
        'timed_out' => timedOut,
        'not_configured' => notConfigured,
        'inactive' => inactive,
        'unrecognized' => unrecognized,
        'unavailable' => unavailable,
        'unsupported' => unsupported,
        'incompatible' => incompatible,
        _ => throw const FormatException('unknown_media_tool_state'),
      };
}

class MediaToolStatus {
  final MediaToolState state;
  final String setting, defaultPath, path, version;
  final bool enabled, custom, fallback;

  const MediaToolStatus(this.state,
      {this.setting = '',
      this.defaultPath = '',
      this.path = '',
      this.version = '',
      this.enabled = false,
      this.custom = false,
      this.fallback = false});

  factory MediaToolStatus.fromJson(Map value) => MediaToolStatus(
        MediaToolState.fromJson(value['state']),
        setting: value['setting'] as String,
        defaultPath: value['defaultPath'] as String,
        path: value['path'] as String,
        version: value['version'] as String,
        enabled: value['enabled'] as bool,
        custom: value['custom'] as bool,
        fallback: value['fallback'] as bool,
      );
}

class MediaBridgeStatus {
  final MediaToolState state;
  final String interpreter, version;

  const MediaBridgeStatus(this.state,
      {this.interpreter = '', this.version = ''});

  factory MediaBridgeStatus.fromJson(Map value) => MediaBridgeStatus(
        MediaToolState.fromJson(value['state']),
        interpreter: value['interpreter'] as String,
        version: value['version'] as String,
      );
}

class MediaToolsStatus {
  final MediaToolStatus ytDlp, ffmpeg, node;
  final MediaBridgeStatus bridge;

  const MediaToolsStatus(
      {required this.ytDlp,
      required this.ffmpeg,
      required this.node,
      required this.bridge});

  factory MediaToolsStatus.fromJson(Map value) => MediaToolsStatus(
        ytDlp: MediaToolStatus.fromJson(value['ytDlp'] as Map),
        ffmpeg: MediaToolStatus.fromJson(value['ffmpeg'] as Map),
        node: MediaToolStatus.fromJson(value['node'] as Map),
        bridge: MediaBridgeStatus.fromJson(value['bridge'] as Map),
      );
}

enum ManagedYtDlpAction { read, update, rollback }

class ManagedYtDlpState {
  final String entry, activeVersion, previousVersion;
  final bool inUse;

  const ManagedYtDlpState(
      {this.entry = '',
      this.activeVersion = '',
      this.previousVersion = '',
      this.inUse = false});

  factory ManagedYtDlpState.fromJson(Map value) => ManagedYtDlpState(
        entry: value['entry'] as String,
        activeVersion: value['activeVersion'] as String,
        previousVersion: value['previousVersion'] as String,
        inUse: value['inUse'] as bool,
      );
}

class ManagedYtDlpException implements Exception {
  final String code;

  const ManagedYtDlpException(this.code);
}
