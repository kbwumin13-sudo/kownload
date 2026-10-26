import 'dart:convert';
import 'dart:io';

import '../api/netdisk.dart';
import 'browser_profile_access.dart';

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
  final NetdiskException? profileError;
  const NetdiskBrowser(
      {required this.name,
      required this.application,
      required this.localStatePath,
      required this.profiles,
      this.profileError});
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
      : _readText = readText ?? BrowserProfileAccess.read,
        _appExists = appExists ?? ((p) => Directory(p).exists()),
        _run = run ?? _startDetached,
        _home = home ?? Platform.environment['HOME'] ?? '',
        _isMacOS = isMacOS ?? Platform.isMacOS;

  static Future<ProcessResult> _startDetached(
      String executable, List<String> arguments) async {
    // Await startup only: a newly opened browser can stay alive indefinitely.
    final process = await Process.start(executable, arguments,
        mode: ProcessStartMode.detached);
    return ProcessResult(process.pid, 0, '', '');
  }

  static List<NetdiskBrowserProfile> parseProfiles(String localState) {
    final root = jsonDecode(localState);
    if (root is! Map || (root['profile'] != null && root['profile'] is! Map)) {
      throw const FormatException('Invalid profile metadata');
    }
    final cache = (root['profile'] as Map?)?['info_cache'] ?? {};
    if (cache is! Map) {
      throw const FormatException('Invalid profile cache');
    }
    final profiles = <NetdiskBrowserProfile>[];
    for (final entry in cache.entries) {
      final directory = entry.key as String;
      // Profile directory is a single Chromium-managed directory name.
      if (directory != 'Default' &&
          !RegExp(r'^Profile \d+$').hasMatch(directory)) {
        continue;
      }
      final info = entry.value;
      final name = info is Map ? info['name'] : null;
      profiles.add(NetdiskBrowserProfile(
          directory, name is String && name.isNotEmpty ? name : directory));
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
      final profiles = parseProfiles(await _readText(localStatePath));
      if (profiles.isEmpty) {
        throw NetdiskException(
            '该文件没有可用的浏览器个人资料：$localStatePath。请先在浏览器中创建资料，或选择实际使用的 Local State 文件。',
            'empty_profiles');
      }
      return profiles;
    } on FormatException {
      throw NetdiskException(
          '浏览器个人资料格式无效：$localStatePath。请重新读取或选择实际使用的 Local State 文件。',
          'invalid_profile');
    } on FileSystemException catch (e) {
      final code = e.osError?.errorCode;
      final detail =
          e.osError == null ? e.message : '${e.osError!.message}（errno $code）';
      if (code == 1 || code == 13) {
        throw NetdiskException(
            '没有权限读取浏览器个人资料：$localStatePath。系统错误：$detail。请通过“选择资料文件”授权读取；若仍被拒绝，请检查 macOS 隐私与安全性中的文件访问权限。',
            'profile_permission_denied');
      }
      throw NetdiskException(
          '${code == 2 ? '未找到浏览器资料文件' : '无法读取浏览器资料文件'}：$localStatePath。系统错误：$detail。请选择实际使用的 Local State 文件。',
          code == 2 ? 'profile_not_found' : 'profile_read_failed');
    }
  }

  Future<List<NetdiskBrowser>> browsers() async {
    if (!_isMacOS) return [];
    if (!_home.startsWith('/')) {
      throw const NetdiskException(
          '无法确定浏览器资料路径：应用的 HOME 为空或不是绝对路径。请重新启动应用并检查运行环境。', 'invalid_home');
    }
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
      NetdiskException? profileError;
      try {
        profiles = await readProfiles(localState);
      } on NetdiskException catch (e) {
        profiles = [];
        profileError = e;
      }
      browsers.add(NetdiskBrowser(
          name: candidate.$1,
          application: application,
          localStatePath: localState,
          profiles: profiles,
          profileError: profileError));
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
    // Invoke Chromium directly so its existing instance receives the profile
    // and URL too; LaunchServices may ignore --args for an already running app.
    final executable = '${browser.application}/Contents/MacOS/${browser.name}';
    final ProcessResult result;
    try {
      result = await _run(executable, [
        '--profile-directory=${profile.directory}',
        if (![
          '$_home/Library/Application Support/Google/Chrome/Local State',
          '$_home/Library/Application Support/Microsoft Edge/Local State',
        ].contains(browser.localStatePath))
          '--user-data-dir=${File(browser.localStatePath).parent.path}',
        launchUrl,
      ]);
    } on ProcessException catch (e) {
      throw NetdiskException(
          '无法启动${browser.name}：${e.message}（错误码 ${e.errorCode}）。请确认浏览器已安装且允许执行。',
          'browser_launch_failed');
    }
    if (result.exitCode != 0) {
      throw const NetdiskException(
          '无法打开浏览器，请确认已安装并重新选择', 'browser_launch_failed');
    }
  }
}
