import 'dart:io';

import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:gopeed/api/model/extension.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/extension/controllers/extension_controller.dart';
import 'package:gopeed/app/modules/extension/views/extension_view.dart';
import 'package:gopeed/app/modules/setting/controllers/setting_controller.dart';
import 'package:gopeed/app/modules/setting/views/setting_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_appearance.dart';
import 'package:gopeed/macos/mac_shell.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/package_info.dart';
import 'package:gopeed/util/util.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Settings extends SettingController {
  @override
  void fetchLatestVersion() {}
}

class _Box extends Fake implements Box {
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) => defaultValue;
}

class _Extensions extends ExtensionController {
  int stores = 0;
  int loads = 0;
  int updates = 0;
  // Fixture skips network initialization; actions are exercised explicitly.
  @override
  // ignore: must_call_super
  Future<void> onInit() async {}
  @override
  Future<void> loadInstalled({bool refreshUpdates = false}) async {
    loads++;
    if (refreshUpdates) updates++;
  }

  @override
  Future<void> refreshStore() async {
    stores++;
  }
}

Future<_Extensions> _mountSettings(WidgetTester tester, String section) async {
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
  app.downloaderConfig.value.extra.themeMode = 'system';
  app.downloaderConfig.value.extra.locale = 'en_US';
  app.startConfig.value.network = 'tcp';
  app.startConfig.value.address = '127.0.0.1:9999';
  app.startConfig.value.apiToken = '';
  Get.put<AppController>(app);
  Get.put<SettingController>(_Settings());
  final extensions = Get.put<ExtensionController>(_Extensions()) as _Extensions;
  MacPanel.settingsSection.value = section;
  await tester.pumpWidget(
      GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
    GetPage(name: '/', page: () => const SettingView()),
    GetPage(name: Routes.SETTING, page: () => const SettingView()),
  ]));
  await tester.pumpAndSettle();
  return extensions;
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
  });
  tearDown(() {
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    MacPanel.settingsSection.value = 'general';
  });

  testWidgets(
      'general keeps UI and about settings, fixed menu bar entry and shortcut editor',
      (tester) async {
    if (!Platform.isMacOS) return;
    await _mountSettings(tester, 'general');
    expect(find.byType(MacHotKeyEditor), findsOneWidget);
    final menubar =
        tester.widget<ListTile>(find.widgetWithText(ListTile, '菜单栏入口'));
    expect((menubar.trailing! as Switch).value, isTrue);
    expect((menubar.trailing! as Switch).onChanged, isNull);
    expect(find.textContaining('kownload 常驻菜单栏'), findsOneWidget);
    expect(find.text('downloadDir'), findsNothing);
    await _reveal(tester, 'theme');
    expect(find.text('locale'), findsOneWidget);
    await _reveal(tester, 'version');
    expect(find.text('test'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'download keeps directory, archive, HTTP, media, BT and ed2k settings',
      (tester) async {
    if (!Platform.isMacOS) return;
    await _mountSettings(tester, 'download');
    expect(find.text('downloadDir'), findsOneWidget);
    expect(find.byType(MacHotKeyEditor), findsNothing);
    for (final label in [
      'autoExtract',
      'User-Agent',
      '媒体下载',
      'BitTorrent',
      'ed2k'
    ]) {
      // Locale is English in these tests, so media uses the existing English label.
      await _reveal(tester, label == '媒体下载' ? 'Media downloads' : label);
    }
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'connections shows installed status and config without maintenance or store',
      (tester) async {
    if (!Platform.isMacOS) return;
    final controller = await _mountSettings(tester, 'connections');
    final extension = Extension(
        identity: 'sample',
        name: 'sample',
        author: 'Example',
        title: 'Sample capability',
        description: '',
        icon: '',
        version: '1.0.0',
        homepage: '',
        repository: null,
        disabled: false,
        devMode: false,
        devPath: '');
    extension.settings = [
      Setting(
          name: 'token',
          title: 'Token',
          description: '',
          required: false,
          type: SettingType.string)
    ];
    controller.installedExtensions.assignAll([extension]);
    await _reveal(tester, 'Sample capability');
    expect(find.text('v1.0.0 · 已启用'), findsOneWidget);
    expect(find.byIcon(CupertinoIcons.gear), findsOneWidget);
    expect(find.byIcon(CupertinoIcons.trash), findsNothing);
    expect(find.text('打开扩展商店'), findsNothing);
    expect(find.text('检查扩展更新'), findsNothing);
    expect(controller.stores, 0);
    expect(controller.updates, 0);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'advanced loads maintenance only after expansion and store only after a click',
      (tester) async {
    if (!Platform.isMacOS) return;
    final directory =
        Directory.systemTemp.createTempSync('kownload-settings-test-');
    addTearDown(() => directory.deleteSync(recursive: true));
    await tester
        .runAsync(() => Util.initStorageDir(previewStorage: directory.path));
    final controller = await _mountSettings(tester, 'advanced');
    expect(find.byType(ExtensionView), findsNothing);
    expect(controller.stores, 0);
    await tester.tap(find.text('扩展安装与维护'));
    await tester.pumpAndSettle();
    expect(find.byType(ExtensionView), findsOneWidget);
    expect(find.text('打开扩展商店'), findsOneWidget);
    expect(controller.stores, 0);
    await tester.tap(find.text('检查扩展更新'));
    await tester.pumpAndSettle();
    expect(controller.updates, 1);
    await tester.tap(find.text('打开扩展商店'));
    await tester.pumpAndSettle();
    expect(controller.stores, 1);
    expect(find.text('返回已安装'), findsOneWidget);
    await tester.tap(find.text('返回已安装'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('扩展安装与维护'));
    await tester.pumpAndSettle();
    expect(find.byType(ExtensionView), findsNothing);
    await _reveal(tester, 'API');
    await _reveal(tester, 'logDirectory');
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'shell has five top categories, preserves task child and returns through MacPanel',
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
    for (final label in ['任务', '通用', '下载', '连接与能力', '高级']) {
      expect(find.text(label), findsOneWidget);
    }
    expect(find.text('task child'), findsOneWidget);
    expect(find.text('资料库'), findsNothing);
    expect(find.text('extensions'), findsNothing);
    await tester.tap(find.text('通用'));
    await tester.pumpAndSettle();
    expect(MacPanel.settingsSection.value, 'general');
    expect(find.text('settings child'), findsOneWidget);
    await tester.tap(find.text('高级'));
    await tester.pumpAndSettle();
    expect(MacPanel.settingsSection.value, 'advanced');
    await tester.tap(find.text('任务'));
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
