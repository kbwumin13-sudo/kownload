import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/home/views/home_view.dart';
import 'package:gopeed/app/modules/root/views/root_view.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/api/model/task.dart' show Protocol;

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  setUp(() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
  });
  tearDown(() {
    MacPanel.cancelAutoHide();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });
  testWidgets(
      'settings retains the panel draft and advanced options remain reachable',
      (tester) async {
    if (!Platform.isMacOS) return;
    await tester.binding.setSurfaceSize(const Size(640, 700));
    MacPanel.incoming.value = null;
    MacPanel.presentation.value = 'panel';
    addTearDown(() async {
      MacPanel.cancelAutoHide();
      await tester.pumpWidget(const SizedBox.shrink());
      await tester.binding.setSurfaceSize(null);
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
    });
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    Get.put<AppController>(app);
    await tester.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: Routes.ROOT,
          participatesInRootNavigator: true,
          page: () => const RootView(),
          children: [
            GetPage(
                name: Routes.HOME,
                transition: Transition.noTransition,
                page: () => const HomeView(),
                children: [
                  GetPage(
                      name: '/task', page: () => const Text('task content')),
                  GetPage(
                      name: '/setting',
                      page: () => const Text('settings content')),
                  GetPage(
                      name: '/extension',
                      page: () => const Text('extension content')),
                ]),
          ]),
    ]));
    await tester.pumpAndSettle();
    final input = find.byKey(const ValueKey('panel-download-input'));
    expect(input, findsOneWidget);
    await tester.enterText(input, 'https://example.com/待下载.bin');
    await MacPanel.showSettings();
    await tester.pumpAndSettle();
    expect(find.text('settings content'), findsOneWidget);
    final settingsBounds = tester.getRect(find.text('settings content'));
    expect(settingsBounds.top, greaterThanOrEqualTo(0));
    expect(settingsBounds.bottom, lessThanOrEqualTo(700));
    expect(input, findsNothing);
    await MacPanel.showPanel();
    await tester.pumpAndSettle();
    expect(find.text('https://example.com/待下载.bin'), findsOneWidget);
    await MacPanel.showSettings();
    await tester.pumpAndSettle();
    await MacPanel.showPanel();
    await tester.pumpAndSettle();
    expect(find.text('https://example.com/待下载.bin'), findsOneWidget);
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    expect(find.text('confirm'), findsOneWidget);
    await tester.tap(find.text('cancel'));
    await tester.pumpAndSettle();
    expect(input, findsOneWidget);
    expect(find.text('https://example.com/待下载.bin'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  test('create recognizes HTTPS, magnet, torrent and ed2k inputs', () {
    final view = CreateView();
    expect(view.parseProtocol('https://example.com/file.bin'), Protocol.http);
    expect(
        view.parseProtocol(
            'magnet:?xt=urn:btih:0123456789012345678901234567890123456789'),
        Protocol.bt);
    expect(view.parseProtocol('/tmp/example.torrent'), Protocol.bt);
    expect(view.parseProtocol('ed2k://|file|test|1|hash|/'), Protocol.ed2k);
  });
}
