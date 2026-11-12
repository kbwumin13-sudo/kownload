import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/browser_handoff.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/http_path_create.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/services/browser_handoff.dart';
import 'package:gopeed/app/services/browser_integration.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'support/scoped_task_batch_fixture.dart';

final download = {
  'id': 10,
  'startTime': '2026-11-11T01:00:00.000Z',
  'urlHash': List.filled(64, 'a').join()
};
Map<String, dynamic> prepare([String id = 'capture']) => {
      'action': 'handoffPrepare',
      'operationId': id,
      'clientId': 'chrome-one',
      'browser': 'chrome',
      'download': download,
      'req': {
        'url': 'https://fixture.test/$id',
        'extra': {
          'header': {'Cookie': 'synthetic-only'}
        }
      },
      'opts': {'name': 'file.bin'}
    };
Map<String, dynamic> query([String id = 'capture']) => {
      'action': 'handoffQuery',
      'operationId': id,
      'clientId': 'chrome-one',
      'browser': 'chrome'
    };

class Core {
  final records = <String, Map<String, dynamic>>{},
      identities = <String, String>{};
  final calls = <Map<String, dynamic>>[];
  int creates = 0, starts = 0;
  bool unavailable = false,
      unknownQueries = false,
      lostPrepare = false,
      lostCommit = false;
  Future<void>? prepareGate;
  Future<Map<String, dynamic>> call(
      String method, Map<String, dynamic> data) async {
    calls.add({'method': method, ...data});
    if (unavailable) throw StateError('core unavailable');
    if (method == 'GET') return {'version': 1};
    final id = data['operationId'] as String?, action = data['action'];
    Map<String, dynamic> unknown() => {
          'version': 1,
          'operationId': id,
          'state': 'unknown',
          'taskIds': <String>[]
        };
    if (action == 'query' && data['pending'] == true)
      return {
        'version': 1,
        'items': records.values.where((r) => r['state'] == 'prepared').toList()
      };
    if (id == null) throw const FormatException();
    final identity = '${data['clientId']}|${data['browser']}';
    if (identities[id] != null && identities[id] != identity)
      throw const FormatException('identity conflict');
    if (action == 'query')
      return Map.from(unknownQueries ? unknown() : records[id] ?? unknown());
    if (action == 'prepare') {
      await prepareGate;
      final result = records.putIfAbsent(id, () {
        creates++;
        identities[id] = identity;
        return {
          'version': 1,
          'operationId': id,
          'state': 'prepared',
          'taskId': 'task-$id',
          'taskIds': ['task-$id'],
          'download': data['download']
        };
      });
      if (lostPrepare) throw StateError('lost prepare reply');
      return Map.from(result);
    }
    final result = records[id];
    if (result == null) return unknown();
    if (action == 'abort' && result['state'] == 'prepared') {
      result['state'] = 'aborted';
      result['reason'] = data['reason'];
    }
    if (action == 'commit' && result['state'] == 'prepared') {
      if (data['cancelConfirmed'] != true ||
          data['browserState']['state'] != 'interrupted' ||
          data['browserState']['error'] != 'USER_CANCELED')
        throw const FormatException('handoff not proved');
      result['state'] = 'committed';
      starts++;
    }
    if (action == 'commit' && lostCommit) throw StateError('lost commit reply');
    return Map.from(result);
  }

  Task task([String id = 'capture']) => Task.fromJson({
        'id': 'task-$id',
        'name': 'file.bin',
        'status': 'pause',
        'protocol': 'http',
        'meta': {
          'req': {'url': 'https://fixture.test/$id'},
          'opts': {'path': '/tmp/test'}
        },
        'uploading': false,
        'progress': {
          'used': 0,
          'speed': 0,
          'downloaded': 0,
          'uploadSpeed': 0,
          'uploaded': 0
        },
        'createdAt': '2026-11-11T01:00:00Z',
        'updatedAt': '2026-11-11T01:00:00Z',
        'handoff': {
          'version': 1,
          'operationId': id,
          'clientId': 'chrome-one',
          'browser': 'chrome',
          'phase': records[id]?['state'] ?? 'prepared',
          'requestHash': List.filled(64, 'b').join(),
          'download': download,
          if (records[id]?['reason'] != null) 'reason': records[id]!['reason']
        }
      });
}

void main() {
  late Core core;
  late BrowserHandoff app;
  late List<CreateTask> confirmations;
  late List<String> ended;
  var confirm = true, enabled = true;
  setUp(() {
    core = Core();
    confirmations = [];
    ended = [];
    confirm = true;
    enabled = true;
    app = BrowserHandoff(
        request: core.call,
        preferences: (_, __) => {'enabled': enabled, 'confirm': confirm},
        confirm: (task) async {
          confirmations.add(task);
        },
        ended: (id, reason) => ended.add('$id:$reason'));
  });
  testWidgets(
      'typed capture keeps original operation and queries an unknown prepare before commit',
      (tester) async {
    final fixture = ScopedBatchFixture();
    HttpOverrides.global = null;
    await tester.runAsync(fixture.start);
    addTearDown(() => fixture.close());
    var carrier = false;
    const caps = [httpPathCapability, 'http-sha256-completion/v1'];
    fixture.override = (q) async {
      if (q.uri.path.endsWith('/http-path/v1/capability')) {
        await fixture.respond(q, {
          'scopeVersion': 1,
          'datasetId': fixture.dataset,
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
        return;
      }
      if (q.uri.path != '/api/v1/browser/handoff') {
        await fixture.handle(q);
        return;
      }
      if (q.method == 'GET') {
        await fixture
            .respond(q, {'version': 1, if (carrier) 'pathCreateVersion': 1});
        return;
      }
      final data = Map<String, dynamic>.from(fixture.requests.last['body']);
      final result = await core.call(q.method, data);
      final typed = {
        ...result,
        'pathCreate': {
          'version': 1,
          'datasetId': fixture.dataset,
          'kind': httpPathCreateKind,
          'requiredCapabilities': result['state'] == 'unknown' ? [] : caps
        }
      };
      // A real HTTP failure after this test backend retained prepared state.
      // This is an unknown acknowledgement response, not an injected lost ACK.
      await fixture.respond(q, typed,
          status: data['action'] == 'prepare' ? 503 : 200,
          code: data['action'] == 'prepare' ? 1000 : 0,
          msg: data['action'] == 'prepare' ? 'query_required' : '');
    };
    final ui = await mountHandoffUi(tester, core);
    await ui.flow.command(prepare());
    await tester.pumpAndSettle();
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    final digest = find.byKey(const ValueKey('http-expected-sha256'));
    await tester.ensureVisible(digest);
    await tester.enterText(digest, 'A' * 64);
    final toggle = find.descendant(
        of: find.byKey(const ValueKey('http-path-opt-in')),
        matching: find.byType(Checkbox));
    await tester.ensureVisible(toggle);
    await tester.pumpAndSettle();
    await tester.tap(toggle);
    await tester.pump();
    Future<void> confirm() async {
      final button = find.text('confirm'.tr);
      await tester.ensureVisible(button);
      await tester.tap(button);
      await tester.pump();
      for (var i = 0;
          i < 150 && Get.find<CreateController>().isConfirming.value;
          i++) {
        await tester.runAsync(
            () => Future<void>.delayed(const Duration(milliseconds: 10)));
        await tester.pump(const Duration(milliseconds: 20));
      }
      expect(Get.find<CreateController>().isConfirming.value, false);
    }

    await confirm();
    expect(core.creates, 0, reason: 'old carrier may not ignore typed fields');
    carrier = true;
    await confirm();
    expect(core.creates, 1);
    expect(core.starts, 0,
        reason: 'prepare is paused and has no IO permission');
    final sent = core.calls.singleWhere((q) => q['action'] == 'prepare');
    expect(sent['datasetId'], fixture.dataset);
    expect(sent['operationId'], 'capture');
    expect(sent['download'], download);
    expect(sent['pathCreate']['operationId'], 'capture');
    expect(sent['pathCreate']['requiredCapabilities'], caps);
    expect(sent['pathCreate']['integrity']['expectedSHA256'], 'a' * 64);
    expect(sent['pathCreate']['request'],
        {'req': sent['req'], 'opts': sent['opts']});
    expect(sent['req']['extra']['header']['Cookie'], 'synthetic-only');
    expect(tester.widget<TextFormField>(digest).controller!.text, 'A' * 64);
    await confirm();
    expect(core.creates, 1);
    expect(core.calls.where((q) => q['action'] == 'prepare'), hasLength(1));
    expect(core.starts, 0);
    final committed = await tester.runAsync(() => ui.flow.command({
          ...query(),
          'action': 'handoffCommit',
          'taskId': 'task-capture',
          'download': download,
          'browserState': {'state': 'interrupted', 'error': 'USER_CANCELED'},
          'cancelConfirmed': true,
        }));
    expect(committed!['state'], 'committed');
    expect(core.starts, 1);
    expect(core.calls.singleWhere((q) => q['action'] == 'commit')['datasetId'],
        fixture.dataset);
    expect(
        fixture.requests.where(
            (q) => q['method'] == 'POST' && q['body']?['action'] == 'prepare'),
        hasLength(1));
    expect(ui.createdReceipts, 0);
  });
  test(
      'canonical preference controls next prepare; prepared is not created and old tasks retain JSON',
      () async {
    expect((await app.command(prepare()))['state'], 'accepted');
    expect(core.creates, 0);
    expect(confirmations, hasLength(1));
    final t = confirmations.single;
    expect(jsonEncode(t.toJson()), isNot(contains('browserHandoff')));
    expect(t.browserHandoff, isNotNull);
    final result = await app.prepareConfirmed('capture', t.browserHandoff!, t);
    expect(result.state, 'prepared');
    expect(core.creates, 1);
    expect(core.starts, 0);
    expect((await app.command(query()))['state'], 'prepared');
    confirm = false;
    expect((await app.command(prepare('auto')))['state'], 'prepared');
    expect(confirmations, hasLength(1));
    expect(core.starts, 0);
    enabled = false;
    await expectLater(app.command(prepare('disabled')), throwsStateError);
    final task = core.task();
    expect(task.status, Status.pause);
    expect(task.handoff!.blocked, true);
    expect(task.handoff!.label, '待浏览器交接');
    final old = task.toJson()..remove('handoff');
    expect(Task.fromJson(old).toJson(), old);
    final future = Task.fromJson({
      ...task.toJson(),
      'handoff': {'version': 2, 'phase': 'other'}
    });
    expect(future.handoff!.blocked, true);
  });
  test(
      'query never recreates unknown; restart reads durable prepared or committed',
      () async {
    confirm = false;
    await app.command(prepare());
    final restarted = BrowserHandoff(
        request: core.call,
        preferences: (_, __) => {'enabled': false, 'confirm': true},
        confirm: (_) async {
          fail('must not reopen confirmation');
        });
    expect((await restarted.command(query()))['state'], 'prepared');
    expect((await restarted.command(query('deleted')))['state'], 'unknown');
    expect(core.creates, 1);
    await restarted.command({
      ...query(),
      'action': 'handoffCommit',
      'taskId': 'task-capture',
      'download': download,
      'cancelConfirmed': true,
      'browserState': {'state': 'interrupted', 'error': 'USER_CANCELED'}
    });
    expect((await restarted.command(query()))['state'], 'committed');
    expect(core.starts, 1);
    expect(core.creates, 1);
    await restarted.command({
      ...query(),
      'action': 'handoffCommit',
      'taskId': 'task-capture',
      'download': download,
      'cancelConfirmed': true,
      'browserState': {'state': 'interrupted', 'error': 'USER_CANCELED'}
    });
    expect(core.starts, 1);
  });
  test(
      'prepare reply loss then unavailable query cannot recreate or start a held task',
      () async {
    confirm = false;
    core.lostPrepare = true;
    await expectLater(app.command(prepare()), throwsStateError);
    expect(core.creates, 1);
    expect(core.starts, 0);
    core.unknownQueries = true;
    core.lostPrepare = false;
    expect((await app.command(query()))['state'], 'unknown');
    expect((await app.command(prepare()))['state'], 'unknown');
    expect(core.creates, 1);
    expect(core.starts, 0);
    core.unknownQueries = false;
    expect((await app.command(query()))['state'], 'prepared');
  });
  test(
      'commit ACK loss never becomes a local cancellation and query recovers the same committed task',
      () async {
    confirm = false;
    await app.command(prepare());
    core.lostCommit = true;
    await expectLater(
        app.command({
          ...query(),
          'action': 'handoffCommit',
          'taskId': 'task-capture',
          'download': download,
          'cancelConfirmed': true,
          'browserState': {'state': 'interrupted', 'error': 'USER_CANCELED'}
        }),
        throwsStateError);
    expect(core.starts, 1);
    core.unavailable = true;
    await expectLater(
        app.command({
          ...query(),
          'action': 'handoffAbort',
          'reason': 'browser_completed'
        }),
        throwsStateError);
    expect(ended, isEmpty);
    core.unavailable = false;
    expect((await app.command(query()))['state'], 'committed');
    expect(core.creates, 1);
    expect(core.starts, 1);
    final prefixed = BrowserHandoffResult.fromJson({
      'version': 1,
      'operationId': 'prefix',
      'state': 'prepared',
      'taskId': '-_valid-task',
      'taskIds': ['-_valid-task']
    });
    expect(prefixed.taskIds, ['-_valid-task']);
    final local = {
      'version': 1,
      'operationId': 'cancel',
      'state': 'aborted',
      'taskIds': <String>[],
      'reason': 'user_cancelled'
    };
    expect(() => BrowserHandoffResult.fromJson(local), throwsFormatException);
    expect(BrowserHandoffResult.fromJson(local, allowLocalAbort: true).taskIds,
        isEmpty);
  });
  test(
      'abort ends an accepted pending even if core unknown; late UI cannot prepare',
      () async {
    await app.command(prepare());
    final task = confirmations.single;
    await expectLater(
        app.command({
          ...query(),
          'action': 'handoffAbort',
          'clientId': 'other',
          'reason': 'browser_completed'
        }),
        throwsFormatException);
    expect((await app.command(query()))['state'], 'accepted');
    final abort = await app.command(
        {...query(), 'action': 'handoffAbort', 'reason': 'browser_completed'});
    expect(abort['state'], 'unknown');
    expect(ended, ['capture:browser_completed']);
    expect(
        (await app.prepareConfirmed('capture', task.browserHandoff!, task))
            .state,
        'aborted');
    expect((await app.command(query()))['state'], 'aborted');
    expect(core.creates, 0);
    expect(core.starts, 0);
  });
  test('abort races a prepare in flight and compensates a late prepared task',
      () async {
    await app.command(prepare());
    final task = confirmations.single;
    final gate = Completer<void>();
    core.prepareGate = gate.future;
    final pending = app.prepareConfirmed('capture', task.browserHandoff!, task);
    await Future<void>.delayed(Duration.zero);
    await app.command(
        {...query(), 'action': 'handoffAbort', 'reason': 'browser_completed'});
    gate.complete();
    expect((await pending).state, 'aborted');
    expect(core.creates, 1);
    expect(core.starts, 0);
    expect(core.records['capture']!['state'], 'aborted');
  });
  test(
      'concurrent prepare coalesces confirmation and rejects another request identity',
      () async {
    final replies =
        await Future.wait([app.command(prepare()), app.command(prepare())]);
    expect(replies.every((r) => r['state'] == 'accepted'), true);
    expect(confirmations, hasLength(1));
    await expectLater(
        app.command({...prepare(), 'browser': 'edge'}), throwsFormatException);
    await expectLater(
        app.command({
          ...prepare(),
          'req': {'url': 'https://other.test/'}
        }),
        throwsFormatException);
    expect(core.creates, 0);
  });
  test(
      'unsupported core has no ordinary-create fallback; pending listings are passive',
      () async {
    final unsupported = BrowserHandoff(
        request: (_, __) async => {'version': 0},
        preferences: (_, __) => null,
        confirm: (_) async {});
    await expectLater(unsupported.command({'action': 'handoffCapabilities'}),
        throwsFormatException);
    expect(
        (await app.command({
          'action': 'handoffQuery',
          'clientId': 'chrome-one',
          'browser': 'chrome',
          'pending': true
        }))['items'],
        isEmpty);
    expect(core.calls.every((c) => c['action'] != 'prepare'), true);
  });
  test(
      'BrowserIntegration routes all five actions and does not persist handoff credentials',
      () async {
    final bridge = BrowserIntegration(
        read: () => null,
        write: (_) async {
          fail('handoff must not write Hive');
        },
        handoffCommand: app.command);
    expect(await bridge.command({'action': 'handoffCapabilities'}),
        {'version': 1});
    expect((await bridge.command(prepare()))['state'], 'accepted');
    expect((await bridge.command(query()))['state'], 'accepted');
    await bridge.command(
        {...query(), 'action': 'handoffAbort', 'reason': 'user_cancelled'});
    expect(ended, ['capture:user_cancelled']);
  });
  for (final byButton in [false, true])
    testWidgets(
        'handoff terminal input preserves the next queued capture; button=$byButton',
        (tester) async {
      if (!Platform.isMacOS) return;
      final ui = await mountHandoffUi(tester, core);
      final first = prepare();
      first['req']['extra']['header']['Cookie'] = 'fake-A';
      final next = prepare('next');
      next['req']['extra']['header']['Cookie'] = 'fake-B';
      await ui.flow.command(first);
      await tester.pumpAndSettle();
      await ui.flow.command(next);
      await tester.pumpAndSettle();
      if (byButton) {
        await tester.sendKeyEvent(LogicalKeyboardKey.escape);
      } else {
        await ui.flow.command({
          ...query(),
          'action': 'handoffAbort',
          'reason': 'browser_completed'
        });
      }
      await tester.pumpAndSettle();
      final input = find.byKey(const ValueKey('panel-download-input'));
      expect(tester.widget<TextFormField>(input).controller!.text,
          'https://fixture.test/next');
      expect(
          tester
              .widget<EditableText>(find.descendant(
                  of: input, matching: find.byType(EditableText)))
              .focusNode
              .hasFocus,
          true);
      expect(tester.testTextInput.hasAnyClients, true);
      expect((await ui.flow.command(query()))['state'], 'aborted');
      await tester.tap(find.byTooltip('开始下载'));
      await tester.pumpAndSettle();
      expect(core.records.keys, ['next']);
      expect(core.starts, 0);
      expect(core.creates, 1);
      expect(ui.createdReceipts, 0);
      final submitted = core.calls.singleWhere((c) => c['action'] == 'prepare');
      expect(submitted['req']['extra']['header']['Cookie'], 'fake-B');
      expect(submitted['operationId'], 'next');
    });
  testWidgets('Escape detaches text input before clearing and hiding capture',
      (tester) async {
    if (!Platform.isMacOS) return;
    final ui = await mountHandoffUi(tester, core);
    await ui.flow.command(prepare());
    await tester.pumpAndSettle();
    final input = find.byKey(const ValueKey('panel-download-input'));
    await tester.showKeyboard(input);
    final text = tester.widget<TextFormField>(input).controller!;
    expect(tester.testTextInput.hasAnyClients, true);
    final connectedWhenCleared = <bool>[];
    void changed() {
      if (text.text.isEmpty) {
        connectedWhenCleared.add(tester.testTextInput.hasAnyClients);
      }
    }

    text.addListener(changed);
    addTearDown(() => text.removeListener(changed));
    final connectedWhenHidden = <bool>[];
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'hide') {
        connectedWhenHidden.add(tester.testTextInput.hasAnyClients);
      }
      return null;
    });
    tester.testTextInput.log.clear();
    // A show request queued before Escape must not reattach after hide.
    MacPanel.focusRequests.value++;
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pumpAndSettle();
    expect(connectedWhenCleared, isNotEmpty);
    expect(connectedWhenCleared, everyElement(false));
    expect(connectedWhenHidden, [false]);
    expect(tester.testTextInput.hasAnyClients, false);
    expect(tester.testTextInput.log.map((call) => call.method),
        contains('TextInput.clearClient'));
    var connected = true;
    for (final call in tester.testTextInput.log) {
      if (call.method == 'TextInput.setClient') connected = true;
      if (call.method == 'TextInput.clearClient') connected = false;
      if (call.method == 'TextInput.setEditingState') {
        expect(connected, true,
            reason: 'Native TextInputModel must exist for every editing state');
      }
    }
    expect(text.text, isEmpty);
    expect((await ui.flow.command(query()))['reason'], 'user_cancelled');
    expect(core.creates, 0);
    expect(core.starts, 0);

    await MacPanel.showPanel();
    await tester.pumpAndSettle();
    await tester.enterText(input, 'https://fixture.test/manual');
    expect(tester.testTextInput.hasAnyClients, true);
    expect(text.text, 'https://fixture.test/manual');
  });
  testWidgets(
      'native confirmation prepares without resolve/create or created receipt; row checks only query',
      (tester) async {
    if (!Platform.isMacOS) return;
    Database.instance.box = _Box();
    final uiApp = Get.put<AppController>(_UiApp()) as _UiApp;
    uiApp.downloaderConfig.value.downloadDir = '/tmp/kownload-handoff-test';
    uiApp.downloaderConfig.value.extra.defaultDirectDownload = false;
    uiApp.flow = BrowserHandoff(
        request: core.call,
        preferences: (_, __) => {'enabled': true, 'confirm': true},
        begin: uiApp.beginBrowserCapture,
        ended: (id, reason) =>
            uiApp.browserCaptureCompleted(id, reason: reason),
        confirm: (task) => MacPanel.showPanel(task: task));
    Get.put(CreateController());
    final list = Get.put<MacTaskListController>(_List(core));
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = null;
    MacPanel.taskResult.value = null;
    MacPanel.notice.value = null;
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 760));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.incoming.value = null;
      MacPanel.taskResult.value = null;
      MacPanel.notice.value = null;
      MacPanel.cancelAutoHide();
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
      tester.binding.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
    });
    var ordinaryCreates = 0, resolves = 0;
    await tester.pumpWidget(
        GetMaterialApp.router(theme: macTheme(Brightness.light), getPages: [
      GetPage(
          name: '/',
          page: () => CreateView(taskCreator: (_) async {
                ordinaryCreates++;
                return 'wrong';
              }, taskResolver: (_) async {
                resolves++;
                throw StateError('must not resolve');
              }))
    ]));
    await tester.pumpAndSettle();
    await uiApp.flow.command(prepare());
    await tester.pumpAndSettle();
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pumpAndSettle();
    expect(ordinaryCreates, 0);
    expect(resolves, 0);
    expect(uiApp.createdReceipts, 0);
    expect(core.starts, 0);
    expect(core.creates, 1);
    expect(find.text('待浏览器交接'), findsOneWidget);
    expect(find.text('已准备，等待浏览器交接'), findsOneWidget);
    expect(find.text('未完成 1'), findsOneWidget);
    expect(find.byType(LinearProgressIndicator), findsNothing);
    expect(find.textContaining('1 个任务运行中'), findsNothing);
    final before = core.calls.length;
    await tester.tap(find.byTooltip('检查交接'));
    await tester.pumpAndSettle();
    expect(core.calls.skip(before).every((c) => c['action'] == 'query'), true);
    expect(list.tasks.single.handoff!.blocked, true);
    expect(core.starts, 0);
  });
}

class _Box extends Fake implements Box {
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      key == MacOnboarding.completedKey ? true : defaultValue;
}

class _UiApp extends AppController {
  late BrowserHandoff flow;
  int createdReceipts = 0;
  @override
  BrowserHandoff get browserHandoff => flow;
  @override
  void onReady() {}
  @override
  // This fixture deliberately owns no AppController lifecycle resources.
  // ignore: must_call_super
  void onClose() {}
  @override
  void completeBrowserConfirmation(String id,
      {List<String> taskIds = const [], bool cancelled = false}) {
    createdReceipts++;
  }
}

class _List extends MacTaskListController {
  final Core core;
  _List(this.core);
  @override
  // This fixture reads tasks from Core without starting the normal poller.
  // ignore: must_call_super
  void onInit() {}
  @override
  // This fixture has no task polling timer to stop.
  // ignore: must_call_super
  void onClose() {}
  @override
  Future<void> getTasksState() async {
    tasks.assignAll(core.records.keys.map(core.task));
  }
}

Future<_UiApp> mountHandoffUi(WidgetTester tester, Core core) async {
  Database.instance.box = _Box();
  final app = Get.put<AppController>(_UiApp()) as _UiApp;
  app.downloaderConfig.value.downloadDir = '/tmp/kownload-handoff-test';
  app.flow = BrowserHandoff(
      request: core.call,
      preferences: (_, __) => {'enabled': true, 'confirm': true},
      begin: app.beginBrowserCapture,
      ended: (id, reason) => app.browserCaptureCompleted(id, reason: reason),
      confirm: (task) => MacPanel.showPanel(task: task));
  Get.put(CreateController());
  Get.put<MacTaskListController>(_List(core));
  MacPanel.presentation.value = 'panel';
  MacPanel.incoming.value = null;
  MacPanel.taskResult.value = null;
  MacPanel.notice.value = null;
  tester.binding.defaultBinaryMessenger
      .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
  await tester.binding.setSurfaceSize(const Size(640, 760));
  addTearDown(() async {
    await tester.pumpWidget(const SizedBox.shrink());
    MacPanel.incoming.value = null;
    MacPanel.taskResult.value = null;
    MacPanel.notice.value = null;
    MacPanel.cancelAutoHide();
    Get.reset();
    Get.routerDelegate = null;
    Get.routeInformationParser = null;
    await tester.binding.setSurfaceSize(null);
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });
  await tester.pumpWidget(GetMaterialApp.router(
      theme: macTheme(Brightness.light),
      getPages: [GetPage(name: '/', page: () => const CreateView())]));
  await tester.pumpAndSettle();
  return app;
}
