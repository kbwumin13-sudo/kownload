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
import 'package:gopeed/app/modules/extension/views/extension_view.dart';
import 'package:gopeed/app/modules/setting/controllers/setting_controller.dart';
import 'package:gopeed/app/modules/setting/views/setting_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_appearance.dart';
import 'package:gopeed/macos/mac_shell.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/package_info.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}

  bool failSave = false;
  Completer<void>? holdSave;
  final savedConfigs = <Map<String, dynamic>>[];

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

Future<_App> _mountSettings(WidgetTester tester, String section) async {
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
  await tester.pumpWidget(
      GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
    GetPage(name: '/', page: () => const SettingView()),
    GetPage(name: Routes.SETTING, page: () => const SettingView()),
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

void main() {
  setUp(() {
    Get.testMode = true;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async => null);
  });
  tearDown(() {
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    MacPanel.settingsSection.value = 'general';
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
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
    expect(find.text('版本 test'), findsOneWidget);
    expect(find.byType(ExtensionView), findsNothing);
    expect(tester.takeException(), isNull);
  });

  for (final legacy in ['connections', 'advanced']) {
    testWidgets('$legacy links land on general preferences', (tester) async {
      if (!Platform.isMacOS) return;
      await _mountSettings(tester, legacy);
      expect(find.text('使用习惯'), findsOneWidget);
      expect(find.byType(MacHotKeyEditor), findsOneWidget);
      expect(find.byType(ExtensionView), findsNothing);
      expect(find.text('API'), findsNothing);
      expect(find.textContaining('扩展'), findsNothing);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets(
      'reopened guide synchronizes its saved directory back to settings',
      (tester) async {
    if (!Platform.isMacOS) return;
    final app = await _mountSettings(tester, 'general');
    await Database.instance.box.put(MacOnboarding.completedKey, true);
    await _reveal(tester, '重新打开…');
    await tester.tap(find.text('重新打开…'));
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsOneWidget);
    await tester.enterText(
        find.byKey(const ValueKey('onboarding-directory')), '/guide-downloads');
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
    expect(find.text('正在保存…'), findsOneWidget);
    hold.complete();
    await tester.pumpAndSettle();
    expect(app.savedConfigs.length, 2);
    expect(app.savedConfigs.last['extra']['defaultDirectDownload'], isTrue);
    expect(app.savedConfigs.last['extra']['autoStartTasks'], isTrue);
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
