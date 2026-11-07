import 'dart:io';

import 'package:path/path.dart' as path;

import '../util/util.dart';
import 'app_identity.dart';
import 'native_manifests.dart';

/// Browser routes as shown in Settings: null means the browser is not set up.
Future<Map<String, ManifestRoute?>> browserRoutes() async {
  final home = Platform.environment['HOME'] ?? '';
  final executable = await Util.homePathJoin(AppIdentity.hostExecutable);
  final routes = <String, ManifestRoute?>{};
  for (final browser in switchableBrowsers) {
    final directory = macNativeHostsDirectory(browser, home: home);
    routes[browser] = directory == null ||
            home.isEmpty ||
            !await Directory(path.dirname(directory)).exists()
        ? null
        : await manifestRoute(directory, executable);
  }
  return routes;
}

/// The explicit connect action. Previews only write into the temporary
/// directory named by KOWNLOAD_NATIVE_MANIFEST_DIR.
Future<List<String>> connectBrowsers() async {
  final executable = await Util.homePathJoin(AppIdentity.hostExecutable);
  if (Util.isIsolatedPreview) {
    await writeNativeManifests(
        directory: macManifestDirectory('chrome',
            preview: true,
            home: '',
            override: Platform.environment['KOWNLOAD_NATIVE_MANIFEST_DIR']),
        executable: executable,
        backupDirectory: path.join(
            Util.getStorageDir(), 'native-manifest-backups', 'preview'));
    return const ['preview'];
  }
  return switchBrowserManifests(
      home: Platform.environment['HOME'] ?? '',
      executable: executable,
      storageDir: Util.getStorageDir());
}

/// Replaceable in widget tests so they never touch real browser directories.
class BrowserConnection {
  static Future<Map<String, ManifestRoute?>> Function() routes = browserRoutes;
  static Future<List<String>> Function() connect = connectBrowsers;
}
