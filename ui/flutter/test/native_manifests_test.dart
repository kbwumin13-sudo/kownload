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
  test('installed identity resolves browser directories; previews need temp',
      () {
    expect(
        macManifestDirectory('chrome', preview: false, home: '/Users/fixture'),
        '/Users/fixture/Library/Application Support/Google/Chrome/NativeMessagingHosts');
    expect(
        macManifestDirectory('edge', preview: false, home: '/Users/fixture'),
        '/Users/fixture/Library/Application Support/Microsoft Edge/NativeMessagingHosts');
    expect(() => macManifestDirectory('firefox', preview: false, home: '/h'),
        throwsStateError);
    expect(
        () => macManifestDirectory('chrome',
            preview: true,
            home: '/Users/fixture',
            override:
                '/Users/fixture/Library/Application Support/Google/Chrome/NativeMessagingHosts'),
        throwsStateError);
    expect(() => macManifestDirectory('chrome', preview: true, home: '/h'),
        throwsStateError);
    expect(
        macManifestDirectory('chrome',
            preview: true, home: '/h', override: '/private/tmp/fixture'),
        '/private/tmp/fixture');
  });
  test('switch touches installed browsers only and keeps the originals',
      () async {
    final root = await Directory.systemTemp.createTemp('manifest-switch-');
    try {
      final home = '${root.path}/home';
      final storage = '${root.path}/storage';
      final chrome = macNativeHostsDirectory('chrome', home: home)!;
      await Directory(chrome).create(recursive: true);
      final legacy = File('$chrome/${AppIdentity.legacyHostName}.json');
      const original = '{"name":"com.gopeed.gopeed","path":"/old/root/host"}';
      await legacy.writeAsString(original);
      await File('$chrome/com.example.other.json').writeAsString('other');
      expect(await manifestRoute(chrome, '$storage/kownload-native-host'),
          ManifestRoute.elsewhere);
      expect(
          await manifestRoute(macNativeHostsDirectory('edge', home: home)!,
              '$storage/kownload-native-host'),
          ManifestRoute.none);
      for (var i = 0; i < 2; i++) {
        final switched = await switchBrowserManifests(
            home: home,
            executable: '$storage/kownload-native-host',
            storageDir: storage);
        expect(switched, ['chrome'], reason: 'Edge has no profile directory');
      }
      expect(await manifestRoute(chrome, '$storage/kownload-native-host'),
          ManifestRoute.thisApp);
      for (final name in [AppIdentity.hostName, AppIdentity.legacyHostName]) {
        final value = jsonDecode(await File('$chrome/$name.json').readAsString());
        expect(value['path'], '$storage/kownload-native-host');
      }
      final backup = '$storage/native-manifest-backups/chrome';
      expect(await File('$backup/${AppIdentity.legacyHostName}.json')
          .readAsString(), original);
      expect(await File('$backup/${AppIdentity.hostName}.absent').exists(), true);
      expect(await File('$chrome/com.example.other.json').readAsString(),
          'other');
      expect(
          await Directory(
                  '$home/Library/Application Support/Microsoft Edge')
              .exists(),
          false);
    } finally {
      await root.delete(recursive: true);
    }
  });
}
