import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
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
  JournalBox(this.inner);
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      inner.get(key, defaultValue: defaultValue);
  @override
  Future<void> put(dynamic key, dynamic value) async {
    puts++;
    if (!started.isCompleted) started.complete();
    if (gate != null) await gate!.future;
    if (fail) throw StateError('fixture journal failure');
    await ioZone.run(() => inner.put(key, value));
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
      {Duration timeout = const Duration(seconds: 2)}) async {
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
          page: () => CreateView(
              batchJournal:
                  TaskBatchJournalStore(box: journalBox, capacity: 10),
              batchAcknowledgementTimeout: timeout,
              taskCreator: (CreateTask task) async {
                singles++;
                return 'single-$singles';
              }))
    ]));
    await t.pumpAndSettle();
    await t.enterText(find.byKey(const ValueKey('panel-download-input')), urls);
    await t.pump();
  }

  Future<void> click(WidgetTester t) async {
    await t.tap(find.byTooltip('开始下载'));
    await t.pump();
  }

  String input(WidgetTester t) => t
      .widget<TextFormField>(find.byKey(const ValueKey('panel-download-input')))
      .controller!
      .text;
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
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(f.posts, 0);
    expect(input(t), urls);
    expect(
        TaskBatchJournal.decode(box.get(TaskBatchJournalStore.storageKey)).refs,
        hasLength(1));
  });
  testWidgets(
      'stop during identity preflight claims no recorded reference and sends zero POST',
      (t) async {
    final gate = Completer<void>();
    f.override = (req) async {
      if (req.uri.path == '/api/v1/dataset/identity') await gate.future;
      await f.handle(req);
    };
    await mount(t);
    await click(t);
    await spin(t, () => f.requests.isNotEmpty);
    await t.tap(find.byKey(const ValueKey('manual-batch-stop')));
    await t.pump();
    gate.complete();
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(find.text('已停止本次提交，未发送创建请求。'), findsOneWidget);
    expect(f.posts, 0);
    expect(journalBox.puts, 0);
    expect(input(t), urls);
  });
  testWidgets('changed input discards late ACK and preserves the new draft',
      (t) async {
    journalBox.gate = Completer<void>();
    await mount(t);
    await click(t);
    await spin(t, () => journalBox.started.isCompleted);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/new');
    journalBox.gate!.complete();
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
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
    await mount(t);
    await click(t);
    await spin(t, () => f.posts == 1);
    await t.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://fixture.invalid/new');
    f.postGate!.complete();
    await spin(t, () => !Get.find<CreateController>().isConfirming.value);
    expect(input(t), 'https://fixture.invalid/new');
    expect(MacPanel.taskResult.value, isNull);
    expect(f.posts, 1);
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
