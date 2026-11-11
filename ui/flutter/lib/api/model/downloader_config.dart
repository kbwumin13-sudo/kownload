import 'package:json_annotation/json_annotation.dart';

part 'downloader_config.g.dart';

@JsonSerializable(explicitToJson: true)
class DownloaderConfig {
  String downloadDir;
  int maxRunning;
  ProtocolConfig protocolConfig = ProtocolConfig();
  ExtraConfig extra = ExtraConfig();
  ProxyConfig proxy = ProxyConfig();
  WebhookConfig webhook = WebhookConfig();
  ScriptConfig script = ScriptConfig();
  AutoTorrentConfig autoTorrent = AutoTorrentConfig();
  ArchiveConfig archive = ArchiveConfig();
  bool autoDeleteMissingFileTasks;

  DownloaderConfig({
    this.downloadDir = '',
    this.maxRunning = 0,
    this.autoDeleteMissingFileTasks = false,
  });

  factory DownloaderConfig.fromJson(Map<String, dynamic> json) =>
      _$DownloaderConfigFromJson(json);

  Map<String, dynamic> toJson() => _$DownloaderConfigToJson(this);
}

// Protocols may add fields before this frontend exposes an editor for them.
// Keep those JSON values in place while typed fields retain their normal rules.
mixin _ProtocolFields {
  Map<String, dynamic> _unrecognizedFields = {};

  void _readProtocolFields(Map<String, dynamic> json, Set<String> known) {
    _unrecognizedFields = {
      for (final entry in json.entries)
        if (!known.contains(entry.key))
          entry.key: _copyProtocolValue(entry.value),
    };
  }

  Map<String, dynamic> _writeProtocolFields(Map<String, dynamic> known) => {
        for (final entry in _unrecognizedFields.entries)
          entry.key: _copyProtocolValue(entry.value),
        ...known,
      };
}

dynamic _copyProtocolValue(dynamic value) {
  if (value is Map) {
    return {
      for (final entry in value.entries)
        entry.key: _copyProtocolValue(entry.value),
    };
  }
  if (value is List) return value.map(_copyProtocolValue).toList();
  return value;
}

@JsonSerializable(explicitToJson: true)
class ProtocolConfig with _ProtocolFields {
  HttpConfig http = HttpConfig();
  BtConfig bt = BtConfig();
  Ed2kConfig ed2k = Ed2kConfig();
  MediaConfig media = MediaConfig();
  // The native netdisk downloader owns this HTTP-shaped configuration.
  // Retain it unchanged when saving unrelated frontend preferences.
  @JsonKey(includeIfNull: false)
  Map<String, dynamic>? netdisk;

  ProtocolConfig();

  factory ProtocolConfig.fromJson(Map<String, dynamic>? json) {
    if (json == null) return ProtocolConfig();
    return _$ProtocolConfigFromJson(json)
      .._readProtocolFields(json, {'http', 'bt', 'ed2k', 'media', 'netdisk'});
  }

  Map<String, dynamic> toJson() =>
      _writeProtocolFields(_$ProtocolConfigToJson(this));
}

@JsonSerializable()
class MediaConfig with _ProtocolFields {
  String ytDlpPath;
  String ffmpegPath;
  @JsonKey(includeIfNull: false)
  String? jsRuntimePath;
  int connections;

  MediaConfig({
    this.ytDlpPath = '',
    this.ffmpegPath = '',
    this.jsRuntimePath,
    this.connections = 4,
  });

  factory MediaConfig.fromJson(Map<String, dynamic>? json) {
    if (json == null) return MediaConfig();
    return _$MediaConfigFromJson(json)
      .._readProtocolFields(json, {
        'ytDlpPath',
        'ffmpegPath',
        'jsRuntimePath',
        'connections',
      });
  }

  Map<String, dynamic> toJson() =>
      _writeProtocolFields(_$MediaConfigToJson(this));
}

@JsonSerializable()
class HttpConfig with _ProtocolFields {
  String userAgent;
  int connections;
  bool useServerCtime;

  HttpConfig({
    this.userAgent = '',
    this.connections = 0,
    this.useServerCtime = false,
  });

  factory HttpConfig.fromJson(Map<String, dynamic> json) {
    return _$HttpConfigFromJson(json)
      .._readProtocolFields(json, {
        'userAgent',
        'connections',
        'useServerCtime',
      });
  }

  Map<String, dynamic> toJson() =>
      _writeProtocolFields(_$HttpConfigToJson(this));
}

@JsonSerializable()
class BtConfig with _ProtocolFields {
  int listenPort;
  List<String> trackers;
  bool seedKeep;
  double seedRatio;
  int seedTime;

  BtConfig({
    this.listenPort = 0,
    this.trackers = const [],
    this.seedKeep = false,
    this.seedRatio = 0,
    this.seedTime = 0,
  });

  factory BtConfig.fromJson(Map<String, dynamic> json) {
    return _$BtConfigFromJson(json)
      .._readProtocolFields(json, {
        'listenPort',
        'trackers',
        'seedKeep',
        'seedRatio',
        'seedTime',
      });
  }

  Map<String, dynamic> toJson() => _writeProtocolFields(_$BtConfigToJson(this));
}

@JsonSerializable()
class Ed2kConfig with _ProtocolFields {
  int listenPort;
  int udpPort;
  String serverAddr;
  String serverMet;
  String nodesDat;

  Ed2kConfig({
    this.listenPort = 0,
    this.udpPort = 0,
    this.serverAddr = '',
    this.serverMet = '',
    this.nodesDat = '',
  });

  factory Ed2kConfig.fromJson(Map<String, dynamic>? json) {
    if (json == null) return Ed2kConfig();
    return _$Ed2kConfigFromJson(json)
      .._readProtocolFields(json, {
        'listenPort',
        'udpPort',
        'serverAddr',
        'serverMet',
        'nodesDat',
      });
  }

  Map<String, dynamic> toJson() =>
      _writeProtocolFields(_$Ed2kConfigToJson(this));
}

@JsonSerializable(explicitToJson: true)
class ExtraConfig {
  String themeMode;
  String locale;
  bool lastDeleteTaskKeep;
  bool defaultDirectDownload;
  bool defaultBtClient;
  bool notifyWhenNewVersion;
  bool autoStartTasks;
  bool desktopNotification;
  List<DownloadCategory> downloadCategories;

  ExtraConfigBt bt = ExtraConfigBt();
  ExtraConfigGithubMirror githubMirror = ExtraConfigGithubMirror();

  ExtraConfig({
    this.themeMode = '',
    this.locale = '',
    this.lastDeleteTaskKeep = false,
    this.defaultDirectDownload = false,
    this.defaultBtClient = true,
    this.notifyWhenNewVersion = true,
    this.autoStartTasks = false,
    this.desktopNotification = true,
    this.downloadCategories = const [],
  });

  factory ExtraConfig.fromJson(Map<String, dynamic>? json) =>
      json == null ? ExtraConfig() : _$ExtraConfigFromJson(json);

  Map<String, dynamic> toJson() => _$ExtraConfigToJson(this);
}

@JsonSerializable()
class DownloadCategory {
  String name;
  String path;
  bool isBuiltIn;
  String? nameKey; // i18n key for built-in categories (e.g., 'categoryMusic')
  bool
      isDeleted; // Mark built-in categories as deleted instead of removing them

  DownloadCategory({
    required this.name,
    required this.path,
    this.isBuiltIn = false,
    this.nameKey,
    this.isDeleted = false,
  });

  factory DownloadCategory.fromJson(Map<String, dynamic> json) =>
      _$DownloadCategoryFromJson(json);

  Map<String, dynamic> toJson() => _$DownloadCategoryToJson(this);
}

@JsonSerializable()
class WebhookConfig {
  bool enable;
  List<String> urls;

  WebhookConfig({
    this.enable = false,
    this.urls = const [],
  });

  factory WebhookConfig.fromJson(Map<String, dynamic>? json) =>
      json == null ? WebhookConfig() : _$WebhookConfigFromJson(json);

  Map<String, dynamic> toJson() => _$WebhookConfigToJson(this);
}

@JsonSerializable()
class ScriptConfig {
  bool enable;
  List<String> paths;

  ScriptConfig({
    this.enable = false,
    this.paths = const [],
  });

  factory ScriptConfig.fromJson(Map<String, dynamic>? json) =>
      json == null ? ScriptConfig() : _$ScriptConfigFromJson(json);

  Map<String, dynamic> toJson() => _$ScriptConfigToJson(this);
}

@JsonSerializable()
class ProxyConfig {
  bool enable;
  bool system;
  String scheme;
  String host;
  String usr;
  String pwd;

  ProxyConfig({
    this.enable = false,
    this.system = false,
    this.scheme = '',
    this.host = '',
    this.usr = '',
    this.pwd = '',
  });

  factory ProxyConfig.fromJson(Map<String, dynamic> json) =>
      _$ProxyConfigFromJson(json);

  Map<String, dynamic> toJson() => _$ProxyConfigToJson(this);
}

@JsonSerializable()
class ExtraConfigBt {
  List<String> trackerSubscribeUrls = [];
  List<String> subscribeTrackers = [];
  bool autoUpdateTrackers = true;
  DateTime? lastTrackerUpdateTime;

  List<String> customTrackers = [];

  ExtraConfigBt();

  factory ExtraConfigBt.fromJson(Map<String, dynamic> json) =>
      _$ExtraConfigBtFromJson(json);

  Map<String, dynamic> toJson() => _$ExtraConfigBtToJson(this);
}

enum GithubMirrorType {
  jsdelivr,
  ghProxy,
}

@JsonSerializable()
class GithubMirror {
  GithubMirrorType type;
  String url;
  bool isBuiltIn;
  bool isDeleted;

  GithubMirror({
    required this.type,
    required this.url,
    this.isBuiltIn = false,
    this.isDeleted = false,
  });

  factory GithubMirror.fromJson(Map<String, dynamic> json) =>
      _$GithubMirrorFromJson(json);

  Map<String, dynamic> toJson() => _$GithubMirrorToJson(this);
}

@JsonSerializable(explicitToJson: true)
class ExtraConfigGithubMirror {
  bool enabled;
  List<GithubMirror> mirrors;

  ExtraConfigGithubMirror({
    this.enabled = true,
    this.mirrors = const [],
  });

  factory ExtraConfigGithubMirror.fromJson(Map<String, dynamic>? json) =>
      json == null
          ? ExtraConfigGithubMirror()
          : _$ExtraConfigGithubMirrorFromJson(json);

  Map<String, dynamic> toJson() => _$ExtraConfigGithubMirrorToJson(this);
}

@JsonSerializable()
class AutoTorrentConfig {
  bool enable;
  bool deleteAfterDownload;

  AutoTorrentConfig({
    this.enable = false,
    this.deleteAfterDownload = false,
  });

  factory AutoTorrentConfig.fromJson(Map<String, dynamic>? json) =>
      json == null ? AutoTorrentConfig() : _$AutoTorrentConfigFromJson(json);

  Map<String, dynamic> toJson() => _$AutoTorrentConfigToJson(this);
}

@JsonSerializable()
class ArchiveConfig {
  bool autoExtract;
  bool deleteAfterExtract;

  ArchiveConfig({
    this.autoExtract = true,
    this.deleteAfterExtract = true,
  });

  factory ArchiveConfig.fromJson(Map<String, dynamic>? json) =>
      json == null ? ArchiveConfig() : _$ArchiveConfigFromJson(json);

  Map<String, dynamic> toJson() => _$ArchiveConfigToJson(this);
}
