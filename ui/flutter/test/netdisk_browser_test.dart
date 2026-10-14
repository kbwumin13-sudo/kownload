import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/util/netdisk_browser.dart';

void main() {
  test('discovers Chrome and Edge using only Local State profile display names',
      () async {
    final reads = <String>[];
    final launches = <(String, List<String>)>[];
    final tool = NetdiskBrowserTool(
        home: '/fake user',
        isMacOS: true,
        appExists: (path) async => path.startsWith('/Applications/'),
        readText: (path) async {
          reads.add(path);
          return jsonEncode({
            'profile': {
              'info_cache': {
                'Default': {
                  'name': '学习',
                  'user_name': 'must-not-use@example.test'
                },
                'Profile 2': {'name': '工作'},
                '../invalid': {'name': '错误'},
              }
            },
            'os_crypt': {'encrypted_key': 'must-not-use'}
          });
        },
        run: (executable, arguments) async {
          launches.add((executable, arguments));
          return ProcessResult(0, 0, '', '');
        });
    final browsers = await tool.browsers();
    expect(browsers.map((b) => b.name), ['Google Chrome', 'Microsoft Edge']);
    expect(browsers.first.profiles.map((p) => p.name), ['学习', '工作']);
    expect(reads, [
      '/fake user/Library/Application Support/Google/Chrome/Local State',
      '/fake user/Library/Application Support/Microsoft Edge/Local State',
    ]);
    await tool.launch(
        browsers.last,
        browsers.last.profiles.last,
        const NetdiskSource(
            provider: 'quark', url: 'https://pan.quark.cn/s/a?pwd=abcd'));
    expect(launches.single.$1, '/usr/bin/open');
    expect(launches.single.$2, [
      '-a',
      '/Applications/Microsoft Edge.app',
      '--args',
      '--profile-directory=Profile 2',
      'https://pan.quark.cn/s/a?pwd=abcd',
    ]);
  });

  test('manual profile file is session-only and credential files are rejected',
      () async {
    final reads = <String>[];
    final tool = NetdiskBrowserTool(readText: (path) async {
      reads.add(path);
      return '{"profile":{"info_cache":{"Default":{"name":"临时选择"}}}}';
    });
    expect(
        (await tool.readProfiles('/chosen/Local State')).single.name, '临时选择');
    await expectLater(
        tool.readProfiles('/chosen/Cookies'), throwsA(isA<NetdiskException>()));
    expect(reads, ['/chosen/Local State']);
  });

  test('a manually chosen profile root is used for this launch only', () async {
    final launches = <List<String>>[];
    final tool = NetdiskBrowserTool(
        home: '/fake',
        isMacOS: true,
        run: (executable, arguments) async {
          launches.add(arguments);
          return ProcessResult(0, 0, '', '');
        });
    await tool.launch(
        const NetdiskBrowser(
            name: 'Google Chrome',
            application: '/Applications/Google Chrome.app',
            localStatePath: '/chosen profile/Local State',
            profiles: []),
        const NetdiskBrowserProfile('Profile 2', '工作'),
        const NetdiskSource(
            provider: 'quark', url: 'https://pan.quark.cn/s/a'));
    expect(launches.single, contains('--profile-directory=Profile 2'));
    expect(launches.single, contains('--user-data-dir=/chosen profile'));
  });
}
