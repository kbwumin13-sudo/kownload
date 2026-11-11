import 'dart:async';
import 'dart:io';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import 'package:get/get.dart' hide Progress;
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/verified_completion_event.dart';
import 'package:gopeed/api/verified_completion_events.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/services/notification_service.dart';
import 'support/scoped_task_batch_fixture.dart';
import 'manual_task_batch_submission_test.dart' show spin;

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Live {
  final events =
      StreamController<VerifiedCompletionEvent>.broadcast(sync: true);
  int closes = 0;
  late final connection = VerifiedCompletionConnection(
      events: events.stream,
      close: () async {
        closes++;
        unawaited(events.close());
      });
  void add(String domain, {String task = 'task', String cid = 'completion'}) {
    events.add(VerifiedCompletionEvent.fromJson({
      'version': 1,
      'datasetId': domain,
      'taskId': task,
      'completionId': cid
    }));
  }
}

Task sample(
    {Status status = Status.running,
    bool enrolled = true,
    String phase = 'committed',
    String cid = 'completion',
    String? domain,
    String id = 'task',
    String? errorCategory,
    bool bad = false}) {
  final task = Task(
      id: id,
      name: 'fixture.bin',
      meta: Meta(
          req: Request(url: 'https://fixture.invalid/file'), opts: Options()),
      status: status,
      uploading: false,
      progress: Progress(
          used: 0, speed: 0, downloaded: 32768, uploadSpeed: 0, uploaded: 0),
      createdAt: DateTime.utc(2026),
      updatedAt: DateTime.utc(2026))
    ..protocol = Protocol.http;
  final json = task.toJson();
  if (enrolled) {
    json['completion'] = {'version': 1};
    json['completionView'] = {
      'version': bad ? 2 : 1,
      'scopeVersion': 1,
      'datasetId': domain ?? 'a' * 32,
      'taskId': id,
      'completionId': cid,
      'available': true,
      'phase': phase,
      'recordedMatch':
          const ['prepared', 'waiting_verify', 'verifying'].contains(phase)
              ? null
              : true,
      'errorCategory': errorCategory
    };
  }
  final result = Task.fromJson(json);
  final snapshot = api.activeApiSnapshot!;
  result.completionView
      ?.bindToDataset(domain ?? 'a' * 32, isCurrent: () => snapshot.isCurrent);
  return result;
}

void main() {
  final f = ScopedBatchFixture();
  late AppController app;
  late NotificationService service;
  late _Live live;
  final ownedLives = <_Live>[];
  final cleanup = <void Function()>[];
  _Live makeLive() {
    final value = _Live();
    ownedLives.add(value);
    return value;
  }

  var tasks = <Task>[];
  var loads = 0, opens = 0;
  final shows = <Map>[];
  const channel = MethodChannel('dexterous.com/flutter/local_notifications');
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await f.start();
  });
  tearDownAll(f.close);
  setUp(() {
    f.reset();
    MacOSFlutterLocalNotificationsPlugin.registerWith();
    app = Get.put<AppController>(_App());
    tasks = [];
    loads = 0;
    opens = 0;
    shows.clear();
    ownedLives.clear();
    cleanup.clear();
    live = makeLive();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
      if (call.method == 'show') shows.add(Map.from(call.arguments));
      return call.method == 'initialize' ? true : null;
    });
  });
  tearDown(() {
    Get.reset();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, null);
  });
  void notificationTest(String name, Future<void> Function(WidgetTester) body) {
    testWidgets(name, (tester) async {
      try {
        await body(tester);
      } finally {
        for (final close in cleanup) {
          close();
        }
        service.onClose();
        await tester.pump();
        await tester.runAsync(() async {
          for (final item in ownedLives) {
            await item.connection.close();
          }
        });
        await tester.pump();
      }
    }, variant: TargetPlatformVariant.only(TargetPlatform.macOS));
  }

  void start({CompletionEventsOpener? open}) {
    service = NotificationService(
        loadTasks: (_) async {
          loads++;
          return tasks;
        },
        openEvents: open ??
            (s, d, c) async {
              opens++;
              return live.connection;
            });
    service.onInit();
  }

  Future<void> tick(WidgetTester t, {int? opened}) async {
    final before = loads;
    await t.pump(const Duration(seconds: 2));
    await spin(t, () => loads > before && (opened == null || opens >= opened));
  }

  notificationTest(
      'fresh bound event not historical done emits once through the plugin',
      (t) async {
    tasks = [sample()];
    start();
    await tick(t, opened: 1);
    tasks = [sample(status: Status.done)];
    await tick(t);
    expect(shows, isEmpty);
    live.add(f.dataset);
    live.add(f.dataset);
    await t.pump();
    expect(shows.length, 1);
    expect(shows.single['title'], 'notificationTaskDone'.tr);
    await tick(t);
    expect(shows.length, 1);
    expect(opens, 1);
  });
  for (final mode in [
    'historical',
    'bad',
    'committedError',
    'uncommitted',
    'differentCID',
    'wrongDataset',
    'missingTask',
    'differentTask'
  ]) {
    notificationTest('$mode event cannot become a success notification',
        (t) async {
      tasks = [sample()];
      start();
      await tick(t, opened: 1);
      tasks = mode == 'missingTask'
          ? []
          : [
              sample(
                  status: Status.done,
                  bad: mode == 'bad',
                  errorCategory: mode == 'committedError'
                      ? 'completion_state_invalid'
                      : null,
                  phase: mode == 'uncommitted' ? 'verified' : 'committed')
            ];
      await tick(t);
      if (mode != 'historical') {
        live.add(mode == 'wrongDataset' ? 'b' * 32 : f.dataset,
            cid: mode == 'differentCID' ? 'other' : 'completion',
            task: mode == 'differentTask' ? 'other' : 'task');
      }
      await t.pump();
      expect(shows, isEmpty);
    });
  }
  notificationTest(
      'legacy running to done and error remain the original notification path',
      (t) async {
    tasks = [sample(enrolled: false)];
    start();
    await tick(t, opened: 1);
    tasks = [sample(status: Status.done, enrolled: false)];
    await tick(t);
    expect(shows.length, 1);
    expect(shows.single['title'], 'notificationTaskDone'.tr);
    tasks = [sample(status: Status.error, enrolled: false)];
    await tick(t);
    expect(shows.length, 2);
    expect(shows.last['title'], 'notificationTaskError'.tr);
  });
  notificationTest('cold first historical done never emits', (t) async {
    tasks = [sample(status: Status.done)];
    start();
    await tick(t, opened: 1);
    await tick(t);
    expect(shows, isEmpty);
  });
  notificationTest('disabled stops the current stream and rejects late events',
      (t) async {
    tasks = [sample(status: Status.done)];
    start();
    await tick(t, opened: 1);
    app.downloaderConfig.value.extra.desktopNotification = false;
    app.downloaderConfig.refresh();
    await t.pump();
    await spin(t, () => live.closes == 1);
    await tickDisabled(t);
    expect(opens, 1);
    expect(shows, isEmpty);
  });
  notificationTest(
      'dispose closes stream and no further poll or event can notify',
      (t) async {
    tasks = [sample(status: Status.done)];
    start();
    await tick(t, opened: 1);
    service.onClose();
    await t.pump();
    await spin(t, () => live.closes == 1);
    final before = loads;
    await t.pump(const Duration(seconds: 6));
    expect(loads, before);
    expect(shows, isEmpty);
  });
  notificationTest(
      'terminal unsupported/auth/protocol error stops this API epoch',
      (t) async {
    tasks = [sample()];
    start(open: (s, d, c) async {
      opens++;
      throw const CompletionEventsException('unsupported', terminal: true);
    });
    await tick(t, opened: 1);
    await tick(t);
    await tick(t);
    expect(opens, 1);
    f.init('c');
    await tick(t, opened: 2);
    expect(opens, 2);
    expect(shows, isEmpty);
  });
  notificationTest(
      'open fence survives cancellation until actual late opener settles',
      (t) async {
    final gate = Completer<VerifiedCompletionConnection>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete(live.connection);
    });
    final second = makeLive();
    tasks = [sample(status: Status.done)];
    start(open: (s, d, c) async {
      opens++;
      if (opens == 1) return await gate.future;
      return second.connection;
    });
    await tick(t, opened: 1);
    f.init('c');
    await tick(t);
    expect(opens, 1);
    gate.complete(live.connection);
    await t.pump();
    await spin(t, () => live.closes == 1);
    tasks = [sample(status: Status.done)];
    await tick(t, opened: 2);
    second.add(f.dataset);
    await t.pump();
    expect(shows.length, 1);
  });
  notificationTest(
      'dataset change closes old live scope and only future new-domain events can notify',
      (t) async {
    final second = makeLive();
    tasks = [sample(status: Status.done)];
    start(open: (s, d, c) async {
      opens++;
      return opens == 1 ? live.connection : second.connection;
    });
    await tick(t, opened: 1);
    f.dataset = 'c' * 32;
    tasks = [sample(status: Status.done, domain: f.dataset)];
    await tick(t);
    await spin(t, () => live.closes == 1);
    await tick(t, opened: 2);
    second.add(f.dataset);
    await t.pump();
    expect(shows.length, 1);
    expect(f.posts, 0);
  });
  notificationTest('actual close future fences a later API subscription',
      (t) async {
    final closeGate = Completer<void>();
    cleanup.add(() {
      if (!closeGate.isCompleted) closeGate.complete();
    });
    var closeStarted = false;
    final second = makeLive();
    final gated = VerifiedCompletionConnection(
        events: live.events.stream,
        close: () async {
          closeStarted = true;
          await closeGate.future;
          await live.connection.close();
        });
    tasks = [sample(status: Status.done)];
    start(open: (s, d, c) async {
      opens++;
      return opens == 1 ? gated : second.connection;
    });
    await tick(t, opened: 1);
    f.init('c');
    await t.pump();
    await spin(t, () => closeStarted);
    await tick(t);
    expect(opens, 1);
    closeGate.complete();
    await spin(t, () => live.closes == 1);
    tasks = [sample(status: Status.done)];
    await tick(t, opened: 2);
    second.add(f.dataset);
    await t.pump();
    expect(shows.length, 1);
  });
  notificationTest(
      'RAM bound drops excess permissions without evicting already consumed keys',
      (t) async {
    tasks = List.generate(
        2049,
        (i) =>
            sample(status: Status.done, id: 'task-$i', cid: 'completion-$i'));
    start();
    await tick(t, opened: 1);
    for (var i = 0; i < 2049; i++) {
      live.add(f.dataset, task: 'task-$i', cid: 'completion-$i');
    }
    await t.pump();
    expect(shows.length, 2048);
    live.add(f.dataset, task: 'task-0', cid: 'completion-0');
    await t.pump();
    expect(shows.length, 2048);
  });
  notificationTest(
      'bad identity protocol stops this API epoch without affecting legacy notifications',
      (t) async {
    f.override = (q) async {
      q.response.write('<html>old server</html>');
      await q.response.close();
    };
    tasks = [sample(enrolled: false)];
    start();
    await tick(t);
    await spin(t, () => f.requests.isNotEmpty);
    await tick(t);
    await tick(t);
    expect(opens, 0);
    expect(f.requests.length, 1);
    tasks = [sample(status: Status.done, enrolled: false)];
    await tick(t);
    expect(shows.length, 1);
  });
  notificationTest(
      'EOF reconnects only on a later existing tick and never queries history',
      (t) async {
    final second = makeLive();
    tasks = [sample(status: Status.done)];
    start(open: (s, d, c) async {
      opens++;
      return opens == 1 ? live.connection : second.connection;
    });
    await tick(t, opened: 1);
    unawaited(live.events.close());
    await t.pump();
    expect(opens, 1);
    await tick(t, opened: 2);
    expect(shows, isEmpty);
    expect(f.posts, 0);
  });
}

Future<void> tickDisabled(WidgetTester t) async {
  await t.pump(const Duration(seconds: 4));
}
