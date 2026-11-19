import 'dart:async';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' show GetMaterialApp;
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resource.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/task_stats.dart';
import 'package:gopeed/app/modules/task/controllers/task_recovery_feedback.dart';
import 'package:gopeed/app/views/mac_task_failure.dart';
import 'package:gopeed/i18n/message.dart' as i18n;

import 'manual_task_batch_submission_test.dart' show spin;
import 'netdisk_test_support.dart' show mockExistingTask;
import 'support/http_recovery_fixture.dart';

void main() {
  final core = HttpRecoveryFixture();
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await core.start();
  });
  setUp(core.reset);
  tearDownAll(core.close);

  Task sample({Status status = Status.error, int bytes = 32, int time = 0}) =>
      Task(
          id: core.taskId,
          name: 'sample.bin',
          meta:
              Meta(
                  req: Request(
                      url: 'https://fixture.invalid/sample?sig=expired'),
                  opts: Options(path: '/fixture/destination', selectFiles: [0]))
                ..res = Resource(size: 64, range: true, files: []),
          status: status,
          uploading: false,
          progress: Progress(
              used: 0,
              speed: 0,
              downloaded: bytes,
              uploadSpeed: 0,
              uploaded: 0),
          createdAt: DateTime.utc(2026),
          updatedAt: DateTime.utc(2026, 1, 1, 0, 0, time))
        ..protocol = Protocol.http;

  Widget app(Widget body, {Locale locale = const Locale('zh', 'CN')}) =>
      GetMaterialApp(
          locale: locale,
          fallbackLocale: const Locale('en', 'US'),
          translations: i18n.messages,
          home: Scaffold(body: body));

  testWidgets('uses a fixed category and reloads only for a fresh failure',
      (tester) async {
    var reads = 0;
    final old = Completer<TaskStats>(), next = Completer<TaskStats>();
    Future<TaskStats> read(String id) =>
        ++reads == 1 ? old.future : next.future;
    Widget view(int time) => app(MacTaskFailure(
        task: sample(time: time),
        style: const TextStyle(fontSize: 11),
        afterLinkUpdate: time != 0,
        stage: time == 0 ? null : RecoveryStage.failed,
        read: read));
    await tester.pumpWidget(view(0));
    expect(find.textContaining('下载失败'), findsOneWidget);
    old.complete(const TaskStats(
        lastError:
            'HTTP 403 https://fixture.invalid/file?signature=synthetic Cookie: account=B Bearer fake-token'));
    await tester.pumpAndSettle();
    expect(find.textContaining('来源拒绝访问'), findsOneWidget);
    await tester.pumpWidget(view(0));
    await tester.pumpAndSettle();
    expect(reads, 1);
    expect(find.textContaining('signature='), findsNothing);
    expect(find.textContaining('account=B'), findsNothing);
    expect(find.textContaining('fake-token'), findsNothing);
    await tester.pumpWidget(view(1));
    await tester.pumpAndSettle();
    expect(find.textContaining('来源拒绝访问'), findsNothing);
    next.complete(const TaskStats(lastError: 'http request fail, code:403'));
    await tester.pumpAndSettle();
    expect(find.textContaining('下载链接仍不可用'), findsOneWidget);
  });

  testWidgets('missing diagnostics use English fallback without a cause',
      (tester) async {
    await tester.pumpWidget(app(
        MacTaskFailure(
            task: sample(),
            style: const TextStyle(),
            read: (_) async => throw StateError('details unavailable')),
        locale: const Locale('de', 'DE')));
    await tester.pumpAndSettle();
    expect(find.textContaining('Download failed'), findsOneWidget);
    expect(find.textContaining('details unavailable'), findsNothing);
    expect(tester.takeException(), isNull);
  });

  test('only an eligible task can replace its URL or resync drive login', () {
    expect(canReplaceTaskLink(sample(), null), isTrue);
    expect(canReplaceTaskLink(sample()..sourceFrozen = true, null), isFalse);
    expect(
        canReplaceTaskLink(sample()..meta.res!.range = false, null), isFalse);
    expect(canReplaceTaskLink(sample()..meta.res!.size = 0, null), isFalse);
    expect(canReplaceTaskLink(sample(status: Status.running), null), isFalse);
    expect(
        canReplaceTaskLink(sample()..meta.req.extra = {'method': 'POST'}, null),
        isFalse);
    for (final protocol in [
      Protocol.bt,
      Protocol.media,
      Protocol.netdisk,
      null
    ]) {
      expect(canReplaceTaskLink(sample()..protocol = protocol, null), isFalse);
    }
    final drive = mockExistingTask()..protocol = Protocol.netdisk;
    expect(canSyncTaskLogin(drive), isTrue);
    expect(
        canSyncTaskLogin(drive..meta.req.url = 'https://other.invalid'), isTrue,
        reason:
            'the original structured provider page still identifies the drive');
    expect(
        canSyncTaskLogin(
          drive..meta.req.extra = {},
        ),
        isFalse);
  });

  test('running, stale data, and different IDs do not confirm new bytes', () {
    final feedback = TaskRecoveryFeedback();
    final original = sample();
    final attempt = feedback.begin(original)!;
    feedback.report(attempt, RecoveryStage.submitted);
    feedback.observe([original], completeList: true);
    final running = sample(status: Status.running, time: 1);
    feedback.observe([running], completeList: true);
    expect(feedback.stage(running), RecoveryStage.submitted);
    final other = sample(status: Status.running, time: 1, bytes: 48)
      ..id = 'different';
    feedback.observe([other], completeList: false);
    expect(feedback.stage(original), RecoveryStage.submitted);
    final advanced = sample(status: Status.running, time: 1, bytes: 48);
    feedback.observe([advanced], completeList: true);
    expect(feedback.stage(advanced), RecoveryStage.resumed);
  });

  for (final mode in [
    'accepted',
    'later403',
    'rejected',
    'unknown',
    'cooldown'
  ]) {
    testWidgets('same task link update: $mode', (tester) async {
      final feedback = TaskRecoveryFeedback();
      final original = sample();
      final before = original.toJson();
      final root = '/api/v1/tasks/${original.id}';
      final patchGate = Completer<void>();
      final received = <String>[];
      core.override = (request) async {
        final path = request.uri.path;
        if (path.endsWith('/stats')) {
          return core.respond(request, {'lastError': 'download link expired'});
        }
        if (mode == 'cooldown' && path.endsWith('/recovery/v1')) {
          return core.respond(request, {
            'scopeVersion': 1,
            'datasetId': core.dataset,
            'recovery': core.view()
              ..['reason'] = 'cooldown'
              ..['retryAllowed'] = false
          });
        }
        if (request.method == 'PATCH') {
          received.add(path);
          await patchGate.future;
          if (mode == 'rejected') {
            return core.respond(request, null,
                code: 1000,
                msg:
                    'cannot verify HTTP resource identity; partial file preserved');
          }
          if (mode == 'unknown') {
            final socket =
                await request.response.detachSocket(writeHeaders: false);
            socket.destroy();
            return;
          }
          return core.respond(request, null);
        }
        if (request.method == 'PUT') {
          received.add(path);
          return core.respond(request, null);
        }
        await core.handle(request);
      };
      try {
        await tester.pumpWidget(app(Builder(builder: (context) {
          return TextButton(
            onPressed: () => showTaskRecovery(context, original, feedback,
                refresh: () async {}, currentTask: () => original),
            child: const Text('open'),
          );
        })));
        await tester.tap(find.text('open'));
        await spin(
            tester,
            () => core.requests.any(
                (request) => (request['uri'] as Uri).path == '$root/stats'));
        await tester.pumpAndSettle();
        await tester.enterText(find.byType(TextFormField),
            'https://fixture.invalid/sample?sig=fresh');
        final submit = find.widgetWithText(FilledButton, '更新链接并继续原任务');
        await tester.tap(submit);
        if (mode != 'cooldown') {
          await spin(tester, () => received.isNotEmpty);
          expect(feedback.stage(original), RecoveryStage.checking);
        }
        patchGate.complete();
        final expected = switch (mode) {
          'rejected' => RecoveryStage.unsafeLink,
          'unknown' => RecoveryStage.unknown,
          'cooldown' => RecoveryStage.cooldown,
          _ => RecoveryStage.submitted,
        };
        await spin(tester, () => feedback.stage(original) == expected);
        await tester.pumpAndSettle();
        if (mode == 'cooldown') {
          expect(received, isEmpty);
        } else {
          final patch =
              core.requests.singleWhere((r) => r['method'] == 'PATCH');
          expect((patch['uri'] as Uri).path, root);
          final body = patch['body'] as Map;
          expect(body['opts'], isNull);
          expect(body['req']['extra'], isNull);
        }
        if (mode == 'accepted' || mode == 'later403') {
          expect(received, [root, '$root/continue']);
          if (mode == 'later403') {
            final failedAgain = sample(time: 2);
            feedback.observe([failedAgain], completeList: true);
            expect(feedback.stage(failedAgain), RecoveryStage.failed);
            expect(feedback.isLinkUpdate(failedAgain), isTrue);
            expect(
                taskFailure(
                    const TaskStats(lastError: 'http request fail, code:403'),
                    afterRecovery: true),
                TaskFailure.invalidLink);
          }
        } else if (mode == 'unknown') {
          expect(tester.widget<FilledButton>(submit).onPressed, isNull);
          expect(feedback.begin(original, linkUpdate: true), isNull);
          expect(received, [root]);
        } else if (mode == 'rejected') {
          expect(received, [root]);
          expect(find.text('新链接未通过原文件校验，已保留部分文件'), findsOneWidget);
        }
        expect(core.requests.where((r) => r['method'] == 'POST'), isEmpty);
        expect(original.toJson(), before);
      } finally {
        if (!patchGate.isCompleted) patchGate.complete();
        await tester.pumpWidget(const SizedBox());
        await spin(tester, () => core.inFlight == 0);
      }
    });
  }
}
