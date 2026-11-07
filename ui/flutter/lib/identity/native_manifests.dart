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
      !path.isAbsolute(backupDirectory))
    throw ArgumentError('absolute paths required');
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
      if (!await backup.exists()) {
        await backup.writeAsBytes(previous, flush: true);
        await Process.run('/bin/chmod', ['600', backup.path]);
      }
    } else {
      final absent = File(path.join(backupDirectory, '$name.absent'));
      if (!await absent.exists()) await absent.writeAsString('', flush: true);
    }
    final temp = File('${file.path}.tmp');
    await temp.writeAsBytes(bytes, flush: true);
    await temp.rename(file.path);
  }
}
