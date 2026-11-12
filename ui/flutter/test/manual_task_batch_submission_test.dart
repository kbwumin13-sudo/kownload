import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:gopeed/app/views/directory_selector.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/http_path_create.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/app/modules/task/views/task_batch_groups.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/database/task_batch_journal.dart';
import 'package:gopeed/database/task_batch_journal_store.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/util.dart';
import 'support/scoped_task_batch_fixture.dart';

class App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
  @override
  Future<void> saveConfig() async {}
}

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

class SettingsBox extends Fake implements Box {
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      key == MacOnboarding.completedKey ? true : defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {}
}

class JournalBox extends Fake implements Box {
  final Box inner;
  final Zone ioZone = Zone.current;
  Completer<void>? gate;
  bool fail = false;
  final started = Completer<void>();
  int puts = 0;
  final settled = Completer<void>();
  JournalBox(this.inner);
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      inner.get(key, defaultValue: defaultValue);
  @override
  Future<void> put(dynamic key, dynamic value) async {
    puts++;
    if (!started.isCompleted) started.complete();
    try {
      if (gate != null) await gate!.future;
      if (fail) throw StateError('fixture journal failure');
      await ioZone.run(() => inner.put(key, value));
    } finally {
      if (!settled.isCompleted) settled.complete();
    }
  }
}

Future<void> spin(WidgetTester tester, bool Function() done) async {
  for (var i = 0; i < 100 && !done(); i++) {
    await tester
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 10)));
    await tester.pump(const Duration(milliseconds: 20));
  }
  expect(done(), isTrue, reason: 'bounded widget/HTTP fixture did not settle');
}

void main() {
  final f = ScopedBatchFixture();
  late Directory dir;
  late Box box;
  late JournalBox journalBox;
  const urls = 'https://fixture.invalid/one\nhttps://fixture.invalid/two';
  int singles = 0;
  TextEditingController? urlInput;
  Completer<String>? singleGate;
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await f.start();
  });
  tearDownAll(f.close);
  setUp(() async {
    f.reset();
    dir = Directory('/tmp').createTempSync('manual-batch-widget-');
    await Util.initStorageDir(previewStorage: dir.path);
    box = await Hive.openBox('fixture_${dir.path.hashCode.abs()}',
        path: dir.path);
    await box.put(MacOnboarding.completedKey, true);
    Database.instance.box = SettingsBox();
    journalBox = JournalBox(box);
    singles = 0;
    singleGate = null;
    final app = Get.put<AppController>(App());
    app.downloaderConfig.value.downloadDir = dir.path;
    Get.put(CreateController());
    Get.put<MacTaskListController>(Tasks());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = null;
    MacPanel.taskResult.value = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(SystemChannels.platform, (_) async => null);
  });
  tearDown(() async {
    if (singleGate?.isCompleted == false) singleGate!.complete('new-single');
    if (journalBox.gate?.isCompleted == false) journalBox.gate!.complete();
    if (f.postGate?.isCompleted == false) f.postGate!.complete();
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    if (box.isOpen) await box.close();
    dir.deleteSync(recursive: true);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(SystemChannels.platform, null);
  });
  Future<void> mount(WidgetTester t,
      {Duration timeout = const Duration(seconds: 2),
      bool showGroups = false}) async {
    await t.binding.setSurfaceSize(const Size(900, 850));
    addTearDown(() async {
      await t.pumpWidget(const SizedBox.shrink());
      MacPanel.cancelAutoHide();
      await t.pump(const Duration(seconds: 4));
      await t.binding.setSurfaceSize(null);
    });
    await t.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: '/',
          page: () {
            final create = CreateView(
                batchJournal:
                    TaskBatchJournalStore(box: journalBox, capacity: 10),
                batchAcknowledgementTimeout: timeout,
                taskCreator: (CreateTask task) async {
                  singles++;
                  return singleGate == null
                      ? 'single-$singles'
                      : await singleGate!.future;
                });
            return showGroups
                ? Material(
                    child: Column(children: [
                    Expanded(child: create),
                    TaskBatchGroups(
                        tasks: const [],
                        journal: TaskBatchJournalStore(
                            box: journalBox, capacity: 10))
                  ]))
                : create;
          })
    ]));
    await t.pumpAndSettle();
    urlInput = t
        .widget<TextFormField>(
            find.byKey(const ValueKey('panel-download-input')))
        .controller;
    await t.enterText(find.byKey(const ValueKey('panel-download-input')), urls);
    await t.pump();
  }

  Future<void> click(WidgetTester t) async {
    await t.tap(find.byTooltip('开始下载'));
    await t.pump();
  }

  String input(WidgetTester t) => urlInput!.text;
  Future<void> checksum(WidgetTester t, {bool showGroups = false}) async {
    await mount(t, showGroups: showGroups);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/single');
    await t.tap(find.text('更多选项'));
    await t.pumpAndSettle();
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, 'A' * 64);
  }

  Future<void> confirmChecksum(WidgetTester t) async {
    await t.tap(find.text('confirm'.tr));
    await t.pump();
  }

  void verifiedConflict(int status) {
    f.verified = true;
    f.override = (request) async {
      if (request.method == 'POST') {
        final op = f.requests.last['body']['operationId'] as String;
        // The old R belongs to another expected digest. Its history is valid,
        // but does not acknowledge the new 'a'*64 intent sent by this draft.
        f.receipts[op] = f.receipt(op, count: 1);
        final history = f.wrapper(f.receipts[op]!)
          ..['completionViews'] = [
            {
              'version': 1,
              'scopeVersion': 1,
              'datasetId': f.dataset,
              'taskId': '$op-task-0',
              'completionId': 'old-completion',
              'available': true,
              'phase': 'committed',
              'recordedMatch': true
            }
          ];
        await f.respond(request, history,
            status: status, code: 1000, msg: 'operation_conflict');
      } else if (request.method == 'GET' &&
          request.uri.path.contains('/task-batches/v1/operations/')) {
        final op = request.uri.pathSegments.last;
        await f.respond(request, f.wrapper(f.receipts[op]!));
      } else {
        await f.handle(request);
      }
    };
  }

  Future<void> queryOriginal(WidgetTester t) async {
    final query = find.byKey(const ValueKey('manual-batch-query'));
    final gets = f.scopedGets;
    await t.ensureVisible(query);
    await t.tap(query);
    await t.pump();
    await spin(
        t,
        () =>
            f.scopedGets > gets &&
            (query.evaluate().isEmpty ||
                t.widget<TextButton>(query).onPressed != null));
  }

  for (final status in [409, 200]) {
    testWidgets('verified conflict $status keeps draft after old settled GET',
        (t) async {
      verifiedConflict(status);
      await checksum(t);
      await confirmChecksum(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      expect(input(t), 'https://fixture.invalid/single');
      await queryOriginal(t);
      expect(input(t), 'https://fixture.invalid/single');
      final digest = find.byKey(const ValueKey('http-expected-sha256'));
      expect(t.widget<TextFormField>(digest).controller!.text, 'A' * 64);
      expect(find.textContaining('与原批次冲突'), findsOneWidget);
      expect(MacPanel.taskResult.value, isNull);
      expect(f.posts, 1);
      expect(singles, 0);
      expect(journalBox.puts, 1);
      final original = f.requests.singleWhere((r) => r['method'] == 'POST');
      expect(original['body']['integrity']['expectedSHA256'], 'a' * 64);
      // Neither another history query nor the confirm button can consume a
      // new permit for this already-conflicted attempt.
      await t.ensureVisible(digest);
      await t.enterText(digest, 'C' * 64);
      await confirmChecksum(t);
      await spin(t, () => !Get.find<CreateController>().isConfirming.value);
      expect(input(t), 'https://fixture.invalid/single');
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
      expect(MacPanel.taskResult.value, isNull);
      expect(t.widget<TextFormField>(digest).controller!.text, 'C' * 64);
    });
  }

  testWidgets('verified conflict late history cannot affect a new URL draft',
      (t) async {
    verifiedConflict(409);
    await checksum(t);
    await confirmChecksum(t);
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    final gate = Completer<void>();
    var waiting = false;
    addTearDown(() {
      if (!gate.isCompleted) gate.complete();
    });
    final previous = f.override!;
    f.override = (request) async {
      if (request.method == 'GET' &&
          request.uri.path.contains('/verified-http/v1/operations/')) {
        waiting = true;
        await gate.future;
      }
      await previous(request);
    };
    final query = find.byKey(const ValueKey('manual-batch-query'));
    await t.ensureVisible(query);
    await t.tap(query);
    await t.pump();
    await spin(t, () => waiting);
    final url = find.byType(TextFormField).first;
    await t.ensureVisible(url);
    await t.enterText(url, 'https://fixture.invalid/new-draft');
    await t.pump();
    gate.complete();
    await spin(t, () => f.inFlight == 0);
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
    await t.pump(const Duration(milliseconds: 20));
    expect(input(t), 'https://fixture.invalid/new-draft');
    expect(find.textContaining('与原批次冲突'), findsNothing);
    expect(find.byKey(const ValueKey('manual-batch-query')), findsNothing);
    expect(MacPanel.taskResult.value, isNull);
    expect(f.posts, 1);
    expect(singles, 0);
    expect(journalBox.puts, 1);
  });

  testWidgets(
      'verified conflict cold journal restores history without completing a new draft',
      (t) async {
    verifiedConflict(409);
    await checksum(t);
    await confirmChecksum(t);
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    final op = f.requests.singleWhere((r) => r['method'] == 'POST')['body']
        ['operationId'] as String;
    final stored = box.get(TaskBatchJournalStore.storageKey);
    expect(TaskBatchJournal.decode(stored).refs.single.state, 'query_required');
    await t.pumpWidget(const SizedBox());
    await t.pump();
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    final name = box.name;
    await t.runAsync(() async {
      await box.close();
      box = await Hive.openBox(name, path: dir.path);
      journalBox = JournalBox(box);
    });
    final app = Get.put<AppController>(App());
    app.downloaderConfig.value.downloadDir = dir.path;
    Get.put(CreateController());
    Get.put<MacTaskListController>(Tasks());
    MacPanel.incoming.value = null;
    MacPanel.taskResult.value = null;
    await checksum(t, showGroups: true);
    await spin(
        t,
        () =>
            find.byKey(ValueKey('batch-$op')).evaluate().isNotEmpty &&
            find.text('已创建 1 · 失败 0 · 待创建 0').evaluate().isNotEmpty);
    expect(input(t), 'https://fixture.invalid/single');
    expect(
        t
            .widget<TextFormField>(
                find.byKey(const ValueKey('http-expected-sha256')))
            .controller!
            .text,
        'A' * 64);
    expect(find.byKey(const ValueKey('manual-batch-query')), findsNothing);
    expect(MacPanel.taskResult.value, isNull);
    expect(journalBox.puts, 0);
    expect(box.get(TaskBatchJournalStore.storageKey), stored);
    expect(f.posts, 1);
    expect(singles, 0);
  });

  for (final option in ['autoExtract', 'autoTorrentEnable']) {
    testWidgets(
        'verified local $option true rejection leaves no journal; same draft false sends once',
        (t) async {
      f.verified = true;
      await checksum(t);
      final toggle = find.text(option.tr);
      await t.ensureVisible(toggle);
      await t.tap(toggle);
      await t.pumpAndSettle();
      await confirmChecksum(t);
      await spin(t, () => !Get.find<CreateController>().isConfirming.value);
      expect(journalBox.puts, 0);
      expect(f.requests, isEmpty);
      expect(singles, 0);
      expect(find.textContaining('unsupported_option'), findsOneWidget);
      expect(find.byKey(const ValueKey('manual-batch-query')), findsNothing);
      expect(input(t), 'https://fixture.invalid/single');
      final digest = find.byKey(const ValueKey('http-expected-sha256'));
      expect(t.widget<TextFormField>(digest).controller!.text, 'A' * 64);
      await t.ensureVisible(toggle);
      await t.tap(toggle);
      await t.pumpAndSettle();
      await confirmChecksum(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      expect(journalBox.puts, 1);
      expect(singles, 0);
      final body =
          f.requests.firstWhere((r) => r['method'] == 'POST')['body'] as Map;
      final field = option == 'autoExtract' ? 'autoExtract' : 'autoTorrent';
      final inherited = option == 'autoExtract' ? 'autoTorrent' : 'autoExtract';
      expect(body['request']['opts']['extra'][field], false);
      expect(body['request']['opts']['extra'][inherited], isNull);
      expect(body['integrity']['expectedSHA256'], 'a' * 64);
    });
    testWidgets(
        'verified unknown ACK changing $option to true still queries original reference',
        (t) async {
      f.verified = true;
      f.mode = 'lostAck';
      await checksum(t);
      await confirmChecksum(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      final toggle = find.text(option.tr);
      await t.ensureVisible(toggle);
      await t.tap(toggle);
      await t.pumpAndSettle();
      f.mode = 'success';
      final gets = f.scopedGets;
      await confirmChecksum(t);
      await spin(
          t,
          () =>
              f.scopedGets > gets &&
              !Get.find<CreateController>().isConfirming.value);
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
      expect(singles, 0);
      expect(input(t), 'https://fixture.invalid/single');
      expect(find.text('原批次已确认，当前修改后的草稿已保留。'), findsOneWidget);
      final query = f.requests
          .lastWhere((r) => (r['uri'] as Uri).path.contains('/operations/'));
      expect(query['method'], 'GET');
      expect(query['body'], isNull);
    });
  }
  testWidgets(
      'verified single journal ACK gates exactly one dedicated POST, no legacy resolve/create',
      (t) async {
    f.verified = true;
    journalBox.gate = Completer<void>();
    await checksum(t);
    await confirmChecksum(t);
    await spin(t, () => journalBox.started.isCompleted);
    expect(f.posts, 0);
    expect(singles, 0);
    journalBox.gate!.complete();
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    expect(journalBox.puts, 1);
    expect(singles, 0);
    final request = f.requests.firstWhere((r) => r['method'] == 'POST');
    expect(
        (request['uri'] as Uri).path, contains('/verified-http/v1/operations'));
    expect(request['body']['integrity'],
        {'algorithm': 'sha256', 'expectedSHA256': 'a' * 64});
    expect(input(t), '');
  });
  for (final outcome in [
    'lostAck',
    'html',
    'partial',
    'redirect',
    'wrongDomain'
  ]) {
    testWidgets(
        'verified $outcome keeps original reference; changed digest/options query only',
        (t) async {
      f.verified = true;
      f.mode = outcome;
      await checksum(t);
      await confirmChecksum(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      expect(journalBox.puts, 1);
      expect(singles, 0);
      expect(input(t), 'https://fixture.invalid/single');
      final digest = find.byKey(const ValueKey('http-expected-sha256'));
      if (digest.evaluate().isEmpty) {
        await t.tap(find.text('更多选项'));
        await t.pumpAndSettle();
      }
      await t.ensureVisible(digest);
      await t.enterText(digest, 'b' * 64);
      final directory = find.byType(DirectorySelector);
      t.widget<DirectorySelector>(directory).controller.text =
          '/tmp/changed-fixture-only';
      f.mode = 'success';
      final gets = f.scopedGets;
      final query = find.byKey(const ValueKey('manual-batch-query'));
      await t.ensureVisible(query);
      await t.tap(query);
      await spin(
          t,
          () =>
              f.scopedGets > gets &&
              find.text('原批次已确认，当前修改后的草稿已保留。').evaluate().isNotEmpty);
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
      expect(singles, 0);
      expect(t.widget<TextFormField>(digest).controller!.text, 'b' * 64);
      expect(input(t), 'https://fixture.invalid/single');
      expect(
          f.requests.where(
              (r) => (r['uri'] as Uri).path.contains('/task-batches/v1')),
          isEmpty);
    });
  }
  testWidgets(
      'clearing checksum after unknown ACK still only queries original verified operation',
      (t) async {
    f.verified = true;
    f.mode = 'lostAck';
    await checksum(t);
    await confirmChecksum(t);
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, '');
    f.mode = 'success';
    final gets = f.scopedGets;
    await confirmChecksum(t);
    await spin(
        t,
        () =>
            f.scopedGets > gets &&
            !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 1);
    expect(singles, 0);
    expect(journalBox.puts, 1);
    expect(input(t), 'https://fixture.invalid/single');
    expect(find.text('原批次已确认，当前修改后的草稿已保留。'), findsOneWidget);
    expect(
        f.requests
            .where((r) => (r['uri'] as Uri).path.contains('/task-batches/v1')),
        isEmpty);
  });
  testWidgets(
      'verified stop while POST hangs permits original GET; late response cannot clear edited digest',
      (t) async {
    f.verified = true;
    f.postGate = Completer<void>();
    await checksum(t);
    await confirmChecksum(t);
    await spin(t, () => f.posts == 1);
    final stop = find.byKey(const ValueKey('manual-batch-stop'));
    await t.ensureVisible(stop);
    await t.tap(stop);
    await t.pump();
    expect(Get.find<CreateController>().isConfirming.value, false);
    expect(f.postGate!.isCompleted, false);
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, 'b' * 64);
    final gets = f.scopedGets;
    final query = find.byKey(const ValueKey('manual-batch-query'));
    await t.ensureVisible(query);
    await t.tap(query);
    await spin(
        t,
        () =>
            f.scopedGets > gets &&
            find.text('批次结果尚未确认，请查询原批次。').evaluate().isNotEmpty);
    expect(f.posts, 1);
    expect(f.postGate!.isCompleted, false);
    f.postGate!.complete();
    await spin(t, () => f.inFlight == 0 && f.receipts.isNotEmpty);
    await t.pumpAndSettle();
    expect(input(t), 'https://fixture.invalid/single');
    expect(t.widget<TextFormField>(digest).controller!.text, 'b' * 64);
    expect(Get.find<CreateController>().isConfirming.value, false);
    expect(f.posts, 1);
  });
  testWidgets(
      'verified digest edit releases hung confirmation without releasing real Hive fence',
      (t) async {
    f.verified = true;
    journalBox.gate = Completer<void>();
    await checksum(t);
    await confirmChecksum(t);
    await spin(t, () => journalBox.started.isCompleted);
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, 'b' * 64);
    await t.pump();
    expect(Get.find<CreateController>().isConfirming.value, false);
    expect(journalBox.gate!.isCompleted, false);
    expect(f.posts, 0);
    journalBox.gate!.complete();
    await spin(t, () => journalBox.settled.isCompleted);
    expect(f.posts, 0);
    expect(journalBox.puts, 1);
    final query = find.byKey(const ValueKey('manual-batch-query'));
    await t.ensureVisible(query);
    await t.tap(query);
    await spin(t, () => find.text('批次结果尚未确认，请查询原批次。').evaluate().isNotEmpty);
    expect(f.posts, 0);
    expect(t.widget<TextFormField>(digest).controller!.text, 'b' * 64);
  });
  testWidgets(
      'verified dataset change after journal ACK prevents POST and retains reference',
      (t) async {
    f.verified = true;
    journalBox.gate = Completer<void>();
    await checksum(t);
    await confirmChecksum(t);
    await spin(t, () => journalBox.started.isCompleted);
    f.dataset = 'c' * 32;
    journalBox.gate!.complete();
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 1);
    expect(singles, 0);
    expect(input(t), 'https://fixture.invalid/single');
    expect(find.byKey(const ValueKey('manual-batch-query')), findsOneWidget);
  });
  for (final entry in {
    'unsupported receiver': 'https://fixture.invalid/single',
    'multiline': urls,
    'BT': 'magnet:?xt=urn:btih:fixture',
    'ED2K': 'ed2k://|file|fixture|1|abcdef|/',
    'netdisk': 'https://pan.quark.cn/s/fixture',
  }.entries) {
    testWidgets(
        'explicit checksum ${entry.key} sends no legacy request or journal write',
        (t) async {
      if (entry.key == 'unsupported receiver') {
        f.verified = true;
        f.unsupportedPreflight = true;
      }
      await mount(t);
      await t.enterText(
          find.byKey(const ValueKey('panel-download-input')), entry.value);
      await t.tap(find.text('更多选项'));
      await t.pumpAndSettle();
      final digest = find.byKey(const ValueKey('http-expected-sha256'));
      await t.ensureVisible(digest);
      await t.enterText(digest, 'A' * 64);
      await t.tap(find.text('confirm'.tr));
      await t.pump();
      if (entry.key == 'unsupported receiver') {
        await spin(t, () => !Get.find<CreateController>().isConfirming.value);
        expect(f.posts, 0);
      } else {
        expect(f.requests, isEmpty);
      }
      expect(Get.find<CreateController>().isConfirming.value, isFalse);
      expect(journalBox.puts, 0);
      expect(singles, 0);
      expect(
          find.text(entry.key == 'unsupported receiver'
              ? '当前下载服务未确认 SHA-256 校验能力，未提交任务。'
              : 'SHA-256 校验目前仅支持手动单链接 HTTP(S)，未提交任务。'),
          findsOneWidget);
    });
  }
  testWidgets(
      'digest remains bound to its original URL until explicitly edited',
      (t) async {
    await mount(t);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/first');
    await t.tap(find.text('更多选项'));
    await t.pumpAndSettle();
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, 'a' * 64);
    final url = find.byType(TextFormField).first;
    await t.ensureVisible(url);
    await t.enterText(url, 'https://fixture.invalid/changed');
    await t.tap(find.text('confirm'.tr));
    await t.pump();
    expect(find.text('链接已变化，请重新填写当前链接的 SHA-256，未提交任务。'), findsOneWidget);
    expect(t.widget<TextFormField>(digest).controller!.text, 'a' * 64);
    expect(f.requests, isEmpty);
    expect(journalBox.puts, 0);
    expect(singles, 0);
  });
  testWidgets(
      'invalid checksum cannot fall through to resolve or single create',
      (t) async {
    await mount(t);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/single');
    await t.tap(find.text('更多选项'));
    await t.pumpAndSettle();
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, 'a' * 63);
    await t.tap(find.text('confirm'.tr));
    await t.pump();
    expect(f.requests, isEmpty);
    expect(journalBox.puts, 0);
    expect(singles, 0);
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
  });
  testWidgets('explicitly empty checksum preserves the ordinary single path',
      (t) async {
    await mount(t);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/single');
    await t.tap(find.text('更多选项'));
    await t.pumpAndSettle();
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await t.ensureVisible(digest);
    await t.enterText(digest, '  ');
    Get.find<CreateController>().directDownload.value = true;
    await t.tap(find.text('confirm'.tr));
    await spin(t,
        () => singles == 1 && !Get.find<CreateController>().isConfirming.value);
    expect(f.requests, isEmpty);
    expect(journalBox.puts, 0);
  });
  for (final outcome in ['partial', 'lostAck']) {
    testWidgets(
        '$outcome query preserves a new checksum draft without another POST',
        (t) async {
      f.mode = outcome;
      await mount(t);
      final urlController = t
          .widget<TextFormField>(
              find.byKey(const ValueKey('panel-download-input')))
          .controller!;
      await click(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      await t.tap(find.text('更多选项'));
      await t.pumpAndSettle();
      final digest = find.byKey(const ValueKey('http-expected-sha256'));
      await t.ensureVisible(digest);
      await t.enterText(digest, 'a' * 64);
      final op = f.receipts.keys.single;
      f.receipts[op] = f.receipt(op);
      final gets = f.scopedGets;
      final query = find.byKey(const ValueKey('manual-batch-query'));
      await t.ensureVisible(query);
      await t.tap(query);
      await spin(
          t,
          () =>
              f.scopedGets > gets &&
              find.text('原批次已确认，当前修改后的草稿已保留。').evaluate().isNotEmpty);
      expect(urlController.text, urls);
      expect(t.widget<TextFormField>(digest).controller!.text, 'a' * 64);
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
    });
  }
  testWidgets(
      'real button waits journal ACK then consumes exactly one POST and completes input',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    expect(f.posts, 0);
    expect(singles, 0);
    await click(t);
    expect(f.posts, 0);
    journalBox.gate!.complete();
    await spin(t, () => input(t).isEmpty);
    expect(f.posts, 1);
    expect(journalBox.puts, 1);
    expect(singles, 0);
    expect(
        TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey)).refs,
        hasLength(1));
    expect(
        f.requests.where((r) => r['method'] == 'POST').single['body']['reqs'],
        hasLength(2));
  });
  testWidgets(
      'selection-only changes during journal ACK preserve the one original submit',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    final field = t.widget<TextFormField>(
        find.byKey(const ValueKey('panel-download-input')));
    field.controller!.selection = const TextSelection.collapsed(offset: 0);
    await t.pump();
    journalBox.gate!.complete();
    await spin(t, () => input(t).isEmpty);
    expect(f.posts, 1);
    expect(journalBox.puts, 1);
  });
  for (final failure in ['failed', 'ackUnknown']) {
    testWidgets(
        'journal $failure sends zero POST, repeated confirm only queries same reference',
        (t) async {
      journalBox.fail = failure == 'failed';
      if (failure == 'ackUnknown') journalBox.gate = Completer<void>();
      await mount(t, timeout: const Duration(milliseconds: 30));
      await click(t);
      await spin(
          t,
          () =>
              journalBox.started.isCompleted &&
              !Get.find<CreateController>().isConfirming.value);
      expect(f.posts, 0);
      expect(input(t), urls);
      final gets = f.scopedGets;
      if (journalBox.gate != null) {
        journalBox.gate!.complete();
        await t.runAsync(
            () => Future<void>.delayed(const Duration(milliseconds: 20)));
      }
      await click(t);
      await spin(
          t,
          () =>
              f.scopedGets > gets &&
              !Get.find<CreateController>().isConfirming.value);
      expect(f.posts, 0);
      expect(journalBox.puts, 1);
      expect(input(t), urls);
    });
  }
  testWidgets(
      'stop waiting during journal write retains reference and sends zero POST',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    await t.tap(find.byKey(const ValueKey('manual-batch-stop')));
    await t.pump();
    journalBox.gate!.complete();
    await spin(t, () => journalBox.settled.isCompleted);
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
    expect(f.posts, 0);
    expect(input(t), urls);
    expect(
        TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey)).refs,
        hasLength(1));
  });
  testWidgets(
      'stop releases confirmation before journal settles and late completion cannot reset a new query',
      (t) async {
    journalBox.gate = Completer<void>();
    final queryGate = Completer<void>();
    addTearDown(() {
      if (!queryGate.isCompleted) queryGate.complete();
    });
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    await t.tap(find.byKey(const ValueKey('manual-batch-stop')));
    await t.pump();
    final controller = Get.find<CreateController>();
    expect(controller.isConfirming.value, isFalse);
    expect(journalBox.gate!.isCompleted, isFalse);
    final gets = f.scopedGets;
    f.override = (req) async {
      if (req.method == 'GET' && req.uri.path.contains('/datasets/')) {
        await queryGate.future;
      }
      await f.handle(req);
    };
    await click(t);
    await spin(t, () => f.scopedGets > gets);
    expect(controller.isConfirming.value, isTrue);
    journalBox.gate!.complete();
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 30)));
    await t.pump();
    expect(controller.isConfirming.value, isTrue);
    expect(f.posts, 0);
    expect(journalBox.puts, 1);
    queryGate.complete();
    await spin(t, () => !controller.isConfirming.value);
    expect(input(t), urls);
    expect(
        TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey)).refs,
        hasLength(1));
  });
  testWidgets(
      'stop during POST permits query while original POST remains pending',
      (t) async {
    f.postGate = Completer<void>();
    final handled = Completer<void>();
    f.override = (req) async {
      await f.handle(req);
      if (req.method == 'POST') handled.complete();
    };
    await mount(t);
    await click(t);
    await spin(t, () => f.posts == 1);
    await t.tap(find.byKey(const ValueKey('manual-batch-stop')));
    await t.pump();
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
    expect(f.postGate!.isCompleted, isFalse);
    final gets = f.scopedGets;
    await t.tap(find.byKey(const ValueKey('manual-batch-query')));
    await spin(t, () => f.scopedGets > gets);
    expect(f.posts, 1);
    f.postGate!.complete();
    await spin(t, () => handled.isCompleted);
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
    await t.pumpAndSettle();
    expect(input(t), urls);
    expect(f.posts, 1);
    expect(journalBox.puts, 1);
  });
  testWidgets(
      'stop during identity preflight claims no recorded reference and sends zero POST',
      (t) async {
    final gate = Completer<void>();
    final handled = Completer<void>();
    f.override = (req) async {
      if (req.uri.path == '/api/v1/dataset/identity') await gate.future;
      await f.handle(req);
      handled.complete();
    };
    await mount(t);
    await click(t);
    await spin(t, () => f.requests.isNotEmpty);
    await t.tap(find.byKey(const ValueKey('manual-batch-stop')));
    await t.pump();
    gate.complete();
    await spin(t, () => handled.isCompleted);
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
    await t.pumpAndSettle();
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
    expect(find.text('已停止本次提交，未发送创建请求。'), findsOneWidget);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
    expect(input(t), urls);
  });
  for (final stage in ['identity', 'preflight', 'journal', 'POST']) {
    testWidgets(
        'editing input during pending $stage releases only the old confirmation',
        (t) async {
      final gate = Completer<void>();
      final handled = Completer<void>();
      var waiting = false;
      addTearDown(() {
        if (!gate.isCompleted) gate.complete();
      });
      if (stage == 'journal') journalBox.gate = gate;
      f.override = (req) async {
        final hold = !waiting &&
            ((stage == 'identity' &&
                    req.uri.path == '/api/v1/dataset/identity') ||
                (stage == 'preflight' &&
                    req.method == 'GET' &&
                    req.uri.path.contains('/datasets/')) ||
                (stage == 'POST' && req.method == 'POST'));
        if (hold) {
          waiting = true;
          await gate.future;
        }
        await f.handle(req);
        if (hold) handled.complete();
      };
      singleGate = Completer<String>();
      await mount(t);
      await click(t);
      await spin(t,
          () => stage == 'journal' ? journalBox.started.isCompleted : waiting);
      final controller = Get.find<CreateController>();
      expect(controller.isConfirming.value, isTrue);
      const next = 'https://fixture.invalid/next-draft';
      await t.enterText(
          find.byKey(const ValueKey('panel-download-input')), next);
      await t.pump();
      expect(gate.isCompleted, isFalse);
      expect(controller.isConfirming.value, isFalse);
      controller.directDownload.value = true;
      await click(t);
      await spin(t, () => singles == 1);
      expect(controller.isConfirming.value, isTrue);
      gate.complete();
      await spin(
          t,
          () => stage == 'journal'
              ? journalBox.settled.isCompleted
              : handled.isCompleted);
      await t.runAsync(
          () => Future<void>.delayed(const Duration(milliseconds: 20)));
      await t.pump(const Duration(milliseconds: 20));
      expect(controller.isConfirming.value, isTrue);
      expect(input(t), next);
      expect(MacPanel.taskResult.value, isNull);
      expect(f.posts, stage == 'POST' ? 1 : 0);
      expect(journalBox.puts, stage == 'journal' || stage == 'POST' ? 1 : 0);
      singleGate!.complete('new-single');
      await spin(t, () => !controller.isConfirming.value && input(t).isEmpty);
      expect(singles, 1);
      expect(f.posts, stage == 'POST' ? 1 : 0);
      expect(
          TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey))
              .refs,
          hasLength(stage == 'journal' || stage == 'POST' ? 1 : 0));
    });
  }
  testWidgets('changed input discards late ACK and preserves the new draft',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/new');
    journalBox.gate!.complete();
    await spin(t, () => journalBox.settled.isCompleted);
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
    expect(f.posts, 0);
    expect(input(t), 'https://fixture.invalid/new');
    expect(journalBox.puts, 1);
  });
  for (final change in ['dataset', 'route']) {
    testWidgets(
        '$change change during journal ACK stops first POST and retains original scope',
        (t) async {
      journalBox.gate = Completer<void>();
      await mount(t);
      await click(t);
      await spin(t, () => journalBox.started.isCompleted);
      if (change == 'dataset') {
        f.dataset = 'c' * 32;
      } else {
        f.init('c');
      }
      journalBox.gate!.complete();
      await spin(t, () => !Get.find<CreateController>().isConfirming.value);
      expect(f.posts, 0);
      expect(input(t), urls);
      final ref =
          TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey))
              .refs
              .single;
      expect(ref.datasetId, 'a' * 32);
      expect(ref.route.routeScope, 'b' * 64);
    });
  }
  testWidgets('dispose during journal wait never submits after unmount',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    await t.pumpWidget(const SizedBox.shrink());
    journalBox.gate!.complete();
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 1);
  });
  for (final outcome in ['lostAck', 'html', 'partial']) {
    testWidgets(
        '$outcome preserves complete draft; original batch query never repeats POST',
        (t) async {
      f.mode = outcome;
      await mount(t);
      await click(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      expect(input(t), urls);
      expect(journalBox.puts, 1);
      final gets = f.scopedGets;
      await click(t);
      await spin(
          t,
          () =>
              f.scopedGets > gets &&
              !Get.find<CreateController>().isConfirming.value);
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
      if (outcome == 'partial') {
        expect(input(t), urls);
      } else {
        expect(input(t), isEmpty);
      }
    });
  }
  for (final outcome in ['partial', 'lostAck']) {
    testWidgets(
        '$outcome original query ignores edited options and preserves the changed draft',
        (t) async {
      f.mode = outcome;
      await mount(t);
      await click(t);
      await spin(
          t,
          () =>
              f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
      final urlController = t
          .widget<TextFormField>(
              find.byKey(const ValueKey('panel-download-input')))
          .controller!;
      await t.tap(find.text('更多选项'));
      await t.pumpAndSettle();
      final changedPath = '${dir.path}/changed';
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(
              MacPanel.channel,
              (call) async =>
                  call.method == 'chooseDirectory' ? changedPath : null);
      final folder = find.descendant(
          of: find.byType(DirectorySelector),
          matching: find.byIcon(CupertinoIcons.folder));
      await t.ensureVisible(folder);
      await t.tap(folder);
      await t.pumpAndSettle();
      final directory =
          t.widget<DirectorySelector>(find.byType(DirectorySelector));
      expect(directory.controller.text, changedPath);
      final connections = find
          .byWidgetPredicate(
              (w) => w is TextField && w.keyboardType == TextInputType.number)
          .first;
      await t.ensureVisible(connections);
      await t.enterText(connections, '7');
      final header = find
          .byWidgetPredicate((w) =>
              w is TextField && w.decoration?.hintText == 'httpHeaderValue')
          .first;
      await t.ensureVisible(header);
      await t.enterText(header, 'changed-header');
      await t.pump();
      final op = f.receipts.keys.single;
      f.receipts[op] = f.receipt(op);
      final gets = f.scopedGets;
      final query = find.byKey(const ValueKey('manual-batch-query'));
      await t.ensureVisible(query);
      await t.tap(query);
      await spin(t, () => f.scopedGets > gets);
      await spin(
          t, () => find.text('原批次已确认，当前修改后的草稿已保留。').evaluate().isNotEmpty);
      expect(urlController.text, urls);
      expect(directory.controller.text, changedPath);
      expect(t.widget<TextField>(connections).controller!.text, '7');
      expect(t.widget<TextField>(header).controller!.text, 'changed-header');
      expect(f.posts, 1);
      expect(journalBox.puts, 1);
      expect(
          f.requests
              .lastWhere((r) =>
                  r['method'] == 'GET' &&
                  (r['uri'] as Uri).path.contains('/datasets/'))['uri']
              .pathSegments
              .last,
          op);
    });
  }
  testWidgets(
      'unsupported preflight and Mem identity never write journal or POST',
      (t) async {
    f.unsupportedPreflight = true;
    await mount(t);
    await click(t);
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
    f.unsupportedPreflight = false;
    f.persistent = false;
    await click(t);
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
  });
  testWidgets(
      'conflicting intent keeps its draft and never reports prior IDs as a fresh success',
      (t) async {
    f.override = (req) async {
      if (req.method == 'POST') {
        final op = f.requests.last['body']['operationId'] as String;
        await f.respond(req, f.wrapper(f.receipt(op)),
            status: 409, code: 1000, msg: 'operation_conflict');
      } else {
        await f.handle(req);
      }
    };
    await mount(t);
    await click(t);
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    expect(input(t), urls);
    expect(MacPanel.taskResult.value, isNull);
    expect(find.textContaining('与原批次冲突'), findsOneWidget);
    expect(f.posts, 1);
  });
  testWidgets(
      'non-success preflight never grants a POST even with a matching wrapper',
      (t) async {
    f.override = (req) async {
      if (req.method == 'GET' && req.uri.path.contains('/datasets/')) {
        final op = Uri.decodeComponent(req.uri.pathSegments.last);
        await f.respond(req, f.wrapper(f.receipt(op, state: 'unknown')),
            status: 503,
            code: 1000,
            msg: 'operation_unavailable_query_same_id');
      } else {
        await f.handle(req);
      }
    };
    await mount(t);
    await click(t);
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
    expect(input(t), urls);
  });
  testWidgets(
      'late POST reply cannot clear a changed draft or publish old task feedback',
      (t) async {
    f.postGate = Completer<void>();
    final handled = Completer<void>();
    f.override = (req) async {
      await f.handle(req);
      if (req.method == 'POST') handled.complete();
    };
    await mount(t);
    await click(t);
    await spin(t, () => f.posts == 1);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/new');
    f.postGate!.complete();
    await spin(t, () => handled.isCompleted);
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
    await t.pumpAndSettle();
    expect(Get.find<CreateController>().isConfirming.value, isFalse);
    expect(input(t), 'https://fixture.invalid/new');
    expect(MacPanel.taskResult.value, isNull);
    expect(f.posts, 1);
  });
  testWidgets(
      'path opt-in waits for capability and journals one POST; changed intent only queries',
      (t) async {
    var supported = false;
    const caps = [httpPathCapability, 'http-sha256-completion/v1'];
    f.override = (q) async {
      final root = '/api/v1/datasets/${f.dataset}/http-path/v1';
      if (!q.uri.path.startsWith(root)) {
        await f.handle(q);
        return;
      }
      if (q.uri.path.endsWith('/capability')) {
        if (!supported) {
          q.response.statusCode = 404;
          q.response.write('<html>old</html>');
          await q.response.close();
          return;
        }
        await f.respond(q, {
          'scopeVersion': 1,
          'datasetId': f.dataset,
          'kind': httpPathCreateKind,
          'requestVersion': 1,
          'supported': true,
          'requiredCapabilities': caps,
          'methods': {
            'create': 'POST',
            'query': 'GET',
            'cancel': 'POST',
            'preview': 'POST'
          }
        });
      } else if (q.method == 'POST') {
        expect(journalBox.puts, 1,
            reason: 'journal ACK must precede first POST');
        final op = (f.requests.last['body'] as Map)['operationId'] as String;
        f.receipts[op] = f.receipt(op, count: 1);
        await f.respond(q, null,
            status: 503,
            code: 1000,
            msg: 'operation_unavailable_query_same_id');
      } else {
        final op = Uri.decodeComponent(q.uri.pathSegments.last);
        await f.respond(q, {
          ...f.wrapper(f.receipts[op] ?? f.receipt(op, state: 'unknown')),
          'kind': httpPathCreateKind,
          'requiredCapabilities': f.receipts.containsKey(op) ? caps : []
        });
      }
    };
    await checksum(t);
    final toggle = find.byKey(const ValueKey('http-path-opt-in'));
    Future<void> togglePath(bool value) async {
      final checkbox =
          find.descendant(of: toggle, matching: find.byType(Checkbox));
      await t.ensureVisible(checkbox);
      await t.pumpAndSettle();
      expect(checkbox.hitTestable(), findsOneWidget,
          reason: 'the real opt-in checkbox must be rendered and hittable');
      await t.tap(checkbox);
      await t.pump();
      expect(t.widget<CheckboxListTile>(toggle).value, value);
    }

    await togglePath(true);
    await confirmChecksum(t);
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
    supported = true;
    await confirmChecksum(t);
    await spin(t,
        () => f.posts == 1 && !Get.find<CreateController>().isConfirming.value);
    final body =
        f.requests.firstWhere((q) => q['method'] == 'POST')['body'] as Map;
    expect(body['requiredCapabilities'], caps);
    expect(body['pathIntent'],
        {'version': 1, 'path': dir.path, 'origin': 'default'});
    expect(body['request']['opts']['path'], dir.path);
    expect(body['integrity']['expectedSHA256'], 'a' * 64);
    expect(singles, 0);
    final op = body['operationId'];
    await togglePath(false);
    final gets = f.requests
        .where((q) => q['method'] == 'GET' && q['uri'].path.endsWith('/$op'))
        .length;
    final query = find.text('查询原批次');
    await t.ensureVisible(query);
    await t.tap(query);
    await spin(
        t,
        () =>
            f.requests
                .where((q) =>
                    q['method'] == 'GET' && q['uri'].path.endsWith('/$op'))
                .length >
            gets);
    await t
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
    await t.pump();
    expect(f.posts, 1);
    expect(input(t), 'https://fixture.invalid/single');
    expect(MacPanel.taskResult.value, isNull);
  });
  testWidgets('BT path selection uses preview before a journaled single POST',
      (t) async {
    const variant = PathCreateVariant.bt;
    var posted = false;
    f.override = (q) async {
      if (!q.uri.path.contains('/bt-path/v1/')) {
        await f.handle(q);
        return;
      }
      if (q.uri.path.endsWith('/capability')) {
        await f.respond(q, {
          'scopeVersion': 1,
          'datasetId': f.dataset,
          'kind': variant.kind,
          'requestVersion': 1,
          'supported': true,
          'requiredCapabilities': [variant.capability],
          'methods': {
            'create': 'POST',
            'query': 'GET',
            'cancel': 'POST',
            'preview': 'POST'
          }
        });
      } else if (q.uri.path.endsWith('/preview')) {
        expect(journalBox.puts, 0);
        await f.respond(q, {
          'version': 1,
          'decision': {
            'version': 1,
            'root': dir.path,
            'origin': 'default',
            'ruleId': '',
            'ruleVersion': null
          },
          'name': 'fixture-tree',
          'layoutHash': 'a' * 64,
          'layout': {
            'version': 1,
            'parentRoot': dir.path,
            'members': [
              {'index': 0, 'relativePath': 'tree/a.bin', 'selected': true},
              {'index': 1, 'relativePath': 'tree/b.bin', 'selected': true},
            ]
          }
        });
      } else {
        final op = q.method == 'POST'
            ? f.requests.last['body']['operationId'] as String
            : q.uri.pathSegments.last;
        if (q.method == 'POST') {
          expect(journalBox.puts, 1);
          expect(
              f.requests.last['body']['request']['opts']['selectFiles'], [1]);
          posted = true;
          await f.respond(q, null,
              status: 503, code: 1000, msg: 'query_required');
        } else {
          await f.respond(q, {
            ...f.wrapper(f.receipt(op, state: 'unknown')),
            'kind': variant.kind,
            'requiredCapabilities': posted ? [variant.capability] : []
          });
        }
      }
    };
    await mount(t);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'magnet:?xt=urn:btih:0123456789012345678901234567890123456789');
    await t.tap(find.text('更多选项'));
    await t.pumpAndSettle();
    final toggle = find.descendant(
        of: find.byKey(const ValueKey('http-path-opt-in')),
        matching: find.byType(Checkbox));
    await t.ensureVisible(toggle);
    await t.pumpAndSettle();
    await t.tap(toggle);
    await t.pump();
    expect(Get.find<CreateController>().directDownload.value, false);
    await confirmChecksum(t);
    final selection = find.byKey(const ValueKey('path-native-file-selection'));
    await spin(t, () => selection.evaluate().isNotEmpty);
    expect(journalBox.puts, 0);
    expect(
        f.requests.where((q) =>
            q['method'] == 'POST' && q['uri'].path.endsWith('/operations')),
        isEmpty);
    await t.tap(find.byKey(const ValueKey('path-member-0')));
    await t.pump();
    await t
        .tap(find.descendant(of: selection, matching: find.text('confirm'.tr)));
    await spin(
        t, () => posted && !Get.find<CreateController>().isConfirming.value);
    await queryOriginal(t);
    expect(
        f.requests.where((q) =>
            q['method'] == 'POST' && q['uri'].path.endsWith('/operations')),
        hasLength(1));
    expect(
        f.requests.where((q) => q['uri'].path.contains('/resolve')), isEmpty);
    expect(journalBox.puts, 1);
    expect(singles, 0);
  });
  testWidgets(
      'single and mixed BT multi retain their original single creator path',
      (t) async {
    await mount(t);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/one\nmagnet:?xt=urn:btih:fixture');
    await click(t);
    await spin(t,
        () => singles == 2 && !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
  });
}
