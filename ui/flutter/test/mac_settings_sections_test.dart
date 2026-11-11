import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/core/common/start_config.dart';
import 'package:gopeed/app/modules/extension/controllers/extension_controller.dart';
import 'package:gopeed/app/modules/extension/views/extension_view.dart';
import 'package:gopeed/app/modules/setting/controllers/setting_controller.dart';
import 'package:gopeed/app/modules/setting/views/setting_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/identity/browser_connection.dart';
import 'package:gopeed/identity/native_manifests.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_appearance.dart';
import 'package:gopeed/macos/mac_shell.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/package_info.dart';

import 'support/onboarding_check_fixture.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}

  bool failSave = false;
  Completer<void>? holdSave;
  final savedConfigs = <Map<String, dynamic>>[];
  final savedStarts = <Map<String, String>>[];
  final trackerRequests = <List<String>>[];
  bool failTracker = false;

  @override
  Future<void> trackerUpdate() async {
    trackerRequests
        .add(List.of(downloaderConfig.value.extra.bt.trackerSubscribeUrls));
    if (failTracker) throw StateError('fixture update failed');
  }

  Completer<void>? holdStartSave;

  @override
  Future<void> saveStartConfig() async {
    savedStarts.add({
      'network': startConfig.value.network,
      'address': startConfig.value.address,
      'apiToken': startConfig.value.apiToken
    });
    if (failSave) throw StateError('fixture preference write failed');
    await holdStartSave?.future;
  }

  @override
  Future<void> saveConfig() async {
    savedConfigs.add(jsonDecode(jsonEncode(downloaderConfig.value.toJson()))
        as Map<String, dynamic>);
    if (failSave) throw StateError('service unavailable');
    final hold = holdSave;
    holdSave = null;
    await hold?.future;
  }
}

class _PolicyStart extends StartConfig {
  final policy = 'fixture-policy-must-survive';
}

class _Extensions extends ExtensionController {
  @override
  Future<void> loadInitialData() async {}
}

class _Settings extends SettingController {
  @override
  void fetchLatestVersion() {}
}

class _Box extends Fake implements Box {
  final _storedValues = <dynamic, dynamic>{};

  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      _storedValues[key] ?? defaultValue;

  @override
  Future<void> put(dynamic key, dynamic value) async {
    _storedValues[key] = value;
  }
}

Future<_App> _mountSettings(WidgetTester tester, String section,
    {bool shell = false, Future<void> Function(String)? webhookTest}) async {
  await tester.binding.setSurfaceSize(const Size(900, 700));
  addTearDown(() => tester.binding.setSurfaceSize(null));
  Database.instance.box = _Box();
  packageInfo = PackageInfo(
      appName: 'kownload',
      packageName: 'com.gopeed.gopeed',
      version: 'test',
      buildNumber: '1');
  final app = _App();
  app.downloaderConfig.value.downloadDir = '/downloads';
  app.downloaderConfig.value.maxRunning = 3;
  app.downloaderConfig.value.archive.autoExtract = false;
  app.downloaderConfig.value.protocolConfig.media.ytDlpPath =
      '/custom/resolver';
  app.downloaderConfig.value.protocolConfig.http.userAgent = 'preserved-agent';
  app.downloaderConfig.value.protocolConfig.netdisk = {'connections': 7};
  app.downloaderConfig.value.extra.themeMode = 'system';
  app.downloaderConfig.value.extra.locale = 'en_US';
  app.startConfig.value.network = 'tcp';
  app.startConfig.value.address = '127.0.0.1:9999';
  app.startConfig.value.apiToken = '';
  Get.put<AppController>(app);
  Get.put<SettingController>(_Settings());
  MacPanel.settingsSection.value = section;
  MacPanel.presentation.value = 'settings';
  Widget settings() {
    final view = SettingView(webhookTest: webhookTest);
    return shell ? MacShell(selectedIndex: 2, child: view) : view;
  }

  await tester.pumpWidget(
      GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
    GetPage(name: '/', page: settings),
    GetPage(name: Routes.SETTING, page: settings),
    GetPage(
        name: Routes.EXTENSION,
        page: () => const ExtensionView(),
        binding:
            BindingsBuilder(() => Get.put<ExtensionController>(_Extensions()))),
  ]));
  await tester.pumpAndSettle();
  return app;
}

Future<void> _reveal(WidgetTester tester, String text) async {
  final finder = find.text(text);
  await tester.scrollUntilVisible(finder, 220,
      scrollable: find.byType(Scrollable).first);
  await tester.pumpAndSettle();
}

/// Stands in for the host's stored Dock choice (UserDefaults): it outlives
/// the settings view the way the choice outlives a restart.
void _dockHost(Map<String, bool> stored,
    {bool refuse = false, List<MethodCall>? calls}) {
  TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
      .setMockMethodCallHandler(MacPanel.channel, (call) async {
    calls?.add(call);
    switch (call.method) {
      case 'getShowInDock':
        return stored['ShowInDock'] ?? false;
      case 'setShowInDock':
        if (refuse) return stored['ShowInDock'] ?? false;
        final show = (call.arguments as Map)['show'] as bool;
        stored['ShowInDock'] = show;
        return show;
    }
    return null;
  });
}

Finder _dockSwitch() => find.descendant(
    of: find
        .ancestor(of: find.text('在程序坞中显示 kownload'), matching: find.byType(Row))
        .first,
    matching: find.byType(Switch));

bool _dockShown(WidgetTester tester) =>
    tester.widget<Switch>(_dockSwitch()).value;

void main() {
  setUp(() {
    Get.testMode = true;
    // Settings must never read or write the real browser directories.
    BrowserConnection.routes =
        () async => {'chrome': ManifestRoute.none, 'edge': null};
    BrowserConnection.connect = () async => throw StateError('unexpected');
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async => null);
  });
  tearDown(() {
    BrowserConnection.routes = browserRoutes;
    BrowserConnection.connect = connectBrowsers;
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    MacPanel.settingsSection.value = 'general';
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });

  Future<void> openEditor(WidgetTester tester, String section) async {
    final button = find.byKey(ValueKey('settings-$section'));
    if (button.evaluate().isEmpty) {
      await _reveal(tester, '协议、媒体与代理');
      await tester.tap(find.text('协议、媒体与代理'));
      await tester.pumpAndSettle();
    }
    await tester.scrollUntilVisible(
      button,
      200,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    await tester.ensureVisible(button);
    await tester.pumpAndSettle();
    await tester.tap(button);
    await tester.pumpAndSettle();
    expect(find.byKey(ValueKey('settings-editor-$section')), findsOneWidget);
  }

  Finder field(String label) => find.descendant(
        of: find
            .ancestor(of: find.text(label), matching: find.byType(Row))
            .first,
        matching: find.byType(TextField),
      );

  testWidgets(
    'ordinary navigation opens media editor; cancel keeps config; save preserves unknown protocols',
    (tester) async {
      if (!Platform.isMacOS) return;
      final app = await _mountSettings(tester, 'general', shell: true);
      final protocol = app.downloaderConfig.value.protocolConfig.toJson();
      protocol['futureProvider'] = {
        'value': [1, null, true],
      };
      protocol['media']['futureMedia'] = {'mode': 'keep'};
      app.downloaderConfig.value.protocolConfig = ProtocolConfig.fromJson(
        protocol,
      );
      final before = app.downloaderConfig.value.toJson();
      await tester.tap(find.text('下载').first);
      await tester.pumpAndSettle();
      await openEditor(tester, 'media');
      Finder resolver() => find.byWidgetPredicate(
            (w) => w is TextField && w.controller?.text == '/custom/resolver',
          );
      await tester.enterText(resolver(), '/edited/resolver');
      await tester.pumpAndSettle();
      expect(find.text('yt-dlp：/edited/resolver'), findsOneWidget);
      await tester.tap(find.text('取消').last);
      await tester.pumpAndSettle();
      expect(app.savedConfigs, isEmpty);
      expect(app.downloaderConfig.value.toJson(), before);
      await openEditor(tester, 'media');
      await tester.enterText(resolver(), '/saved/resolver');
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(app.savedConfigs, hasLength(1));
      final saved = app.savedConfigs.single;
      expect(saved['protocolConfig']['media']['ytDlpPath'], '/saved/resolver');
      expect(saved['protocolConfig']['media']['futureMedia'], {'mode': 'keep'});
      expect(saved['protocolConfig']['futureProvider'], {
        'value': [1, null, true],
      });
      expect(saved['protocolConfig']['netdisk'], {'connections': 7});
      expect(saved['protocolConfig']['http']['userAgent'], 'preserved-agent');
      expect(tester.takeException(), isNull);
    },
  );

  for (final section in ['http', 'bt', 'ed2k']) {
    testWidgets(
      'ordinary download navigation edits and saves $section settings',
      (tester) async {
        if (!Platform.isMacOS) return;
        final app = await _mountSettings(tester, 'general', shell: true);
        final before = app.downloaderConfig.value.protocolConfig.toJson();
        await tester.tap(find.text('下载').first);
        await tester.pumpAndSettle();
        await openEditor(tester, section);
        final label = section == 'http'
            ? 'User-Agent'
            : section == 'bt'
                ? 'port'
                : 'ed2kTcpPort';
        await tester.enterText(
          field(label),
          section == 'http' ? 'edited-agent' : '12345',
        );
        await tester.tap(find.text('保存').last);
        await tester.pumpAndSettle();
        final saved = app.savedConfigs.single['protocolConfig'];
        expect(
          saved[section][section == 'http' ? 'userAgent' : 'listenPort'],
          section == 'http' ? 'edited-agent' : 12345,
        );
        expect(saved['media'], before['media']);
        expect(saved['netdisk'], before['netdisk']);
        expect(tester.takeException(), isNull);
      },
    );
  }

  testWidgets(
    'proxy editor saves only its draft and extension management remains reachable',
    (tester) async {
      if (!Platform.isMacOS) return;
      final app = await _mountSettings(tester, 'general', shell: true);
      final original = app.downloaderConfig.value.protocolConfig.toJson();
      await tester.tap(find.text('下载').first);
      await tester.pumpAndSettle();
      await openEditor(tester, 'proxy');
      expect(tester.takeException(), isNull, reason: 'proxy open layout');
      await tester.tap(find.text('proxy').last);
      await tester.pumpAndSettle();
      await tester.tap(find.byType(DropdownButtonFormField<ProxyModeEnum>));
      await tester.pumpAndSettle();
      await tester.tap(find.text('customProxy').last);
      await tester.pumpAndSettle();
      expect(tester.takeException(), isNull, reason: 'custom proxy layout');
      await tester.tap(find.byType(DropdownButtonFormField<String>));
      await tester.pumpAndSettle();
      await tester.tap(find.text('HTTP').last);
      await tester.pumpAndSettle();
      await tester.enterText(
        find.widgetWithText(TextFormField, 'server'),
        '127.0.0.1',
      );
      await tester.pumpAndSettle();
      await tester.enterText(
        find.widgetWithText(TextFormField, 'port'),
        '7890',
      );
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(tester.takeException(), isNull, reason: 'proxy save layout');
      expect(app.savedConfigs.single['proxy']['host'], '127.0.0.1:7890');
      expect(app.savedConfigs.single['protocolConfig'], original);
      final button = find.byKey(const ValueKey('settings-extensions'));
      await tester.scrollUntilVisible(
        button,
        160,
        scrollable: find.byType(Scrollable).first,
      );
      await tester.tap(button);
      await tester.pumpAndSettle();
      expect(find.byType(ExtensionView), findsOneWidget);
      expect(app.savedConfigs, hasLength(1));
      expect(tester.takeException(), isNull);
    },
  );

  testWidgets(
    'failed section save keeps draft for retry and cancel restores live values',
    (tester) async {
      if (!Platform.isMacOS) return;
      final app = await _mountSettings(tester, 'download');
      await openEditor(tester, 'http');
      await tester.enterText(field('User-Agent'), 'retry-agent');
      app.failSave = true;
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(find.text('更改尚未保存，请检查下载服务后重试。'), findsOneWidget);
      expect(
        app.downloaderConfig.value.protocolConfig.http.userAgent,
        'preserved-agent',
      );
      expect(
        find.byWidgetPredicate(
          (w) => w is TextField && w.controller?.text == 'retry-agent',
        ),
        findsOneWidget,
      );
      await tester.tap(find.text('取消').last);
      await tester.pumpAndSettle();
      expect(
        app.downloaderConfig.value.protocolConfig.http.userAgent,
        'preserved-agent',
      );
      expect(app.savedConfigs, hasLength(1));
      await openEditor(tester, 'http');
      await tester.enterText(field('User-Agent'), 'saved-after-retry');
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      app.failSave = false;
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('settings-editor-http')), findsNothing);
      expect(
        app.downloaderConfig.value.protocolConfig.http.userAgent,
        'saved-after-retry',
      );
      expect(app.savedConfigs, hasLength(3));
      expect(tester.takeException(), isNull);
    },
  );

  Future<void> nestedEntry(WidgetTester tester, String value,
      {bool chooseScript = false}) async {
    await tester.tap(find.widgetWithText(OutlinedButton, 'add').last);
    await tester.pumpAndSettle();
    if (chooseScript) {
      await tester.tap(find.byIcon(Icons.folder_open).last);
      await tester.pumpAndSettle();
    } else {
      final nested = find.byType(AlertDialog).last;
      await tester.enterText(
          find
              .descendant(of: nested, matching: find.byType(TextFormField))
              .last,
          value);
    }
    await tester.tap(find.text('confirm').last);
    await tester.pumpAndSettle();
  }

  for (final scheme in ['https', 'future-proxy']) {
    testWidgets('remaining proxy preserves existing $scheme selection',
        (tester) async {
      if (!Platform.isMacOS) return;
      final app = await _mountSettings(tester, 'download');
      app.downloaderConfig.value.proxy
        ..proxyMode = ProxyModeEnum.customProxy
        ..scheme = scheme;
      await openEditor(tester, 'proxy');
      await tester.tap(find.text('proxy').last);
      await tester.pumpAndSettle();
      final dropdown = tester.widget<DropdownButtonFormField<String>>(
          find.byType(DropdownButtonFormField<String>));
      expect(dropdown.initialValue, scheme == 'https' ? scheme : null);
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(app.savedConfigs.single['proxy']['scheme'], scheme);
      expect(app.downloaderConfig.value.proxy.scheme, scheme);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets(
      'remaining settings API saves three fields on the same current start object',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'general', shell: true);
    final current = _PolicyStart()
      ..network = 'tcp'
      ..address = '127.0.0.1:9999'
      ..apiToken = 'fixture-original';
    app.startConfig.value = current;
    final json = app.downloaderConfig.value.protocolConfig.toJson();
    json['futureProvider'] = {
      'unknown': [1, null, 'kept']
    };
    json['http']['futureHttp'] = {'rule': false};
    app.downloaderConfig.value.protocolConfig = ProtocolConfig.fromJson(json);
    final original = app.downloaderConfig.value.toJson();
    await tester.tap(find.text('下载').first);
    await tester.pumpAndSettle();
    await openEditor(tester, 'api');
    await tester.tap(find.text('protocol').last);
    await tester.pumpAndSettle();
    await tester.enterText(
        find.widgetWithText(TextFormField, 'IP'), '127.0.0.2');
    await tester.pumpAndSettle();
    await tester.enterText(find.widgetWithText(TextFormField, 'port'), '0');
    await tester.pumpAndSettle();
    await tester.enterText(field('apiToken'), 'fixture-new');
    expect(tester.widget<TextField>(field('apiToken')).obscureText, isTrue);
    expect(app.savedConfigs, isEmpty);
    expect(current.address, '127.0.0.1:9999');
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(app.savedStarts, isEmpty);
    expect(current.apiToken, 'fixture-original');
    await openEditor(tester, 'api');
    await tester.enterText(field('apiToken'), 'fixture-saved');
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(app.savedStarts, [
      {
        'network': 'tcp',
        'address': '127.0.0.1:9999',
        'apiToken': 'fixture-saved'
      }
    ]);
    expect(identical(app.startConfig.value, current), isTrue);
    expect(current.policy, 'fixture-policy-must-survive');
    expect(app.savedConfigs, isEmpty);
    expect(app.downloaderConfig.value.toJson(), original);
    expect(app.trackerRequests, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining settings API failure restores current values and keeps retry draft',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final current = app.startConfig.value;
    final original = (current.network, current.address, current.apiToken);
    await openEditor(tester, 'api');
    await tester.enterText(field('apiToken'), 'fixture-retry');
    await tester.tap(find.text('protocol').last);
    await tester.pumpAndSettle();
    await tester.enterText(
        find.widgetWithText(TextFormField, 'IP'), '127.0.0.9');
    await tester.pumpAndSettle();
    await tester.enterText(find.widgetWithText(TextFormField, 'port'), '12345');
    await tester.pumpAndSettle();
    await tester.tap(find.byType(DropdownButtonFormField<String>).last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('Unix').last);
    await tester.pumpAndSettle();
    app.failSave = true;
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect((current.network, current.address, current.apiToken), original);
    expect(identical(current, app.startConfig.value), isTrue);
    expect(find.text('启动设置保存未完成，请重试。'), findsOneWidget);
    expect(app.savedStarts.single, {
      'network': 'unix',
      'address': '127.0.0.9:12345',
      'apiToken': 'fixture-retry'
    });
    expect(app.savedConfigs, isEmpty);
    app.failSave = false;
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(current.apiToken, 'fixture-retry');
    expect(current.network, 'unix');
    expect(current.address, '127.0.0.9:12345');
    expect(app.savedStarts, hasLength(2));
    expect(app.savedStarts.last, app.savedStarts.first);
    expect(app.savedConfigs, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining settings Webhook edits stay local and only explicit test invokes its substitute',
      (tester) async {
    if (!Platform.isMacOS) return;
    final calls = <String>[];
    var failWebhook = false;
    final app =
        await _mountSettings(tester, 'download', webhookTest: (url) async {
      calls.add(url);
      if (failWebhook) throw StateError('fixture webhook failed');
    });
    final protocol = app.downloaderConfig.value.protocolConfig.toJson();
    await openEditor(tester, 'webhook');
    await tester.tap(find.text('webhook').last);
    await tester.pumpAndSettle();
    await nestedEntry(tester, 'https://fixture.invalid/hook');
    expect(app.savedConfigs, isEmpty);
    expect(app.downloaderConfig.value.webhook.urls, isEmpty);
    expect(calls, isEmpty);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs, isEmpty);
    await openEditor(tester, 'webhook');
    await tester.tap(find.text('webhook').last);
    await tester.pumpAndSettle();
    await nestedEntry(tester, 'https://fixture.invalid/hook');
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs.single['webhook']['urls'],
        ['https://fixture.invalid/hook']);
    expect(app.savedConfigs.single['protocolConfig'], protocol);
    expect(calls, isEmpty);
    await openEditor(tester, 'webhook');
    await tester.tap(find.text('webhook').last);
    await tester.pumpAndSettle();
    await tester.tap(find.byTooltip('edit').last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('webhookTest').last);
    await tester.pumpAndSettle();
    expect(calls, ['https://fixture.invalid/hook']);
    expect(find.text('webhookTestSuccess'), findsOneWidget);
    failWebhook = true;
    await tester.tap(find.text('webhookTest').last);
    await tester.pumpAndSettle();
    expect(calls, hasLength(2));
    expect(find.text('webhookTestFail'), findsOneWidget);
    await tester.tap(find.text('cancel').last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs, hasLength(1));
    expect(app.trackerRequests, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining settings completion script picker and nested edits never execute or save before apply',
      (tester) async {
    if (!Platform.isMacOS) return;
    var pickerCalls = 0;
    var webhookCalls = 0;
    final app =
        await _mountSettings(tester, 'download', webhookTest: (_) async {
      webhookCalls++;
    });
    final before = app.downloaderConfig.value.protocolConfig.toJson();
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'chooseFiles') {
        pickerCalls++;
        return ['/tmp/fixture-not-executed.sh'];
      }
      return null;
    });
    await openEditor(tester, 'script');
    await tester.tap(find.text('script').last);
    await tester.pumpAndSettle();
    expect(pickerCalls, 0);
    await nestedEntry(tester, '', chooseScript: true);
    expect(pickerCalls, 1);
    expect(app.savedConfigs, isEmpty);
    expect(app.downloaderConfig.value.script.paths, isEmpty);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs, isEmpty);
    await openEditor(tester, 'script');
    await tester.tap(find.text('script').last);
    await tester.pumpAndSettle();
    await nestedEntry(tester, '/tmp/fixture-not-executed.sh');
    await tester.tap(find.byType(Switch).last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs.single['script'], {
      'enable': true,
      'paths': ['/tmp/fixture-not-executed.sh']
    });
    expect(app.savedConfigs.single['protocolConfig'], before);
    expect(webhookCalls, 0);
    expect(app.trackerRequests, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining settings GitHub mirror confirms nested draft then applies only mirror fields',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final before = app.downloaderConfig.value.protocolConfig.toJson();
    await openEditor(tester, 'mirrors');
    await tester.tap(find.text('githubMirror').last);
    await tester.pumpAndSettle();
    await nestedEntry(tester, 'https://fixture.invalid/mirror');
    expect(app.savedConfigs, isEmpty);
    expect(app.downloaderConfig.value.extra.githubMirror.mirrors, isEmpty);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    await openEditor(tester, 'mirrors');
    await tester.tap(find.text('githubMirror').last);
    await tester.pumpAndSettle();
    await nestedEntry(tester, 'https://fixture.invalid/mirror');
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(app.savedConfigs, hasLength(1));
    expect(app.downloaderConfig.value.extra.githubMirror.mirrors.single.url,
        'https://fixture.invalid/mirror');
    expect(app.savedConfigs.single['protocolConfig'], before);
    expect(app.trackerRequests, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining settings BT update uses saved subscriptions and requires its explicit button',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    app.downloaderConfig.value.extra.bt.trackerSubscribeUrls = [
      'https://fixture.invalid/saved-trackers'
    ];
    await openEditor(tester, 'bt');
    await tester.tap(find.text('subscribeTracker').last);
    await tester.pumpAndSettle();
    await tester.tap(find
        .descendant(
            of: find.byKey(const ValueKey('settings-editor-bt')),
            matching: find.byType(Checkbox))
        .first);
    await tester.pumpAndSettle();
    expect(app.trackerRequests, isEmpty);
    expect(app.downloaderConfig.value.extra.bt.trackerSubscribeUrls,
        ['https://fixture.invalid/saved-trackers']);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    await openEditor(tester, 'bt');
    await tester.tap(find.text('保存').last);
    await tester.pumpAndSettle();
    expect(app.trackerRequests, isEmpty);
    final update = find.byKey(const ValueKey('settings-tracker-update'));
    await tester.ensureVisible(update);
    await tester.pumpAndSettle();
    expect(app.trackerRequests, isEmpty);
    await tester.tap(update);
    await tester.pumpAndSettle();
    expect(app.trackerRequests, [
      ['https://fixture.invalid/saved-trackers']
    ]);
    expect(app.savedConfigs, hasLength(1));
    app.failTracker = true;
    await tester.tap(update);
    await tester.pumpAndSettle();
    expect(app.trackerRequests, hasLength(2));
    expect(find.text('未能更新 Tracker，请检查网络后重试。'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'remaining extra API waits for preference persistence before acknowledging',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final pending = Completer<void>();
    app.holdStartSave = pending;
    await openEditor(tester, 'api');
    await tester.enterText(field('apiToken'), 'fixture-pending');
    await tester.tap(find.text('保存').last);
    await tester.pump();
    expect(app.savedStarts, hasLength(1));
    expect(app.savedConfigs, isEmpty);
    expect(find.byKey(const ValueKey('settings-editor-api')), findsOneWidget);
    expect(find.text('正在保存…'), findsOneWidget);
    expect(
        tester
            .widget<TextButton>(find.widgetWithText(TextButton, '取消').last)
            .onPressed,
        isNull);
    pending.complete();
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('settings-editor-api')), findsNothing);
    expect(app.startConfig.value.apiToken, 'fixture-pending');
    expect(app.savedConfigs, isEmpty);
    expect(tester.takeException(), isNull);
  });

  for (final section in ['script', 'webhook', 'mirrors']) {
    testWidgets(
        'remaining extra $section save failure retains draft without executing actions',
        (tester) async {
      if (!Platform.isMacOS) return;
      var webhookCalls = 0;
      final app =
          await _mountSettings(tester, 'download', webhookTest: (_) async {
        webhookCalls++;
      });
      final protocol = app.downloaderConfig.value.protocolConfig.toJson();
      Map<String, dynamic> values() => switch (section) {
            'script' => app.downloaderConfig.value.script.toJson(),
            'webhook' => app.downloaderConfig.value.webhook.toJson(),
            _ => app.downloaderConfig.value.extra.githubMirror.toJson(),
          };
      final before = values();
      await openEditor(tester, section);
      await tester
          .tap(find.text(section == 'mirrors' ? 'githubMirror' : section).last);
      await tester.pumpAndSettle();
      final editor = find.byKey(ValueKey('settings-editor-$section'));
      final toggle =
          find.descendant(of: editor, matching: find.byType(Switch)).last;
      await tester.tap(toggle);
      await tester.pumpAndSettle();
      final draftEnabled = tester.widget<Switch>(toggle).value;
      app.failSave = true;
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(values(), before);
      expect(tester.widget<Switch>(toggle).value, draftEnabled);
      expect(find.text('更改尚未保存，请检查下载服务后重试。'), findsOneWidget);
      expect(webhookCalls, 0);
      expect(app.trackerRequests, isEmpty);
      app.failSave = false;
      await tester.tap(find.text('保存').last);
      await tester.pumpAndSettle();
      expect(app.savedConfigs, hasLength(2));
      expect(app.savedConfigs.last['protocolConfig'], protocol);
      expect(webhookCalls, 0);
      expect(app.trackerRequests, isEmpty);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets(
      'remaining extra mirror deletion still requires confirmation and outer cancel keeps original',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final original = GithubMirror(
        type: GithubMirrorType.jsdelivr,
        url: 'https://fixture.invalid/builtin',
        isBuiltIn: true);
    app.downloaderConfig.value.extra.githubMirror.mirrors = [original];
    await openEditor(tester, 'mirrors');
    await tester.tap(find.text('githubMirror').last);
    await tester.pumpAndSettle();
    await tester.tap(find.byTooltip('delete').last);
    await tester.pumpAndSettle();
    expect(find.text('confirmDelete'), findsOneWidget);
    await tester.tap(find.text('cancel').last);
    await tester.pumpAndSettle();
    expect(find.text('https://fixture.invalid/builtin'), findsOneWidget);
    await tester.tap(find.byTooltip('delete').last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('confirm').last);
    await tester.pumpAndSettle();
    expect(find.text('https://fixture.invalid/builtin'), findsNothing);
    expect(original.isDeleted, isFalse);
    expect(app.savedConfigs, isEmpty);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(
        identical(app.downloaderConfig.value.extra.githubMirror.mirrors.single,
            original),
        isTrue);
    expect(original.isDeleted, isFalse);
    expect(app.trackerRequests, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets('general offers ordinary preferences and the shared guide',
      (tester) async {
    if (!Platform.isMacOS) return;
    await _mountSettings(tester, 'general');
    expect(find.byType(MacHotKeyEditor), findsOneWidget);
    expect(find.text('登录项…'), findsOneWidget);
    expect(find.text('保存位置'), findsNothing);
    await _reveal(tester, '外观');
    expect(find.text('语言'), findsOneWidget);
    await _reveal(tester, '重新打开…');
    expect(find.text('版本 test（1）'), findsOneWidget);
    expect(find.byType(ExtensionView), findsNothing);
    expect(tester.takeException(), isNull);
  });

  testWidgets('browser connection asks first, then switches and refreshes',
      (tester) async {
    if (!Platform.isMacOS) return;
    var calls = 0;
    var connected = false;
    BrowserConnection.routes = () async => {
          'chrome': connected ? ManifestRoute.thisApp : ManifestRoute.elsewhere,
          'edge': null,
        };
    BrowserConnection.connect = () async {
      calls++;
      connected = true;
      return ['chrome'];
    };
    await _mountSettings(tester, 'general');
    await _reveal(tester, '浏览器连接');
    expect(find.text('Chrome：指向其他程序（如旧版） · Edge：未检测到'), findsOneWidget);
    await tester.tap(find.text('连接…'));
    await tester.pumpAndSettle();
    expect(find.text('连接浏览器助手'), findsOneWidget);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(calls, 0);
    await tester.tap(find.text('连接…'));
    await tester.pumpAndSettle();
    await tester.tap(find.widgetWithText(TextButton, '连接'));
    await tester.pumpAndSettle();
    expect(calls, 1);
    expect(find.text('Chrome：已连接本应用 · Edge：未检测到\n已连接，下次在网页上点击下载时生效。'),
        findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'browser preferences retain each profile and save independently of engine config',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'general');
    await app.browserIntegration.command({
      'action': 'sync',
      'clientId': 'chrome-one',
      'browser': 'chrome',
      'downloadsPermission': true,
      'version': '0.4.3',
      'legacySettings': {
        'downloadCapture': {'enabled': true, 'confirm': false},
        'showAll': true
      },
    });
    await app.browserIntegration.command({
      'action': 'sync',
      'clientId': 'edge-one',
      'browser': 'edge',
      'legacySettings': {
        'downloadCapture': {'enabled': false, 'confirm': true},
        'showAll': false
      },
    });
    await _reveal(tester, '普通下载');
    DropdownButton<String> mode() =>
        tester.widget<DropdownButton<String>>(find.byWidgetPredicate((w) =>
            w is DropdownButton<String> &&
            w.items!.any((i) => i.value == 'confirm')));
    expect(mode().value, 'direct');
    mode().onChanged!('confirm');
    await tester.pumpAndSettle();
    expect(app.browserIntegration.clients.first.preferences.confirmDownloads,
        true);
    expect(
        app.browserIntegration.clients.first.preferences.captureEnabled, true);
    expect(app.browserIntegration.clients.first.preferences.showAll, true);
    final browsers = tester.widget<DropdownButton<String>>(
        find.byWidgetPredicate(
            (w) => w is DropdownButton<String> && w.value == 'chrome-one'));
    browsers.onChanged!('edge-one');
    await tester.pumpAndSettle();
    expect(mode().value, 'browser');
    expect(app.browserIntegration.clients.last.preferences.showAll, false);
    expect(app.savedConfigs, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets('dock switch starts off and applies each change through the host',
      (tester) async {
    if (!Platform.isMacOS) return;
    final stored = <String, bool>{};
    final calls = <MethodCall>[];
    _dockHost(stored, calls: calls);
    final app = await _mountSettings(tester, 'general');
    expect(_dockShown(tester), isFalse);
    expect(find.text('关闭时只在菜单栏显示，不出现在程序坞和 ⌘Tab 中。'), findsOneWidget);
    await tester.tap(_dockSwitch());
    await tester.pumpAndSettle();
    expect(_dockShown(tester), isTrue);
    expect(stored['ShowInDock'], isTrue);
    await tester.tap(_dockSwitch());
    await tester.pumpAndSettle();
    expect(_dockShown(tester), isFalse);
    expect(stored['ShowInDock'], isFalse);
    expect(
        calls
            .where((c) => c.method == 'setShowInDock')
            .map((c) => (c.arguments as Map)['show']),
        [true, false]);
    // The choice belongs to the host, not the download service's settings.
    expect(app.savedConfigs, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets('a stored dock choice is shown again after a restart',
      (tester) async {
    if (!Platform.isMacOS) return;
    final stored = <String, bool>{'ShowInDock': true};
    _dockHost(stored);
    await _mountSettings(tester, 'general');
    expect(_dockShown(tester), isTrue);
    await tester.tap(_dockSwitch());
    await tester.pumpAndSettle();
    expect(stored['ShowInDock'], isFalse);
    // Remount everything the way a new launch does; only the host keeps state.
    await tester.pumpWidget(const SizedBox());
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    await _mountSettings(tester, 'general');
    expect(_dockShown(tester), isFalse);
    expect(tester.takeException(), isNull);
  });

  testWidgets('a refused or unavailable dock change keeps the actual state',
      (tester) async {
    if (!Platform.isMacOS) return;
    final stored = <String, bool>{};
    _dockHost(stored, refuse: true);
    await _mountSettings(tester, 'general');
    await tester.tap(_dockSwitch());
    await tester.pumpAndSettle();
    expect(_dockShown(tester), isFalse);
    expect(find.text('没有切换成功，请重试。'), findsOneWidget);
    expect(stored, isEmpty);
    // A host that cannot answer at all leaves the default: off.
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async => null);
    await tester.tap(_dockSwitch());
    await tester.pumpAndSettle();
    expect(_dockShown(tester), isFalse);
    expect(find.text('没有切换成功，请重试。'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  for (final legacy in ['connections', 'advanced']) {
    testWidgets('$legacy links expose existing advanced settings in download',
        (tester) async {
      if (!Platform.isMacOS) return;
      final app = await _mountSettings(tester, legacy);
      expect(find.text('保存位置'), findsOneWidget);
      await openEditor(tester, 'media');
      await tester.tap(find.text('取消').last);
      await tester.pumpAndSettle();
      expect(find.byType(ExtensionView), findsNothing);
      await openEditor(tester, 'api');
      await tester.tap(find.text('取消').last);
      await tester.pumpAndSettle();
      expect(app.savedConfigs, isEmpty);
      expect(app.savedStarts, isEmpty);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets(
      'reopened guide synchronizes its saved directory back to settings',
      (tester) async {
    if (!Platform.isMacOS) return;
    MacOnboarding.checksOverride = FixtureOnboardingChecks();
    addTearDown(() => MacOnboarding.checksOverride = null);
    final app = await _mountSettings(tester, 'general');
    await Database.instance.box.put(MacOnboarding.completedKey, true);
    await _reveal(tester, '重新打开…');
    await tester.tap(find.text('重新打开…'));
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsOneWidget);
    await tester.enterText(
        find.byKey(const ValueKey('onboarding-directory')), '/guide-downloads');
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsNothing);
    MacPanel.settingsSection.value = 'download';
    await tester.pumpAndSettle();
    expect(
        tester
            .widget<TextFormField>(find.byType(TextFormField))
            .controller!
            .text,
        '/guide-downloads');
    expect(app.savedConfigs.last['downloadDir'], '/guide-downloads');
    expect(tester.takeException(), isNull);
  });

  testWidgets('download saves everyday choices and preserves engine config',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    expect(find.text('保存位置'), findsOneWidget);
    expect(find.byType(MacHotKeyEditor), findsNothing);
    expect(find.text('User-Agent'), findsNothing);
    expect(find.text('HTTP'), findsNothing);
    expect(find.text('BitTorrent'), findsNothing);
    expect(find.text('ed2k'), findsNothing);
    await tester.tap(find.byType(DropdownButton<int>));
    await tester.pumpAndSettle();
    await tester.tap(find.text('4 个').last);
    await tester.pumpAndSettle();
    expect(app.downloaderConfig.value.maxRunning, 4);
    await _reveal(tester, '自动解压压缩包');
    await tester.tap(find.descendant(
        of: find
            .ancestor(of: find.text('自动解压压缩包'), matching: find.byType(Row))
            .first,
        matching: find.byType(Switch)));
    await tester.pumpAndSettle();
    expect(app.downloaderConfig.value.archive.autoExtract, isTrue);
    expect(find.text('解压成功后删除压缩包'), findsOneWidget);
    final saved = app.savedConfigs.last;
    expect(saved['maxRunning'], 4);
    expect(saved['archive']['autoExtract'], isTrue);
    expect(saved['protocolConfig']['media']['ytDlpPath'], '/custom/resolver');
    expect(saved['protocolConfig']['http']['userAgent'], 'preserved-agent');
    expect(saved['protocolConfig']['netdisk'], {'connections': 7});
    await _reveal(tester, '已保存');
    expect(find.text('已保存'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets('directory edits save, empty paths keep the previous location',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final path = find.byType(TextFormField);
    await tester.enterText(path, '/new-downloads');
    await tester.testTextInput.receiveAction(TextInputAction.done);
    await tester.pumpAndSettle();
    expect(app.savedConfigs.last['downloadDir'], '/new-downloads');
    await tester.enterText(path, '  ');
    await tester.testTextInput.receiveAction(TextInputAction.done);
    await tester.pumpAndSettle();
    expect(find.text('请选择保存位置。'), findsOneWidget);
    expect(app.downloaderConfig.value.downloadDir, '/new-downloads');
    expect(app.savedConfigs.length, 1);
    expect(tester.takeException(), isNull);
  });

  testWidgets('failed saves provide a retry that writes the changed value',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    app.failSave = true;
    // The compact visual still accepts clicks across the full control target.
    await tester.tapAt(
        tester.getCenter(find.byType(Switch).first) + const Offset(20, 0));
    await tester.pumpAndSettle();
    expect(find.text('更改尚未保存，请检查下载服务后重试。'), findsOneWidget);
    expect(find.text('已保存'), findsNothing);
    app.failSave = false;
    await tester.tap(find.text('重试'));
    await tester.pumpAndSettle();
    expect(app.savedConfigs.length, 2);
    expect(app.savedConfigs.last['extra']['defaultDirectDownload'], isTrue);
    await _reveal(tester, '已保存');
    expect(find.text('已保存'), findsOneWidget);
    expect(find.text('重试'), findsNothing);
    expect(tester.takeException(), isNull);
  });

  testWidgets('rapid changes serialize saves and retain the latest choices',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'download');
    final hold = Completer<void>();
    app.holdSave = hold;
    await tester.tap(find.byType(Switch).first);
    await tester.pump();
    await tester.tap(find.byType(Switch).at(1));
    await tester.pump();
    expect(app.savedConfigs.length, 1);
    await _reveal(tester, '正在保存…');
    expect(find.text('正在保存…'), findsOneWidget);
    hold.complete();
    await tester.pumpAndSettle();
    expect(app.savedConfigs.length, 2);
    expect(app.savedConfigs.last['extra']['defaultDirectDownload'], isTrue);
    expect(app.savedConfigs.last['extra']['autoStartTasks'], isTrue);
    await _reveal(tester, '已保存');
    expect(find.text('已保存'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'shell has three ordinary destinations and returns through MacPanel',
      (tester) async {
    final calls = <MethodCall>[];
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      calls.add(call);
      return null;
    });
    addTearDown(() => tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null));
    await tester.binding.setSurfaceSize(const Size(640, 400));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    await tester.pumpWidget(GetMaterialApp.router(
      theme: macTheme(Brightness.light),
      getPages: [
        GetPage(
            name: '/',
            page: () =>
                const MacShell(selectedIndex: 0, child: Text('task child'))),
        GetPage(
            name: Routes.SETTING,
            page: () => const MacShell(
                selectedIndex: 2, child: Text('settings child'))),
        GetPage(
            name: Routes.TASK,
            page: () =>
                const MacShell(selectedIndex: 0, child: Text('task child'))),
      ],
    ));
    await tester.pumpAndSettle();
    for (final label in ['下载列表', '通用', '下载']) {
      expect(find.text(label), findsOneWidget);
    }
    expect(find.text('task child'), findsOneWidget);
    expect(find.text('资料库'), findsNothing);
    expect(find.text('extensions'), findsNothing);
    await tester.tap(find.text('通用'));
    await tester.pumpAndSettle();
    expect(MacPanel.settingsSection.value, 'general');
    expect(find.text('settings child'), findsOneWidget);
    expect(find.text('功能'), findsNothing);
    expect(find.text('高级'), findsNothing);
    await tester.tap(find.text('下载'));
    await tester.pumpAndSettle();
    expect(MacPanel.settingsSection.value, 'download');
    await tester.tap(find.text('下载列表'));
    await tester.pumpAndSettle();
    expect(Get.rootDelegate.currentConfiguration?.uri.path, Routes.TASK);
    expect(find.text('task child'), findsOneWidget);
    final searches = MacAppearance.searchRequests.value;
    MacAppearance.onCommand?.call('settings');
    await tester.pumpAndSettle();
    expect(MacPanel.settingsSection.value, 'general');
    MacAppearance.onCommand?.call('search');
    await tester.pumpAndSettle();
    expect(Get.rootDelegate.currentConfiguration?.uri.path, Routes.TASK);
    expect(MacAppearance.searchRequests.value, searches + 1);
    MacAppearance.onCommand?.call('newTask');
    await tester.pumpAndSettle();
    expect(MacPanel.presentation.value, 'panel');
    await tester.tap(find.byTooltip('返回轻面板'));
    await tester.pumpAndSettle();
    final show = calls.lastWhere((c) => c.method == 'setPresentation');
    expect(show.arguments['mode'], 'panel');
    expect(show.arguments['show'], true);
    expect(tester.takeException(), isNull);
  });
}
