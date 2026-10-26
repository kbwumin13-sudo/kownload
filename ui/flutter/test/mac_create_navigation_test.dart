import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/home/views/home_view.dart';
import 'package:gopeed/app/modules/root/views/root_view.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

// Keep fixtures static; no API polling or polling timer is created.
class _Tasks extends MacTaskListController {
  @override
  // ignore: must_call_super
  void onInit() {}
  @override
  void onClose() {}
}

Task _task(String id, Status status) => Task(
    id: id,
    name: '$id.zip',
    meta:
        Meta(req: Request(url: 'https://example.com/$id.zip'), opts: Options()),
    status: status,
    uploading: false,
    progress: Progress(
        used: 0, speed: 1024, downloaded: 2048, uploadSpeed: 0, uploaded: 0),
    createdAt: DateTime(2026),
    updatedAt: DateTime(2026));

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

  testWidgets(
      'compact panel filters real tasks and limits each preview to three',
      (tester) async {
    if (!Platform.isMacOS) return;
    await tester.binding.setSurfaceSize(const Size(640, 380));
    MacPanel.incoming.value = null;
    MacPanel.presentation.value = 'panel';
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    Get.put<AppController>(app);
    Get.put(CreateController());
    final tasks = _Tasks();
    tasks.tasks.assignAll([
      _task('running', Status.running),
      _task('paused', Status.pause),
      _task('failed', Status.error),
      _task('waiting', Status.wait),
      _task('finished', Status.done),
    ]);
    Get.put<MacTaskListController>(tasks);
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(GetMaterialApp.router(
        theme: macTheme(Brightness.dark),
        getPages: [GetPage(name: '/', page: () => const CreateView())]));
    await tester.pumpAndSettle();
    expect(find.text('进行中 4'), findsOneWidget);
    expect(find.text('已完成 1'), findsOneWidget);
    expect(find.text('running.zip'), findsOneWidget);
    expect(find.text('failed.zip'), findsOneWidget);
    expect(find.text('waiting.zip'), findsNothing);
    expect(find.text('finished.zip'), findsNothing);
    await tester.tap(find.text('已完成 1'));
    await tester.pumpAndSettle();
    expect(find.text('finished.zip'), findsOneWidget);
    expect(find.text('running.zip'), findsNothing);
    expect(find.byTooltip('继续'), findsNothing);
    tasks.tasks.removeWhere((task) => task.status == Status.done);
    await tester.pumpAndSettle();
    expect(find.text('已完成 0'), findsOneWidget);
    expect(find.text('暂无已完成的下载'), findsOneWidget);
    await tester.enterText(find.byKey(const ValueKey('panel-download-input')),
        '草稿 https://example.com/file');
    await tester.pump();
    await tester.tap(find.byTooltip('清空'));
    await tester.pumpAndSettle();
    final field = tester.widget<TextFormField>(
        find.byKey(const ValueKey('panel-download-input')));
    expect(field.controller!.text, isEmpty);
    expect(tester.takeException(), isNull);
  });

  test('create recognizes HTTPS, magnet, torrent and ed2k inputs', () {
    const view = CreateView();
    expect(view.parseProtocol('https://example.com/file.bin'), Protocol.http);
    expect(
        view.parseProtocol(
            'magnet:?xt=urn:btih:0123456789012345678901234567890123456789'),
        Protocol.bt);
    expect(view.parseProtocol('/tmp/example.torrent'), Protocol.bt);
    expect(view.parseProtocol('ed2k://|file|test|1|hash|/'), Protocol.ed2k);
  });
}
