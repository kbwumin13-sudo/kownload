import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/util/netdisk_browser.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test('default reader uses native metadata and preserves native access errors',
      () async {
    const channel = MethodChannel('gopeed/browser_profiles');
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    final calls = <MethodCall>[];
    var denied = false;
    messenger.setMockMethodCallHandler(channel, (call) async {
      calls.add(call);
      if (denied) {
        throw PlatformException(
            code: 'profile_permission_denied',
            message: '主应用无法读取 /native/Local State（errno 1）');
      }
      return '{"profile":{"info_cache":{"Default":{"name":"原生授权资料"}}}}';
    });
    addTearDown(() => messenger.setMockMethodCallHandler(channel, null));
    final tool = NetdiskBrowserTool();
    expect(
        (await tool.readProfiles('/native/Local State')).single.name, '原生授权资料');
    denied = true;
    await expectLater(
        tool.readProfiles('/native/Local State'),
        throwsA(isA<NetdiskException>()
            .having((e) => e.reasonCode, 'native reason',
                'profile_permission_denied')
            .having((e) => e.message, 'native detail', contains('errno 1'))));
    expect(calls.map((call) => call.method), ['read', 'read']);
    expect(
        calls.every((call) => call.arguments['path'] == '/native/Local State'),
        isTrue);
  }, skip: !Platform.isMacOS);

  test('discovery keeps each browser read failure without inventing profiles',
      () async {
    for (final reason in [
      'permission',
      'missing',
      'empty',
      'invalid',
      'read'
    ]) {
      final reads = <String>[];
      final tool = NetdiskBrowserTool(
          home: '/real home',
          isMacOS: true,
          appExists: (_) async => true,
          readText: (path) async {
            reads.add(path);
            if (path.contains('Google/Chrome')) {
              if (reason == 'empty') return '{}';
              if (reason == 'invalid') return 'not json';
              throw FileSystemException(
                  'read failed',
                  path,
                  OSError(
                      'system detail',
                      reason == 'permission'
                          ? 13
                          : reason == 'missing'
                              ? 2
                              : 5));
            }
            return '{"profile":{"info_cache":{"Default":{"name":"Edge"}}}}';
          });
      final browsers = await tool.browsers();
      final chrome = browsers.first;
      expect(chrome.profiles, isEmpty);
      expect(
          chrome.profileError?.reasonCode,
          {
            'permission': 'profile_permission_denied',
            'missing': 'profile_not_found',
            'empty': 'empty_profiles',
            'invalid': 'invalid_profile',
            'read': 'profile_read_failed',
          }[reason]);
      expect(chrome.profileError?.message, contains(chrome.localStatePath));
      expect(browsers.last.profiles.single.name, 'Edge');
      expect(browsers.last.profileError, isNull);
      expect(reads, hasLength(2));
      expect(reads.every((path) => path.endsWith('/Local State')), isTrue);
    }
  });

  test('only actual valid profile directories are selectable and launchable',
      () async {
    final profiles = NetdiskBrowserTool.parseProfiles(jsonEncode({
      'profile': {
        'info_cache': {
          'Default': {'name': 42},
          'Profile 12': {},
          'Profile x': {},
          'Profile 2/other': {},
          '../Default': {},
          r'Profile \d': {},
        }
      }
    }));
    expect(profiles.map((p) => p.directory), ['Default', 'Profile 12']);
    expect(profiles.map((p) => p.name), ['Default', 'Profile 12']);
    var launched = false;
    final tool = NetdiskBrowserTool(
        home: '/fake',
        isMacOS: true,
        run: (_, __) async {
          launched = true;
          return ProcessResult(0, 0, '', '');
        });
    await expectLater(
        tool.launch(
            const NetdiskBrowser(
                name: 'Chrome',
                application: '/Applications/Google Chrome.app',
                localStatePath: '/chosen/Local State',
                profiles: []),
            const NetdiskBrowserProfile('../Profile 2', 'bad'),
            const NetdiskSource(
                provider: 'quark', url: 'https://pan.quark.cn/s/a')),
        throwsA(isA<NetdiskException>()));
    expect(launched, isFalse);
  });

  test('permission failures include the attempted Local State path', () async {
    final tool = NetdiskBrowserTool(readText: (path) async {
      throw FileSystemException('Cannot open file', path,
          const OSError('Operation not permitted', 1));
    });
    await expectLater(
        tool.readProfiles('/real home/Local State'),
        throwsA(isA<NetdiskException>()
            .having((e) => e.reasonCode, 'reason', 'profile_permission_denied')
            .having(
                (e) => e.message, 'path', contains('/real home/Local State'))
            .having((e) => e.message, 'OS error', contains('1'))));
  });

  test('empty profiles and malformed profile metadata are explicit failures',
      () async {
    for (final state in ['{}', '{"profile":{"info_cache":{}}}']) {
      final tool = NetdiskBrowserTool(readText: (_) async => state);
      await expectLater(
          tool.readProfiles('/chosen/Local State'),
          throwsA(isA<NetdiskException>()
              .having((e) => e.reasonCode, 'reason', 'empty_profiles')));
    }
    for (final state in [
      '[]',
      '{"profile":[]}',
      '{"profile":{"info_cache":[]}}'
    ]) {
      final tool = NetdiskBrowserTool(readText: (_) async => state);
      await expectLater(
          tool.readProfiles('/chosen/Local State'),
          throwsA(isA<NetdiskException>()
              .having((e) => e.reasonCode, 'reason', 'invalid_profile')));
    }
  });

  test('missing HOME is diagnosed before any guessed filesystem reads',
      () async {
    final reads = <String>[];
    final tool = NetdiskBrowserTool(
        home: '',
        isMacOS: true,
        appExists: (_) async => true,
        readText: (path) async {
          reads.add(path);
          return '{}';
        });
    await expectLater(
        tool.browsers(),
        throwsA(isA<NetdiskException>()
            .having((e) => e.reasonCode, 'reason', 'invalid_home')));
    expect(reads, isEmpty);
  });

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
    for (final browser in browsers) {
      await tool.launch(
          browser,
          browser.profiles.last,
          const NetdiskSource(
              provider: 'quark', url: 'https://pan.quark.cn/s/a?pwd=abcd'));
      expect(launches.last.$1,
          '${browser.application}/Contents/MacOS/${browser.name}');
      expect(launches.last.$2, [
        '--profile-directory=Profile 2',
        'https://pan.quark.cn/s/a?pwd=abcd',
      ]);
    }
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

  test(
      'direct launch preserves the selected profile, custom root and claim URL',
      () async {
    final launches = <(String, List<String>)>[];
    final tool = NetdiskBrowserTool(
        home: '/fake',
        isMacOS: true,
        run: (executable, arguments) async {
          launches.add((executable, arguments));
          return ProcessResult(0, 0, '', '');
        });
    for (final name in ['Google Chrome', 'Microsoft Edge']) {
      await tool.launch(
          NetdiskBrowser(
              name: name,
              application: '/fake/Applications/$name.app',
              localStatePath: '/custom root/Local State',
              profiles: []),
          const NetdiskBrowserProfile('Default', '实际资料'),
          const NetdiskSource(
              provider: 'quark', url: 'https://pan.quark.cn/s/a?pwd=abcd#file'),
          requestId: 'request with space/&');
      expect(launches.last.$1,
          '/fake/Applications/$name.app/Contents/MacOS/$name');
      expect(launches.last.$2.take(2),
          ['--profile-directory=Default', '--user-data-dir=/custom root']);
      final url = Uri.parse(launches.last.$2.last);
      expect(url.host, 'pan.quark.cn');
      expect(url.queryParameters,
          {'pwd': 'abcd', 'gopeed_request': 'request with space/&'});
      expect(url.fragment, 'file');
    }
  });

  test('process startup exceptions and failed launch results are actionable',
      () async {
    for (final code in [2, 13, -1]) {
      final tool = NetdiskBrowserTool(
          isMacOS: true,
          run: (executable, arguments) async {
            if (code == -1) return ProcessResult(0, 1, '', 'failed');
            throw ProcessException(
                executable, arguments, 'Cannot start browser', code);
          });
      await expectLater(
          tool.launch(
              const NetdiskBrowser(
                  name: 'Google Chrome',
                  application: '/Applications/Google Chrome.app',
                  localStatePath: '/custom/Local State',
                  profiles: []),
              const NetdiskBrowserProfile('Default', '默认'),
              const NetdiskSource(
                  provider: 'quark', url: 'https://pan.quark.cn/s/a')),
          throwsA(isA<NetdiskException>()
              .having((e) => e.reasonCode, 'reason', 'browser_launch_failed')));
    }
  });

  test('default launcher returns while a newly started browser is still alive',
      () async {
    final root = await Directory.systemTemp.createTemp('netdisk-launch-');
    int? processId;
    try {
      final app = '${root.path}/Google Chrome.app';
      final executable = File('$app/Contents/MacOS/Google Chrome');
      await executable.parent.create(recursive: true);
      // A browser-shaped fixture records argv and stays running until cleanup.
      await executable.writeAsString(r'''#!/bin/sh
printf '%s\n' "$$" "$@" > "$0.invocation"
exec /bin/sleep 30
''');
      expect(
          (await Process.run('/bin/chmod', ['+x', executable.path])).exitCode,
          0);
      final invocation = File('${executable.path}.invocation');
      final tool = NetdiskBrowserTool(home: root.path, isMacOS: true);
      try {
        await tool
            .launch(
                NetdiskBrowser(
                    name: 'Google Chrome',
                    application: app,
                    localStatePath: '${root.path}/Local State',
                    profiles: []),
                const NetdiskBrowserProfile('Default', '默认'),
                const NetdiskSource(
                    provider: 'quark', url: 'https://pan.quark.cn/s/a'),
                requestId: 'claim')
            .timeout(const Duration(seconds: 3));
        final deadline = DateTime.now().add(const Duration(seconds: 2));
        while (
            !await invocation.exists() && DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 10));
        }
        final args = await invocation.readAsLines();
        processId = int.parse(args.first);
        expect(args.skip(1), [
          '--profile-directory=Default',
          '--user-data-dir=${root.path}',
          'https://pan.quark.cn/s/a?gopeed_request=claim',
        ]);
        expect(
            (await Process.run('/bin/kill', ['-0', '$processId'])).exitCode, 0);
      } finally {
        if (processId == null && await invocation.exists()) {
          processId = int.tryParse((await invocation.readAsLines()).first);
        }
      }
    } finally {
      if (processId != null) {
        Process.killPid(processId, ProcessSignal.sigterm);
      }
      await root.delete(recursive: true);
    }
  });
}
