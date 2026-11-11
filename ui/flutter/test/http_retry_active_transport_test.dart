import 'dart:io';
import 'package:dio/dio.dart' show CancelToken;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/modules/task/views/http_task_retry_button.dart';
import 'support/http_recovery_fixture.dart';
import 'manual_task_batch_submission_test.dart' show spin;

/// The non-Mac main/init path supplies no Mac host or journal route.
class _TransportFixture extends HttpRecoveryFixture {
  @override
  void init([String char = 'b']) =>
      api.init('tcp', address, 'fixture-scoped-token');

  @override
  Future<void> handle(HttpRequest request) async {
    if (request.method == 'PUT' &&
        request.uri.path == '/api/v1/tasks/$taskId/continue') {
      return respond(request, null);
    }
    return super.handle(request);
  }
}

void main() {
  final f = _TransportFixture();
  final messages = <String>[];
  late HttpRetrySession session;
  var refresh = 0;
  Task sample() => Task(
      id: f.taskId,
      name: 'fixture.bin',
      meta: Meta(
          req: Request(url: 'https://fixture.invalid/file'), opts: Options()),
      status: Status.error,
      uploading: false,
      progress: Progress(
          used: 0, speed: 0, downloaded: 0, uploadSpeed: 0, uploaded: 0),
      createdAt: DateTime.utc(2026),
      updatedAt: DateTime.utc(2026))
    ..protocol = Protocol.http;
  Future<void> mount(WidgetTester t) => t.pumpWidget(MaterialApp(
      home: Scaffold(
          body: HttpTaskRetryButton(
              task: sample(),
              session: session,
              legacyContinue: () => api.continueTask(f.taskId),
              refresh: () async {
                refresh++;
              },
              notice: messages.add))));
  Future<void> click(WidgetTester t) async {
    final beforeRefresh = refresh, beforeMessages = messages.length;
    await t.tap(find.byType(IconButton));
    await t.pump();
    await spin(
        t, () => refresh > beforeRefresh || messages.length > beforeMessages);
  }

  int getContinues() => f.requests.where((r) => r['method'] == 'PUT').length;
  void widgetTest(String name, Future<void> Function(WidgetTester) body) {
    testWidgets(name, (t) async {
      try {
        expect(api.activeApiSnapshot!.taskBatchRoute, isNull);
        await body(t);
      } finally {
        await t.pumpWidget(const SizedBox());
        await t.pump();
        await spin(t, () => f.inFlight == 0);
        session.dispose();
      }
    });
  }

  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    HttpOverrides.global = null;
    await f.start();
  });
  setUp(() {
    f.reset();
    session = HttpRetrySession();
    refresh = 0;
    messages.clear();
  });
  tearDownAll(f.close);

  widgetTest('no Mac route: explicit same-domain legacy GET permits Continue',
      (t) async {
    f.replyMode = 'legacy';
    await mount(t);
    await click(t);
    expect(f.scopedGets, 1);
    expect(getContinues(), 1);
    expect(refresh, 1);
    expect(f.posts, 0);
    expect(f.requests.every((r) => r['token'] == 'fixture-scoped-token'), true);
  });
  widgetTest(
      'no Mac route: managed error consumes one action in actual dataset',
      (t) async {
    await mount(t);
    await click(t);
    expect(refresh, 1);
    expect(f.posts, 1);
    final post = f.requests.singleWhere((r) => r['method'] == 'POST');
    expect((post['uri'] as Uri).path, contains('/datasets/${f.dataset}/'));
    expect((post['body'] as Map)['expectedEpisode'], 'ep-a');
    expect(getContinues(), 0);
  });
  for (final mode in ['html', 'badVersion', 'held', 'wrongDataset']) {
    widgetTest('no Mac route: $mode is queried but grants no fallback',
        (t) async {
      f.replyMode = mode;
      await mount(t);
      await click(t);
      expect(f.scopedGets, 1);
      expect(f.posts, 0);
      expect(getContinues(), 0);
      expect(refresh, 0);
    });
  }
  widgetTest(
      'no Mac route: remounted unknown action remains original tuple GET',
      (t) async {
    f.override = (request) async {
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    await mount(t);
    await click(t);
    expect(f.posts, 1);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    await t.pumpWidget(const SizedBox());
    f.override = null;
    f.replyMode = 'not_recorded';
    messages.clear();
    await mount(t);
    await t.tap(find.byType(IconButton));
    await t.pump();
    await spin(t, () => refresh == 2);
    expect(f.posts, 1);
    expect(
        (f.requests.last['uri'] as Uri).queryParameters['actionId'], original);
    expect(getContinues(), 0);
  });
  widgetTest(
      'no Mac route: reinit or dataset change never renews an unknown action',
      (t) async {
    f.override = (request) async {
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    await mount(t);
    await click(t);
    expect(f.posts, 1);
    f.override = null;
    f.replyMode = 'managed';
    f.dataset = 'c' * 32;
    messages.clear();
    final gets = f.scopedGets;
    await click(t);
    expect(f.scopedGets, gets);
    expect(f.posts, 1);
    final beforeReinit = api.activeApiSnapshot!;
    f.init();
    expect(beforeReinit.isCurrent, false);
    messages.clear();
    await t.pumpWidget(const SizedBox());
    await mount(t);
    await click(t);
    expect(f.scopedGets, gets);
    expect(f.posts, 1);
    expect(getContinues(), 0);
    expect(messages.single, contains('连接或数据域已变化'));
  });
  test('no Mac route does not enable batch, SHA or completion event transport',
      () async {
    final s = api.activeApiSnapshot!;
    await expectLater(
        api.scopedTaskBatchRequest(s, 'GET', '/api/v1/dataset/identity'),
        throwsStateError);
    await expectLater(
        api.scopedTaskBatchRequest(s, 'GET',
            '/api/v1/datasets/${f.dataset}/verified-http/v1/capability'),
        throwsStateError);
    await expectLater(
        api.verifiedCompletionEventsRequest(s, f.dataset, CancelToken()),
        throwsStateError);
    expect(f.requests, isEmpty);
  });
}
