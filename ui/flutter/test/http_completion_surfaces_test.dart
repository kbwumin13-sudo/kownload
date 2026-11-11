import 'dart:io';
import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:hive/hive.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resource.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/app/modules/task/views/task_view.dart';
import 'package:gopeed/app/modules/task/views/task_batch_groups.dart';
import 'package:gopeed/app/modules/task/views/http_task_retry_button.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/database/task_batch_journal_store.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/util.dart';
import 'support/scoped_task_batch_fixture.dart';
import 'manual_task_batch_submission_test.dart' show spin;

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Tasks extends MacTaskListController {
  @override
  // ignore: must_call_super
  void onInit() {
    isLoading.value = false;
  }

  @override
  // ignore: must_call_super
  void onClose() {}
  @override
  Future<void> getTasksState() async {}
}

void main() {
  final f = ScopedBatchFixture();
  late Directory dir;
  late Box box;
  late TaskBatchJournalStore journal;
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await f.start();
  });
  tearDownAll(f.close);
  setUp(() async {
    f.reset();
    dir = Directory('/tmp').createTempSync('k16-completion-surfaces-');
    await Util.initStorageDir(previewStorage: dir.path);
    box = await Hive.openBox('surface_fixture', path: dir.path);
    await box.put(MacOnboarding.completedKey, true);
    Database.instance.box = box;
    journal = TaskBatchJournalStore(box: box, capacity: 20);
    Get.put<AppController>(_App()).downloaderConfig.value.downloadDir =
        dir.path;
    Get.put(CreateController());
    Get.put(TaskController());
    Get.put<MacTaskListController>(_Tasks());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = null;
    MacPanel.taskResult.value = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(SystemChannels.platform, (_) async => null);
  });
  tearDown(() async {
    MacPanel.cancelAutoHide();
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    await box.close();
    dir.deleteSync(recursive: true);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(SystemChannels.platform, null);
  });
  testWidgets('HTTP error retry is reachable in full list and compact panel',
      (t) async {
    f.override = (request) async {
      if (request.uri.path.endsWith('/stats')) {
        await f.respond(request, {'lastError': 'fixture failure'});
      } else {
        await f.handle(request);
      }
    };
    final task = Task(
        id: '_retry-task',
        name: 'retry.bin',
        meta: Meta(
            req: Request(url: 'https://fixture.invalid/file'), opts: Options()),
        status: Status.error,
        uploading: false,
        progress: Progress(
            used: 0, speed: 0, downloaded: 0, uploadSpeed: 0, uploaded: 0),
        createdAt: DateTime.utc(2026),
        updatedAt: DateTime.utc(2026))
      ..protocol = Protocol.http;
    Get.find<MacTaskListController>().tasks.assignAll([task]);
    final page = ValueNotifier<Widget>(const TaskView());
    await t.binding.setSurfaceSize(const Size(1000, 850));
    addTearDown(() async {
      await t.pumpWidget(const SizedBox());
      page.dispose();
      await t.binding.setSurfaceSize(null);
    });
    await t.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: '/',
          page: () => ValueListenableBuilder<Widget>(
              valueListenable: page, builder: (_, view, __) => view))
    ]));
    await t.pumpAndSettle();
    await spin(
        t, () => find.textContaining('fixture failure').evaluate().isNotEmpty);
    await spin(t, () {
      final refresh = find.byKey(const ValueKey('task-batch-refresh'));
      return refresh.evaluate().isNotEmpty &&
          t.widget<TextButton>(refresh).onPressed != null &&
          f.inFlight == 0;
    });
    expect(find.byType(HttpTaskRetryButton), findsOneWidget);
    expect(find.byTooltip('重试'), findsOneWidget);
    page.value = const CreateView();
    await t.pumpAndSettle();
    await spin(
        t, () => find.textContaining('fixture failure').evaluate().isNotEmpty);
    expect(find.byType(HttpTaskRetryButton), findsOneWidget);
    expect(find.byTooltip('重试'), findsOneWidget);
    expect(f.posts, 0);
    expect(box.get(TaskBatchJournalStore.storageKey), isNull);
  });
  for (final kind in [
    'missing',
    'version',
    'association',
    'dataset',
    'waiting_verify',
    'verified',
    'published_pending_commit',
    'committed',
    'legacy',
    'bt'
  ]) {
    testWidgets(
        'same done Task completion gate across list, compact and group: $kind',
        (t) async {
      const id = 'surface-op-task-0';
      final raw = Task(
          id: id,
          name: 'surface-file.bin',
          meta: Meta(
              req: Request(url: 'https://fixture.invalid/file'),
              opts: Options()),
          status: Status.done,
          uploading: false,
          progress: Progress(
              used: 0,
              speed: 0,
              downloaded: 32768,
              uploadSpeed: 0,
              uploaded: 0),
          createdAt: DateTime.utc(2026),
          updatedAt: DateTime.utc(2026))
        ..protocol = kind == 'bt' ? Protocol.bt : Protocol.http
        ..meta.res = Resource(size: 32768, files: const []);
      final json = raw.toJson();
      final enrolled = kind != 'legacy' && kind != 'bt';
      if (enrolled) {
        json['completion'] = {'version': 1};
        if (kind != 'missing') {
          json['completionView'] = {
            'version': kind == 'version' ? 2 : 1,
            'scopeVersion': 1,
            'datasetId': kind == 'dataset' ? 'c' * 32 : f.dataset,
            'taskId': kind == 'association' ? 'other-task' : id,
            'completionId': 'surface-completion',
            'available': true,
            'phase': [
              'waiting_verify',
              'verified',
              'published_pending_commit',
              'committed'
            ].contains(kind)
                ? kind
                : 'committed',
            'recordedMatch': kind == 'waiting_verify' ? null : true,
            'errorCategory': null
          };
        }
      }
      final task = Task.fromJson(json);
      final snapshot = api.activeApiSnapshot!;
      task.completionView
          ?.bindToDataset(f.dataset, isCurrent: () => snapshot.isCurrent);
      Get.find<MacTaskListController>().tasks.assignAll([task]);
      final receipt = f.receipt('surface-op', count: 1);
      receipt['members'][0]['taskPresent'] = true;
      f.receipts['surface-op'] = receipt;
      final page = ValueNotifier<Widget>(const SizedBox.shrink());
      await t.binding.setSurfaceSize(const Size(1000, 850));
      addTearDown(() async {
        await t.pumpWidget(const SizedBox.shrink());
        page.dispose();
        await t.binding.setSurfaceSize(null);
      });
      await t.pumpWidget(
          GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
        GetPage(
            name: '/',
            page: () => ValueListenableBuilder<Widget>(
                valueListenable: page, builder: (_, view, __) => view))
      ]));
      await t.pumpAndSettle();
      final expected = enrolled
          ? kind == 'committed'
              ? '校验并保存完成'
              : '校验完成状态未确认'
          : null;
      final results = <String, bool>{};
      page.value = const TaskView();
      await t.pumpAndSettle();
      await t.tap(find.widgetWithText(TextButton, '已完成'));
      await t.pumpAndSettle();
      results['list'] = expected == null
          ? find.textContaining('downloaded'.tr).evaluate().isNotEmpty
          : find.textContaining(expected).evaluate().isNotEmpty;
      page.value = const CreateView();
      await t.pumpAndSettle();
      await t.tap(find.textContaining('已完成').first);
      await t.pumpAndSettle();
      results['compact'] = expected == null
          ? find.textContaining('已完成 ·').evaluate().isNotEmpty
          : find.textContaining(expected).evaluate().isNotEmpty;
      if (enrolled && kind != 'committed') {
        results['compact has no done icon'] =
            find.byIcon(CupertinoIcons.checkmark_circle).evaluate().isEmpty;
      }
      page.value =
          Scaffold(body: TaskBatchGroups(tasks: [task], journal: journal));
      await t.pump();
      await spin(
          t,
          () =>
              find.text('批次 surface-op').evaluate().isNotEmpty &&
              find.text('正在查询…').evaluate().isEmpty);
      await t.tap(find.text('批次 surface-op'));
      await t.pumpAndSettle();
      results['group'] = expected == null
          ? find.textContaining('已完成 · 已下载').evaluate().isNotEmpty
          : find.textContaining(expected).evaluate().isNotEmpty;
      expect(results, {for (final key in results.keys) key: true});
      expect(f.posts, 0);
      expect(box.get(TaskBatchJournalStore.storageKey), isNull);
    });
  }
}
