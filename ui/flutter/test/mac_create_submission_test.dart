import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_onboarding.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
  @override
  Future<void> saveConfig() async {}
}

class _Box extends Fake implements Box {
  bool onboardingCompleted = false;
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      key == MacOnboarding.completedKey ? onboardingCompleted : defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    if (key == MacOnboarding.completedKey) onboardingCompleted = value == true;
  }
}

void main() {
  testWidgets(
      'successful capture clears authentication for the next site and advanced errors stay visible',
      (tester) async {
    if (!Platform.isMacOS) return;
    final requests = <Map<String, dynamic>>[];
    var reject = false;
    Future<String> submitTask(CreateTask request) async {
      requests.add(jsonDecode(jsonEncode(request.toJson())));
      if (reject) throw StateError('测试下载失败');
      return 'created';
    }

    Database.instance.box = _Box();
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    app.downloaderConfig.value.extra.defaultDirectDownload = true;
    Get.put<AppController>(app);
    Get.put(CreateController());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = CreateTask(
        req: Request(
            url: 'https://site-a.example/file',
            proxy: RequestProxy(
                mode: RequestProxyMode.custom,
                scheme: 'socks5',
                host: '127.0.0.1:1080'),
            extra: {
          'header': {
            'Cookie': 'fixture-secret',
            'Referer': 'https://site-a.example/'
          }
        }));
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 760));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.cancelAutoHide();
      MacPanel.incoming.value = null;
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(GetMaterialApp.router(
        theme: macTheme(Brightness.light),
        getPages: [
          GetPage(name: '/', page: () => CreateView(taskCreator: submitTask))
        ]));
    await tester.pumpAndSettle();
    Future<void> submit(Finder button) async {
      final expected = requests.length + 1;
      await tester.pump();
      await tester.tap(button);
      await tester.pumpAndSettle();
      expect(requests.length, expected);
      await tester.pumpAndSettle();
      MacPanel.cancelAutoHide();
    }

    await tester.tap(find.byTooltip('开始下载'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    expect(find.text('下载新手教程'), findsOneWidget);
    expect(requests, isEmpty);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(find.text('https://site-a.example/file'), findsOneWidget);
    expect(requests, isEmpty);
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 300));
    await tester.enterText(find.byKey(const ValueKey('onboarding-directory')),
        '/tmp/first-download');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    MacPanel.cancelAutoHide();
    expect(requests, hasLength(1));
    expect(requests.single['opts']['path'], '/tmp/first-download');
    expect(find.text('下载新手教程'), findsNothing);
    expect(
        requests.single['req']['extra']['header']['Cookie'], 'fixture-secret');
    expect(requests.single['req']['proxy']['scheme'], 'socks5');
    expect(requests.single['req']['proxy']['host'], '127.0.0.1:1080');
    await tester.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://site-b.example/file');
    await submit(find.byTooltip('开始下载'));
    expect(jsonEncode(requests.last), isNot(contains('fixture-secret')));
    expect(jsonEncode(requests.last), isNot(contains('site-a.example')));
    await tester.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://site-c.example/file');
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    reject = true;
    await submit(find.text('confirm'));
    expect(find.textContaining('测试下载失败'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });
  for (final customPath in <String?>[null, '/tmp/custom-draft']) {
    testWidgets(
        'persistent panel follows changed Settings default while preserving custom path $customPath',
        (tester) async {
      if (!Platform.isMacOS) return;
      final requests = <CreateTask>[];
      Database.instance.box = _Box()..onboardingCompleted = true;
      final app = _App();
      app.downloaderConfig.value.downloadDir = '/tmp/old-default';
      app.downloaderConfig.value.extra.defaultDirectDownload = true;
      Get.put<AppController>(app);
      Get.put(CreateController());
      MacPanel.presentation.value = 'panel';
      MacPanel.incoming.value = CreateTask(
          req: Request(url: 'https://example.com/draft.zip'),
          opts: customPath == null ? null : Options(path: customPath));
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
      await tester.binding.setSurfaceSize(const Size(640, 760));
      addTearDown(() async {
        await tester.pumpWidget(const SizedBox.shrink());
        MacPanel.cancelAutoHide();
        MacPanel.incoming.value = null;
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(MacPanel.channel, null);
        Get.reset();
        Get.routerDelegate = null;
        Get.routeInformationParser = null;
        await tester.binding.setSurfaceSize(null);
      });
      // Keep the exact same CreateView instance offstage, as RootView does.
      final panel = CreateView(taskCreator: (request) async {
        requests.add(request);
        return 'created';
      });
      await tester.pumpWidget(
          GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
        GetPage(
            name: '/',
            page: () => ValueListenableBuilder<String>(
                valueListenable: MacPanel.presentation,
                builder: (_, mode, __) => Stack(children: [
                      Offstage(offstage: mode == 'settings', child: panel),
                      if (mode == 'settings') const Text('settings'),
                    ])))
      ]));
      await tester.pumpAndSettle();
      final originalState = tester.state(find.byType(CreateView));
      MacPanel.presentation.value = 'settings';
      await tester.pumpAndSettle();
      app.downloaderConfig.value.downloadDir = '/tmp/new-default';
      app.downloaderConfig.refresh();
      await tester.pumpAndSettle();
      MacPanel.presentation.value = 'panel';
      await tester.pumpAndSettle();
      expect(tester.state(find.byType(CreateView)), same(originalState));
      expect(find.text('https://example.com/draft.zip'), findsOneWidget);
      await tester.tap(find.byTooltip('开始下载'));
      await tester.pumpAndSettle();
      MacPanel.cancelAutoHide();
      expect(requests, hasLength(1));
      expect(requests.single.opts!.path, customPath ?? '/tmp/new-default');
      expect(find.text('下载新手教程'), findsNothing);
      expect(tester.takeException(), isNull);
    });
  }
  testWidgets(
      'typing only converts a complete 40-digit hexadecimal hash to magnet',
      (tester) async {
    if (!Platform.isMacOS) return;
    Database.instance.box = _Box()..onboardingCompleted = true;
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    Get.put<AppController>(app);
    Get.put(CreateController());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 400));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.cancelAutoHide();
      MacPanel.incoming.value = null;
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(GetMaterialApp.router(
        theme: macTheme(Brightness.light),
        getPages: [GetPage(name: '/', page: () => const CreateView())]));
    await tester.pumpAndSettle();
    final input = find.byKey(const ValueKey('panel-download-input'));
    String currentText() =>
        tester.widget<TextFormField>(input).controller!.text;
    const url = 'https://example.com/download/archive-2026.zip';
    expect(url.length, greaterThan(40));
    for (var length = 1; length <= url.length; length++) {
      final prefix = url.substring(0, length);
      await tester.enterText(input, prefix);
      await tester.pump();
      expect(currentText(), prefix,
          reason: 'URL prefix of length $length must remain unchanged');
    }
    const hash = '0123456789abcdef0123456789ABCDEF01234567';
    expect(hash.length, 40);
    await tester.enterText(input, hash);
    await tester.pump();
    expect(currentText(), 'magnet:?xt=urn:btih:$hash');
    final nonHex = '${hash.substring(0, 39)}z';
    await tester.enterText(input, nonHex);
    await tester.pump();
    expect(currentText(), nonHex);
    expect(tester.takeException(), isNull);
  });
}
