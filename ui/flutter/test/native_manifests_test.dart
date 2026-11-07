import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/app_identity.dart';
import 'package:gopeed/identity/native_manifests.dart';

void main() {
  test('two names target one executable and preserve original manifest',
      () async {
    final root = await Directory.systemTemp.createTemp('manifest-fixture-');
    try {
      final dir = '${root.path}/manifests';
      final backup = '${root.path}/backup';
      await Directory(dir).create();
      final old = File('$dir/${AppIdentity.legacyHostName}.json');
      await old.writeAsString('original fixture');
      await writeNativeManifests(
          directory: dir,
          executable: '${root.path}/new-host',
          backupDirectory: backup);
      for (final name in [AppIdentity.hostName, AppIdentity.legacyHostName]) {
        final value = jsonDecode(await File('$dir/$name.json').readAsString());
        expect(value['name'], name);
        expect(value['path'], '${root.path}/new-host');
        expect(value['allowed_origins'],
            contains('chrome-extension://jcalkpjhhaachliihlkojhmafniepnoj/'));
      }
      await writeNativeManifests(
          directory: dir,
          executable: '${root.path}/new-host',
          backupDirectory: backup);
      expect(
          await File('$backup/${AppIdentity.legacyHostName}.json')
              .readAsString(),
          'original fixture');
      expect(
          await File('$backup/${AppIdentity.hostName}.absent').exists(), true);
      await writeNativeManifests(
          directory: dir,
          executable: '${root.path}/updated-host',
          backupDirectory: backup);
      expect(
          await File('$backup/${AppIdentity.legacyHostName}.json')
              .readAsString(),
          'original fixture');
      expect(
          await File('$backup/${AppIdentity.hostName}.json').exists(), false);
      expect(
          await File('$backup/${AppIdentity.hostName}.absent').exists(), true);
    } finally {
      await root.delete(recursive: true);
    }
  });
  test('Firefox uses its fixed extension allowlist', () {
    final manifest =
        nativeManifest(AppIdentity.hostName, '/fixture/host', firefox: true);
    expect(manifest.containsKey('allowed_origins'), false);
    expect(manifest['allowed_extensions'], AppIdentity.firefoxExtensionIds);
  });
}
