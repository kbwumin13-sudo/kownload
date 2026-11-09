import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:hive/hive.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/api/model/resolve_result.dart';
import 'package:gopeed/api/model/resolve_task.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_widgets.dart';

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

class _Tasks extends MacTaskListController {
  // This fixture has no polling timer.
  // ignore: must_call_super
  @override
  void onInit() {}
  // ignore: must_call_super
  @override
  void onClose() {}
}

Task _task(String id, Status status) => Task(
      id: id,
      name: '$id.bin',
      status: status,
      uploading: false,
      meta:
          Meta(req: Request(url: 'https://fixture.test/$id'), opts: Options()),
      progress: Progress(
          used: 0, speed: 0, downloaded: 0, uploadSpeed: 0, uploaded: 0),
      createdAt: DateTime(2026),
      updatedAt: DateTime(2026),
    );

void main() {
  testWidgets(
      'browser result selects ongoing and reveals queued or completed tasks',
      (tester) async {
    if (!Platform.isMacOS) return;
    Database.instance.box = _Box()..onboardingCompleted = true;
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    Get.put<AppController>(app);
    Get.put(CreateController());
    final tasks = Get.put<MacTaskListController>(_Tasks());
    tasks.tasks.assignAll([
      for (var i = 0; i < 3; i++) _task('existing-$i', Status.running),
      _task('new-queued', Status.wait),
      _task('new-done', Status.done),
    ]);
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = null;
    MacPanel.browserResult.value = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 760));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.browserResult.value = null;
      MacPanel.cancelAutoHide();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(name: '/', page: () => const CreateView()),
    ]));
    await tester.pumpAndSettle();
    expect(find.text('new-queued.bin'), findsNothing);
    await tester.tap(find.textContaining('已完成').first);
    await tester.pumpAndSettle();
    expect(find.text('new-done.bin'), findsOneWidget);
    await MacPanel.showBrowserResult(['new-queued'], null);
    await tester.pumpAndSettle();
    expect(find.text('new-queued.bin'), findsOneWidget);
    expect(find.text('new-done.bin'), findsNothing);
    await MacPanel.showBrowserResult(['new-done'], null);
    await tester.pumpAndSettle();
    expect(find.text('new-done.bin'), findsOneWidget);
    await MacPanel.showBrowserResult([], '添加下载失败：测试原因');
    await tester.pumpAndSettle();
    expect(find.text('添加下载失败：测试原因'), findsOneWidget);
    await tester.pump(const Duration(seconds: 3));
    expect(MacPanel.visible.value, isTrue);
    expect(tester.takeException(), isNull);
  });
  testWidgets(
      'browser prefill stays on ongoing after confirm and shows submit errors',
      (tester) async {
    if (!Platform.isMacOS) return;
    Database.instance.box = _Box()..onboardingCompleted = true;
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    app.downloaderConfig.value.extra.defaultDirectDownload = true;
    Get.put<AppController>(app);
    Get.put(CreateController());
    MacPanel.presentation.value = 'panel';
    MacPanel.browserResult.value = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await MacPanel.showPanel(
        task: CreateTask(req: Request(url: 'https://fixture.test/confirmed')),
        fromBrowser: true);
    await tester.binding.setSurfaceSize(const Size(640, 760));
    var reject = false;
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.browserResult.value = null;
      MacPanel.incoming.value = null;
      MacPanel.cancelAutoHide();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: '/',
          page: () => CreateView(taskCreator: (_) async {
                if (reject) throw StateError('测试提交失败');
                return 'browser-confirmed';
              })),
    ]));
    await tester.pumpAndSettle();
    expect(find.text('https://fixture.test/confirmed'), findsOneWidget);
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pumpAndSettle();
    await tester.pump(const Duration(seconds: 3));
    expect(MacPanel.visible.value, isTrue);
    expect(MacPanel.browserResult.value!.taskIds, ['browser-confirmed']);
    expect(MacPanel.notice.value, isNull);
    reject = true;
    await MacPanel.showPanel(
        task: CreateTask(req: Request(url: 'https://fixture.test/fail')),
        fromBrowser: true);
    await tester.pumpAndSettle();
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pumpAndSettle();
    expect(find.textContaining('测试提交失败'), findsOneWidget);
    expect(MacPanel.visible.value, isTrue);
  });
  testWidgets('magnet timeout preserves the draft and allows another resolve',
      (tester) async {
    if (!Platform.isMacOS) return;
    Database.instance.box = _Box()..onboardingCompleted = true;
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    app.downloaderConfig.value.extra.defaultDirectDownload = false;
    Get.put<AppController>(app);
    final controller = Get.put(CreateController());
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
    final requests = <ResolveTask>[];
    var pending = Completer<ResolveResult>();
    await tester.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: '/',
          page: () => CreateView(taskResolver: (task) {
                requests.add(task);
                return pending.future;
              }))
    ]));
    await tester.pumpAndSettle();
    const magnet =
        'magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567';
    const error = '获取种子信息超时，请检查网络或稍后重试';
    final input = find.byKey(const ValueKey('panel-download-input'));
    final button = find.descendant(
        of: find.byTooltip('开始下载'), matching: find.byType(MacButton));
    await tester.enterText(input, magnet);
    await tester.pump();
    await tester.tap(button);
    await tester.pump();
    expect(requests, hasLength(1));
    expect(controller.isConfirming.value, isTrue);
    expect(tester.widget<MacButton>(button).onPressed, isNull);
    // Submitting with Return while waiting must not start another resolve.
    await tester.testTextInput.receiveAction(TextInputAction.done);
    await tester.pump();
    expect(requests, hasLength(1));
    pending.completeError(api.TimeoutException(error));
    await tester.pumpAndSettle();
    expect(find.text(error), findsOneWidget);
    expect(controller.isConfirming.value, isFalse);
    expect(tester.widget<MacButton>(button).onPressed, isNotNull);
    expect(tester.widget<TextFormField>(input).controller!.text, magnet);
    pending = Completer<ResolveResult>();
    await tester.tap(button);
    await tester.pump();
    expect(requests, hasLength(2));
    expect(requests.last.req!.url, magnet);
    expect(find.text(error), findsNothing);
    expect(controller.isConfirming.value, isTrue);
    pending.completeError(api.ConnectionException('无法连接下载服务'));
    await tester.pumpAndSettle();
    expect(find.text('无法连接下载服务'), findsOneWidget);
    expect(controller.isConfirming.value, isFalse);
    expect(tester.widget<MacButton>(button).onPressed, isNotNull);
    expect(tester.widget<TextFormField>(input).controller!.text, magnet);
    expect(tester.takeException(), isNull);
  });

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
