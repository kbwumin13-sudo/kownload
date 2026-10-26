import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/bindings/create_binding.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/create/views/netdisk_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_theme.dart';

import 'netdisk_test_support.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _HistoryBox extends Fake implements Box {
  final stored = <dynamic, dynamic>{};
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      key == 'macDownloadOnboarding.v1' ? true : stored[key] ?? defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    stored[key] = value;
  }
}

Future<_HistoryBox> _mountCreate(
    WidgetTester tester, MockNetdiskTransport transport,
    {CreateTask? request, Task? pendingTask, MockBrowserTool? browser}) async {
  MacPanel.incoming.value = null;
  MacPanel.presentation.value = 'panel';
  MacPanel.cancelAutoHide();
  addTearDown(MacPanel.cancelAutoHide);
  await tester.binding.setSurfaceSize(const Size(800, 600));
  addTearDown(() => tester.binding.setSurfaceSize(null));
  addTearDown(() async {
    await tester.pumpWidget(const SizedBox.shrink());
    MacPanel.cancelAutoHide();
    Get.reset();
    // Get.reset in the pinned 4.6.6 release leaves these delegates cached.
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
  });
  final history = _HistoryBox();
  Database.instance.box = history;
  final app = _App();
  app.downloaderConfig.value.downloadDir = '/downloads';
  app.downloaderConfig.value.extra.defaultDirectDownload = true;
  if (pendingTask != null) {
    transport.task = pendingTask;
    app.pendingUpdateTask.value =
        PendingUpdateTask(id: pendingTask.id, name: pendingTask.name);
  }
  Get.put<AppController>(app);
  await tester.pumpWidget(
      GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
    GetPage(name: '/', page: () => const Scaffold(body: Text('首页'))),
    GetPage(
        name: Routes.CREATE,
        page: () => CreateView(
            netdiskApi: transport.api,
            netdiskBrowserTool: browser ?? MockBrowserTool()),
        binding: CreateBinding(),
        transition: Transition.noTransition),
    GetPage(name: Routes.TASK, page: () => const Scaffold(body: Text('任务列表'))),
  ]));
  await tester.pumpAndSettle();
  Get.rootDelegate.toNamed(Routes.CREATE, arguments: request);
  await tester.pumpAndSettle();
  return history;
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
      'restart recovery resumes the original ID without preparing, creating, listing or patching',
      (tester) async {
    final transport = MockNetdiskTransport();
    final original = mockExistingTask(sessionId: '');
    final snapshot = original.toJson();
    transport.handler = (path, method, data) async =>
        path.contains('/browser/requests/') && method == 'GET'
            ? transport.envelope({
                'id': 'request',
                'source': {...mockSource.toJson(), 'shareId': 'share'},
                'state': 'ready',
                'sessionId': 'new-session'
              })
            : null;
    final browser = MockBrowserTool();
    final history = await _mountCreate(tester, transport,
        pendingTask: original,
        browser: browser,
        // A new webpage selection must not supersede the saved task.
        request: CreateTask(
            req: Request(url: mockSource.url, protocol: 'netdisk', extra: {
          'source': {...mockSource.toJson(), 'shareId': 'share'},
          'files': [
            const {'id': 'different', 'name': '其他文件.txt'}
          ],
          'sessionId': 'capture',
        })));
    expect(find.text('夸克网盘 · 恢复下载'), findsOneWidget);
    expect(find.text(mockFile.name), findsOneWidget);
    expect(find.text('其他文件.txt'), findsNothing);
    expect(find.text('/original-downloads'), findsOneWidget);
    expect(find.text('确认下载'), findsNothing);
    expect(transport.at('/tasks/original-task/continue'), isEmpty);
    await tester.tap(find.text('重新同步登录'));
    await tester.pump();
    await tester.pump(const Duration(seconds: 3));
    await tester.pumpAndSettle();
    expect(transport.at('/browser/request').single.$3,
        {'source': mockSource.toJson()});
    expect(browser.launches.single, contains(mockSource.url));
    final resumed = transport.at('/tasks/original-task/continue').single;
    expect(resumed.$2, 'PUT');
    expect(resumed.$3, isNull);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.at('/tasks'), isEmpty);
    expect(transport.at('/list'), isEmpty);
    expect(transport.calls.where((c) => c.$2 == 'PATCH'), isEmpty);
    expect(original.toJson(), snapshot);
    expect(Get.find<AppController>().pendingUpdateTask.value, isNull);
    expect(history.stored, isEmpty);
    expect(find.byKey(const ValueKey('panel-download-input')), findsOneWidget);
    expect(
        tester
            .widget<TextFormField>(
                find.byKey(const ValueKey('panel-download-input')))
            .controller!
            .text,
        isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets('cancelled recovery retains pending task and confirm reopens it',
      (tester) async {
    final transport = MockNetdiskTransport();
    final original = mockExistingTask();
    await _mountCreate(tester, transport, pendingTask: original);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(Get.find<AppController>().pendingUpdateTask.value?.id, original.id);
    // Recovery works without any pasted link or ordinary form validation.
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pump(const Duration(milliseconds: 300));
    // The underlying confirm button stays animated while its dialog is open.
    await tester.pump();
    expect(find.text('夸克网盘 · 恢复下载'), findsOneWidget);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.at('/tasks/original-task/continue'), isEmpty);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(Get.find<AppController>().pendingUpdateTask.value?.id, original.id);
    expect(tester.takeException(), isNull);
  });

  testWidgets('failed continue keeps recovery and pending state until success',
      (tester) async {
    final transport = MockNetdiskTransport();
    final original = mockExistingTask();
    var failResume = true;
    transport.handler = (path, method, data) async {
      if (path.contains('/browser/requests/') && method == 'GET') {
        return transport.envelope({
          'id': 'request',
          'source': mockSource.toJson(),
          'state': 'ready',
          'sessionId': 'new-session'
        });
      }
      if (path.endsWith('/continue') && failResume) {
        return transport
            .envelope({'reasonCode': 'login_required'}, code: 1, msg: '请登录原账号');
      }
      return null;
    };
    await _mountCreate(tester, transport, pendingTask: original);
    await tester.tap(find.text('重新同步登录'));
    await tester.pump();
    await tester.pump(const Duration(seconds: 3));
    await tester.pumpAndSettle();
    expect(find.textContaining('请登录原账号'), findsOneWidget);
    expect(Get.find<AppController>().pendingUpdateTask.value?.id, original.id);
    expect(find.byType(NetdiskView), findsOneWidget);
    failResume = false;
    await tester.tap(find.text('重新同步登录'));
    await tester.pump();
    await tester.pump(const Duration(seconds: 3));
    await tester.pumpAndSettle();
    expect(transport.at('/tasks/original-task/continue'), hasLength(2));
    expect(Get.find<AppController>().pendingUpdateTask.value, isNull);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.at('/list'), isEmpty);
    expect(find.byKey(const ValueKey('panel-download-input')), findsOneWidget);
    expect(
        tester
            .widget<TextFormField>(
                find.byKey(const ValueKey('panel-download-input')))
            .controller!
            .text,
        isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'mixed links intercept the multiline/direct shortcut before ordinary history or HTML download',
      (tester) async {
    final transport = MockNetdiskTransport();
    final history = await _mountCreate(tester, transport);
    const prose =
        'https://example.com/file.zip\n分享：https://pan.quark.cn/s/share 提取码：a123\n'
        'https://www.alipan.com/s/second 密码：b234';
    final url = find
        .descendant(
            of: find.byType(CreateView), matching: find.byType(TextFormField))
        .first;
    await tester.enterText(url, prose);
    await tester.pump();
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pump(const Duration(milliseconds: 300));
    await tester.pump();
    expect(find.byType(NetdiskView), findsOneWidget);
    expect(find.text('夸克网盘 · 确认下载'), findsOneWidget);
    expect(transport.at('/prepare'), isEmpty);
    expect(history.stored, isEmpty);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(tester.widget<TextFormField>(url).controller!.text, prose);
    expect(history.stored, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'native protocol opens the same confirmation and uses capture.files',
      (tester) async {
    final transport = MockNetdiskTransport();
    final history = await _mountCreate(tester, transport,
        request: CreateTask(
            req: Request(url: mockSource.url, protocol: 'netdisk', extra: {
              'provider': 'quark',
              'source': mockSource.toJson(),
              'sessionId': 'native',
              'files': [mockFile.toJson()],
            }),
            opts: Options(path: '/native-downloads')));
    expect(find.byType(NetdiskView), findsOneWidget);
    expect(find.text('报告.pdf'), findsOneWidget);
    expect(transport.at('/list'), isEmpty);
    expect(transport.at('/prepare'), isEmpty);
    expect(history.stored, isEmpty);
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(transport.at('/prepare').single.$3['sessionId'], 'native');
    expect(transport.at('/prepare').single.$3['opts']['path'],
        '/native-downloads');
    expect(transport.at('/tasks/batch'), hasLength(1));
    await tester.pump(const Duration(seconds: 1));
    MacPanel.cancelAutoHide();
    await tester.pumpAndSettle();
    expect(history.stored, isEmpty);
    expect(find.byKey(const ValueKey('panel-download-input')), findsOneWidget);
    expect(
        tester
            .widget<TextFormField>(
                find.byKey(const ValueKey('panel-download-input')))
            .controller!
            .text,
        isEmpty);
    expect(tester.takeException(), isNull);
  });
  testWidgets(
      'cancelling automatic netdisk confirmation consumes queued HTTP input',
      (tester) async {
    final transport = MockNetdiskTransport();
    await _mountCreate(tester, transport,
        request: CreateTask(
            req: Request(url: mockSource.url, protocol: 'netdisk', extra: {
          'provider': 'quark',
          'source': mockSource.toJson(),
          'sessionId': 'native',
          'files': [mockFile.toJson()],
        })));
    expect(find.byType(NetdiskView), findsOneWidget);
    await MacPanel.showPanel(
        task: CreateTask(req: Request(url: 'https://example.com/next.bin')));
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(find.byType(NetdiskView), findsNothing);
    expect(
        tester
            .widget<TextFormField>(
                find.byKey(const ValueKey('panel-download-input')))
            .controller!
            .text,
        'https://example.com/next.bin');
    expect(transport.at('/prepare'), isEmpty);
  });
}
