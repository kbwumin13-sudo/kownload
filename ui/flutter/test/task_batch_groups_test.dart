import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/app/modules/task/views/task_view.dart';
import 'package:gopeed/app/modules/task/views/task_batch_groups.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/database/task_batch_journal.dart';
import 'package:gopeed/database/task_batch_journal_store.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/util.dart';
import 'support/scoped_task_batch_fixture.dart';
import 'manual_task_batch_submission_test.dart' show spin;

class Tasks extends MacTaskListController {
  @override
  // ignore: must_call_super
  void onInit() {}
  @override
  // ignore: must_call_super
  void onClose() {}
  @override
  Future<void> getTasksState() async {}
}

Task task(String id, String name) => Task(
    id: id,
    name: name,
    status: Status.running,
    meta: Meta(
        req: Request(url: 'https://fixture.invalid/$id'),
        opts: Options(path: '/tmp/fixture-only')),
    uploading: false,
    progress: Progress(
        used: 1, speed: 10, downloaded: 500, uploadSpeed: 0, uploaded: 0),
    createdAt: DateTime(2026),
    updatedAt: DateTime(2026));
void main() {
  final f = ScopedBatchFixture();
  late Directory dir;
  late Box box;
  late TaskBatchJournalStore store;
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await f.start();
  });
  tearDownAll(f.close);
  setUp(() async {
    f.reset();
    dir = Directory('/tmp').createTempSync('batch-groups-');
    await Util.initStorageDir(previewStorage: dir.path);
    box = await Hive.openBox('fixture_${dir.path.hashCode.abs()}',
        path: dir.path);
    Database.instance.box = box;
    store = TaskBatchJournalStore(box: box, capacity: 20);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
  });
  tearDown(() async {
    Get.reset();
    await box.close();
    dir.deleteSync(recursive: true);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });
  Future<void> record(String id) async {
    await store.recordForQuery(
        TaskBatchJournalScope(
            route: f.route(),
            dataset: DatasetIdentity(
                version: 1, datasetId: f.dataset, persistent: true)),
        TaskBatchOperationRef(
            namespace: TaskBatchOperationRef.namespaceV1, operationId: id));
  }

  testWidgets(
      'actual empty TaskView still displays full-pending group without any POST or writes',
      (t) async {
    await t.runAsync(() => record('pending-op'));
    final r = f.receipt('pending-op', state: 'pending');
    for (final m in r['members'] as List) {
      m['state'] = 'pending';
    }
    r['taskIds'] = [];
    r['createdCount'] = 0;
    r['pendingCount'] = 2;
    f.receipts['pending-op'] = r;
    final before = box.get(TaskBatchJournalStore.storageKey);
    Get.put(TaskController());
    Get.put<MacTaskListController>(Tasks()..isLoading.value = false);
    await t.binding.setSurfaceSize(const Size(1000, 850));
    addTearDown(() => t.binding.setSurfaceSize(null));
    await t.pumpWidget(GetMaterialApp(
        theme: macTheme(Brightness.light), home: const TaskView()));
    await spin(
        t, () => find.text('已创建 0 · 失败 0 · 待创建 2').evaluate().isNotEmpty);
    expect(find.text('暂无任务'), findsOneWidget);
    expect(find.text('批次 pending-op'), findsOneWidget);
    await t.tap(find.text('批次 pending-op'));
    await t.pumpAndSettle();
    expect(find.text('待创建 · 大小未知'), findsNWidgets(2));
    expect(f.posts, 0);
    expect(box.get(TaskBatchJournalStore.storageKey), before);
  });
  testWidgets(
      'deleted or conflicting created members never adopt stale Task status',
      (t) async {
    final r = f.receipt('history');
    r['members'][0]['taskPresent'] = false;
    r['members'][1]['error'] = 'association_conflict';
    f.receipts['history'] = r;
    await t.pumpWidget(MaterialApp(
        home: Scaffold(
            body: TaskBatchGroups(tasks: [
      task('history-task-0', 'stale-deleted.bin'),
      task('history-task-1', 'foreign.bin')
    ], journal: store))));
    await spin(
        t,
        () =>
            find.text('批次 history').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    await t.tap(find.text('批次 history'));
    await t.pumpAndSettle();
    expect(find.text('已创建，任务已移除'), findsOneWidget);
    expect(find.text('创建关联尚未确认，未采用当前任务状态'), findsOneWidget);
    expect(find.text('stale-deleted.bin'), findsNothing);
    expect(find.text('foreign.bin'), findsNothing);
    expect(f.posts, 0);
  });
  testWidgets(
      'created Task fields are reused while missing size remains unknown',
      (t) async {
    f.receipts['current'] = f.receipt('current');
    f.receipts['current']!['members'][0]['taskPresent'] = true;
    await t.pumpWidget(MaterialApp(
        home: Scaffold(
            body: TaskBatchGroups(
                tasks: [task('current-task-0', 'saved-file.bin')],
                journal: store))));
    await spin(
        t,
        () =>
            find.text('批次 current').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    await t.tap(find.text('批次 current'));
    await t.pumpAndSettle();
    expect(find.text('saved-file.bin'), findsOneWidget);
    expect(find.text('下载中 · 已下载 500 字节 · 大小未知'), findsOneWidget);
    expect(f.posts, 0);
  });
  testWidgets(
      'real Hive close/reopen reconnects only by querying the persisted reference',
      (t) async {
    String? before;
    await t.runAsync(() async {
      await record('reopened-op');
      before = box.get(TaskBatchJournalStore.storageKey);
      final name = box.name;
      await box.close();
      box = await Hive.openBox(name, path: dir.path);
      Database.instance.box = box;
      store = TaskBatchJournalStore(box: box, capacity: 20);
    });
    f.receipts['reopened-op'] = f.receipt('reopened-op', state: 'pending');
    await t.pumpWidget(MaterialApp(
        home:
            Scaffold(body: TaskBatchGroups(tasks: const [], journal: store))));
    await spin(
        t,
        () =>
            find.text('已创建 1 · 失败 0 · 待创建 1').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    expect(f.posts, 0);
    expect(f.scopedGets, 1);
    expect(box.get(TaskBatchJournalStore.storageKey), before);
  });
  testWidgets(
      'unknown operation is never presented as zero created or complete',
      (t) async {
    await t.runAsync(() => record('missing-op'));
    await t.pumpWidget(MaterialApp(
        home:
            Scaffold(body: TaskBatchGroups(tasks: const [], journal: store))));
    await spin(
        t,
        () =>
            find.text('结果未知，仅可查询').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    expect(find.text('已创建 0 · 失败 0 · 待创建 0'), findsNothing);
    expect(f.posts, 0);
  });
  testWidgets(
      'missing taskPresent never adopts an unverified current Task by ID alone',
      (t) async {
    f.receipts['uncertain'] = f.receipt('uncertain');
    await t.pumpWidget(MaterialApp(
        home: Scaffold(
            body: TaskBatchGroups(
                tasks: [task('uncertain-task-0', 'unverified.bin')],
                journal: store))));
    await spin(
        t,
        () =>
            find.text('批次 uncertain').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    await t.tap(find.text('批次 uncertain'));
    await t.pumpAndSettle();
    expect(find.text('unverified.bin'), findsNothing);
    expect(find.text('已创建，任务状态尚未读取'), findsNWidgets(2));
    expect(f.posts, 0);
  });
  testWidgets(
      'changed dataset isolates old journal without claiming its op or rewriting bytes',
      (t) async {
    await t.runAsync(() => record('old-op'));
    final before = box.get(TaskBatchJournalStore.storageKey);
    f.dataset = 'c' * 32;
    await t.pumpWidget(MaterialApp(
        home:
            Scaffold(body: TaskBatchGroups(tasks: const [], journal: store))));
    await spin(t, () => find.text('另有 1 个原连接引用已隔离保留。').evaluate().isNotEmpty);
    expect(f.scopedGets, 0);
    expect(f.posts, 0);
    expect(box.get(TaskBatchJournalStore.storageKey), before);
  });
  testWidgets(
      'UI reference event refreshes an already-mounted empty group area without polling',
      (t) async {
    await t.pumpWidget(MaterialApp(
        home:
            Scaffold(body: TaskBatchGroups(tasks: const [], journal: store))));
    await spin(t,
        () => f.requests.length >= 3 && find.text('正在查询…').evaluate().isEmpty);
    await t.runAsync(() => record('event-op'));
    f.receipts['event-op'] = f.receipt('event-op', state: 'pending');
    TaskBatchGroups.referencesChanged();
    await spin(
        t,
        () =>
            find.text('批次 event-op').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    final requests = f.requests.length;
    await t.pump(const Duration(seconds: 10));
    expect(f.requests.length, requests);
    expect(f.posts, 0);
  });
  testWidgets(
      'live empty page with hasMore offers forward progress instead of hiding groups',
      (t) async {
    f.override = (req) async {
      if (req.uri.path == '/api/v1/task-batches/operations') {
        final next = req.uri.queryParameters['cursor'];
        if (next == null) {
          await f.respond(req, {
            'version': 1,
            'namespace': 'task-batch.create/v1',
            'datasetId': f.dataset,
            'persistent': true,
            'consistency': 'live-keyset',
            'filter': 'all',
            'items': [],
            'hasMore': true,
            'nextCursor': 'opaque-next',
            'scanned': 1000
          });
        } else {
          f.receipts['later'] = f.receipt('later');
          await f.handle(req);
        }
        return;
      }
      await f.handle(req);
    };
    await t.pumpWidget(MaterialApp(
        home:
            Scaffold(body: TaskBatchGroups(tasks: const [], journal: store))));
    await spin(
        t,
        () =>
            find.text('显示更多批次').evaluate().isNotEmpty &&
            find.text('正在查询…').evaluate().isEmpty);
    await t.tap(find.text('显示更多批次'));
    await spin(t, () => find.text('批次 later').evaluate().isNotEmpty);
    expect(f.posts, 0);
    expect(box.get(TaskBatchJournalStore.storageKey), isNull);
  });
}
