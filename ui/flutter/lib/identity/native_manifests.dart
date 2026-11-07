import 'dart:convert';
import 'dart:io';
import 'package:path/path.dart' as path;
import 'app_identity.dart';

Map<String, dynamic> nativeManifest(String name, String executable,
        {bool firefox = false}) =>
    {
      'name': name,
      'description': 'kownload browser extension host',
      'path': executable,
      'type': 'stdio',
      if (firefox) 'allowed_extensions': AppIdentity.firefoxExtensionIds,
      if (!firefox)
        'allowed_origins': AppIdentity.chromiumExtensionIds
            .map((id) => 'chrome-extension://$id/')
            .toList(),
    };

/// The caller chooses an explicit destination. Startup never switches a browser.
/// Backups stay private and preserve the original bytes for simple rollback.
Future<void> writeNativeManifests(
    {required String directory,
    required String executable,
    required String backupDirectory,
    bool firefox = false}) async {
  if (!path.isAbsolute(directory) ||
      !path.isAbsolute(executable) ||
      !path.isAbsolute(backupDirectory)) {
    throw ArgumentError('absolute paths required');
  }
  await Directory(directory).create(recursive: true);
  await Directory(backupDirectory).create(recursive: true);
  await Process.run('/bin/chmod', ['700', backupDirectory]);
  for (final name in [AppIdentity.hostName, AppIdentity.legacyHostName]) {
    final file = File(path.join(directory, '$name.json'));
    final bytes = utf8.encode(const JsonEncoder.withIndent('  ')
        .convert(nativeManifest(name, executable, firefox: firefox)));
    if (await file.exists()) {
      final previous = await file.readAsBytes();
      if (base64Encode(previous) == base64Encode(bytes)) continue;
      // Never overwrite the first rollback copy of this installation.
      final backup = File(path.join(backupDirectory, '$name.json'));
      final absent = File(path.join(backupDirectory, '$name.absent'));
      if (!await backup.exists() && !await absent.exists()) {
        await backup.writeAsBytes(previous, flush: true);
        await Process.run('/bin/chmod', ['600', backup.path]);
      }
    } else {
      final absent = File(path.join(backupDirectory, '$name.absent'));
      final backup = File(path.join(backupDirectory, '$name.json'));
      if (!await absent.exists() && !await backup.exists()) {
        await absent.writeAsString('', flush: true);
      }
    }
    final temp = File('${file.path}.tmp');
    await temp.writeAsBytes(bytes, flush: true);
    await temp.rename(file.path);
  }
}

/// Chromium browsers whose manifests the installed app switches on macOS.
const switchableBrowsers = ['chrome', 'edge'];

/// The browser's own NativeMessagingHosts directory under [home].
String? macNativeHostsDirectory(String browser, {required String home}) {
  final support = path.join(home, 'Library', 'Application Support');
  switch (browser) {
    case 'chrome':
      return path.join(support, 'Google', 'Chrome', 'NativeMessagingHosts');
    case 'edge':
      return path.join(support, 'Microsoft Edge', 'NativeMessagingHosts');
  }
  return null;
}

/// Previews write manifests only into a temporary directory and never connect
/// a browser; the installed app writes the browser's own directory.
String macManifestDirectory(String browser,
    {required bool preview, required String home, String? override}) {
  if (preview) {
    final canonical = override == null ? null : path.canonicalize(override);
    if (canonical == null ||
        !(path.isWithin('/tmp', canonical) ||
            path.isWithin('/private/tmp', canonical) ||
            path.isWithin(
                path.canonicalize(Directory.systemTemp.path), canonical))) {
      throw StateError('预览版只在临时目录生成清单；正式连接在确认安装后进行');
    }
    return override!;
  }
  final directory =
      home.isEmpty ? null : macNativeHostsDirectory(browser, home: home);
  if (directory == null) throw StateError('暂不支持连接这个浏览器');
  return directory;
}

enum ManifestRoute { thisApp, elsewhere, none }

/// Where the browser's two host names lead now. The old version may have
/// rewritten the legacy name since, so this is shown, never assumed.
Future<ManifestRoute> manifestRoute(String directory, String executable) async {
  var present = 0, ours = 0;
  for (final name in [AppIdentity.hostName, AppIdentity.legacyHostName]) {
    final file = File(path.join(directory, '$name.json'));
    if (!await file.exists()) continue;
    present++;
    try {
      final value = jsonDecode(await file.readAsString());
      if (value is Map && value['path'] == executable) ours++;
    } catch (_) {}
  }
  if (ours == 2) return ManifestRoute.thisApp;
  return present == 0 ? ManifestRoute.none : ManifestRoute.elsewhere;
}

/// Explicit, reversible switch of every installed Chromium browser (its
/// profile directory exists) to [executable]: the new name is written and the
/// legacy name com.gopeed.gopeed is pointed at the same host. The first
/// original of each file is kept in <storage>/native-manifest-backups/<browser>
/// for scripts/rollback-identity.py. Returns the switched browsers.
Future<List<String>> switchBrowserManifests(
    {required String home,
    required String executable,
    required String storageDir}) async {
  final switched = <String>[];
  for (final browser in switchableBrowsers) {
    final directory = macNativeHostsDirectory(browser, home: home)!;
    if (!await Directory(path.dirname(directory)).exists()) continue;
    await writeNativeManifests(
        directory: directory,
        executable: executable,
        backupDirectory:
            path.join(storageDir, 'native-manifest-backups', browser));
    switched.add(browser);
  }
  return switched;
}
