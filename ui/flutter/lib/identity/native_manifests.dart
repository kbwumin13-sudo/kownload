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

/// The old path-only helper cannot establish current App ownership. Call the
/// read-only NativeManifestService instead. Kept for older call sites until
/// the settings UI consumes per-browser evidence.
Future<ManifestRoute> manifestRoute(String directory, String executable) async {
  for (final name in [AppIdentity.hostName, AppIdentity.legacyHostName]) {
    if (await FileSystemEntity.type(path.join(directory, '$name.json'),
            followLinks: false) !=
        FileSystemEntityType.notFound) {
      return ManifestRoute.elsewhere;
    }
  }
  return ManifestRoute.none;
}
