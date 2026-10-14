import 'dart:convert';
import 'dart:io';

import '../api/netdisk.dart';

class NetdiskBrowserProfile {
  final String directory;
  final String name;
  const NetdiskBrowserProfile(this.directory, this.name);
}

class NetdiskBrowser {
  final String name;
  final String application;
  final String localStatePath;
  final List<NetdiskBrowserProfile> profiles;
  const NetdiskBrowser(
      {required this.name,
      required this.application,
      required this.localStatePath,
      required this.profiles});
}

typedef NetdiskReadText = Future<String> Function(String path);
typedef NetdiskRunProcess = Future<ProcessResult> Function(
    String executable, List<String> arguments);

/// Session-only browser choice. Reads profile display names from Local State;
/// never opens Cookies, Login Data, Preferences, or a credential store.
class NetdiskBrowserTool {
  final NetdiskReadText _readText;
  final Future<bool> Function(String) _appExists;
  final NetdiskRunProcess _run;
  final String _home;
  final bool _isMacOS;
  NetdiskBrowserTool(
      {NetdiskReadText? readText,
      Future<bool> Function(String)? appExists,
      NetdiskRunProcess? run,
      String? home,
      bool? isMacOS})
      : _readText = readText ?? ((p) => File(p).readAsString()),
        _appExists = appExists ?? ((p) => Directory(p).exists()),
        _run = run ??
            ((executable, arguments) => Process.run(executable, arguments)),
        _home = home ?? Platform.environment['HOME'] ?? '',
        _isMacOS = isMacOS ?? Platform.isMacOS;

  static List<NetdiskBrowserProfile> parseProfiles(String localState) {
    final root = jsonDecode(localState) as Map;
    final cache = (root['profile'] as Map?)?['info_cache'] as Map? ?? {};
    final profiles = <NetdiskBrowserProfile>[];
    for (final entry in cache.entries) {
      final directory = entry.key as String;
      // Profile directory is a single Chromium-managed directory name.
      if (directory != 'Default' &&
          !RegExp(r'^Profile \d+$').hasMatch(directory)) {
        continue;
      }
      final info = entry.value;
      final name = info is Map ? info['name'] as String? : null;
      profiles.add(NetdiskBrowserProfile(directory,
          name?.isNotEmpty == true ? name! : '个人资料 ${profiles.length + 1}'));
    }
    profiles.sort((a, b) => a.directory.compareTo(b.directory));
    return profiles;
  }

  Future<List<NetdiskBrowserProfile>> readProfiles(
      String localStatePath) async {
    if (localStatePath.split(Platform.pathSeparator).last != 'Local State') {
      throw const NetdiskException('请选择浏览器的 Local State 文件', 'invalid_profile');
    }
    try {
      return parseProfiles(await _readText(localStatePath));
    } on FormatException {
      throw const NetdiskException(
          '无法读取浏览器个人资料，请重新选择 Local State 文件', 'invalid_profile');
    }
  }

  Future<List<NetdiskBrowser>> browsers() async {
    if (!_isMacOS) return [];
    final browsers = <NetdiskBrowser>[];
    for (final candidate in [
      ('Google Chrome', 'Google/Chrome'),
      ('Microsoft Edge', 'Microsoft Edge'),
    ]) {
      var application = '/Applications/${candidate.$1}.app';
      if (!await _appExists(application)) {
        application = '$_home/Applications/${candidate.$1}.app';
        if (!await _appExists(application)) continue;
      }
      final localState =
          '$_home/Library/Application Support/${candidate.$2}/Local State';
      List<NetdiskBrowserProfile> profiles;
      try {
        profiles = await readProfiles(localState);
      } on FileSystemException {
        profiles = [];
      } on NetdiskException {
        profiles = [];
      }
      browsers.add(NetdiskBrowser(
          name: candidate.$1,
          application: application,
          localStatePath: localState,
          profiles: profiles));
    }
    return browsers;
  }

  Future<void> launch(NetdiskBrowser browser, NetdiskBrowserProfile profile,
      NetdiskSource source,
      {String? requestId}) async {
    if (!_isMacOS) {
      throw const NetdiskException(
          '请在已安装助手的浏览器中打开原网盘页面', 'unsupported_browser');
    }
    if (netdiskProviderForUrl(source.url) != source.provider ||
        (profile.directory != 'Default' &&
            !RegExp(r'^Profile \d+$').hasMatch(profile.directory))) {
      throw const NetdiskException('浏览器个人资料或网盘页面无效', 'invalid_profile');
    }
    final uri = Uri.parse(source.url);
    final launchUrl = requestId == null
        ? source.url
        : uri.replace(queryParameters: {
            ...uri.queryParameters,
            'gopeed_request': requestId
          }).toString();
    final result = await _run('/usr/bin/open', [
      '-a',
      browser.application,
      '--args',
      '--profile-directory=${profile.directory}',
      if (![
        '$_home/Library/Application Support/Google/Chrome/Local State',
        '$_home/Library/Application Support/Microsoft Edge/Local State',
      ].contains(browser.localStatePath))
        '--user-data-dir=${File(browser.localStatePath).parent.path}',
      launchUrl,
    ]);
    if (result.exitCode != 0) {
      throw const NetdiskException(
          '无法打开浏览器，请确认已安装并重新选择', 'browser_launch_failed');
    }
  }
}
