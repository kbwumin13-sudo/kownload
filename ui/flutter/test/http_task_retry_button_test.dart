import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/app/modules/task/views/http_task_retry_button.dart';
import 'support/http_recovery_fixture.dart';
import 'manual_task_batch_submission_test.dart' show spin;

void main() {
  final f = HttpRecoveryFixture();
  final messages = <String>[];
  var legacy = 0, refresh = 0;
  late HttpRetrySession session;
  final cleanup = <void Function()>[];
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
  Future<void> mount(WidgetTester t, {Task? task}) async =>
      t.pumpWidget(MaterialApp(
          home: Scaffold(
              body: HttpTaskRetryButton(
                  task: task ?? sample(),
                  session: session,
                  legacyContinue: () async {
                    legacy++;
                  },
                  refresh: () async {
                    refresh++;
                  },
                  notice: messages.add))));
  Future<void> click(WidgetTester t, String tooltip) async {
    await t.tap(find.byTooltip(tooltip));
    await t.pump();
  }

  void widgetTest(String name, Future<void> Function(WidgetTester) body) {
    testWidgets(name, (t) async {
      try {
        await body(t);
      } finally {
        for (final release in cleanup) {
          release();
        }
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
    legacy = 0;
    refresh = 0;
    messages.clear();
    cleanup.clear();
    session = HttpRetrySession();
  });
  tearDownAll(f.close);
  widgetTest('explicit error click sends once then only queries original tuple',
      (t) async {
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    expect(f.posts, 1);
    expect(legacy, 0);
    final body =
        Map.from(f.requests.singleWhere((r) => r['method'] == 'POST')['body']);
    expect(body['expectedEpisode'], 'ep-a');
    expect(messages.single, contains('已受理'));
    await click(t, '再次重试');
    await spin(t, () => refresh == 2);
    expect(f.posts, 1);
    final q = (f.requests.last['uri'] as Uri).queryParameters;
    expect(q['actionId'], body['actionId']);
    expect(q['expectedEpisode'], 'ep-a');
  });
  widgetTest('remount after lost ACK queries original action instead of POST',
      (t) async {
    f.override = (request) async {
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    await t.pumpWidget(const SizedBox());
    f.override = null;
    f.replyMode = 'not_recorded';
    await mount(t);
    await t.tap(find.byType(IconButton));
    await t.pump();
    await spin(t, () => refresh == 2);
    expect(f.posts, 1);
    expect(
        (f.requests.last['uri'] as Uri).queryParameters['actionId'], original);
  });
  widgetTest(
      'accepted then running then new error episode allows explicit next action',
      (t) async {
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    expect(messages.single, contains('已受理'));
    expect(f.posts, 1);
    final first =
        Map.from(f.requests.singleWhere((r) => r['method'] == 'POST')['body']);
    f.episode = 'ep-b';
    f.status = 'running';
    await t.pumpWidget(const SizedBox());
    await mount(t, task: sample()..status = Status.running);
    expect(f.posts, 1);
    f.status = 'error';
    await t.pumpWidget(const SizedBox());
    await mount(t);
    await t.tap(find.byType(IconButton));
    await t.pump();
    await spin(t, () => refresh == 2);
    expect(f.posts, 2);
    final second =
        Map.from(f.requests.lastWhere((r) => r['method'] == 'POST')['body']);
    expect(first['expectedEpisode'], 'ep-a');
    expect(second['expectedEpisode'], 'ep-b');
    expect(second['expectedSourceRevision'], 1);
    expect(second['actionId'], isNot(first['actionId']));
    expect(legacy, 0);
  });
  widgetTest('two mounted entry points share a pending first POST action',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    f.override = (request) async {
      if (request.method == 'POST') await gate.future;
      await f.handle(request);
    };
    Widget button(String id) => HttpTaskRetryButton(
        key: ValueKey(id),
        task: sample(),
        legacyContinue: () async {
          legacy++;
        },
        refresh: () async {
          refresh++;
        },
        notice: messages.add);
    await t.pumpWidget(MaterialApp(
        home: Scaffold(
            body: Row(children: [button('full-list'), button('compact')]))));
    Future<void> press(String id) async {
      await t.tap(find.descendant(
          of: find.byKey(ValueKey(id)), matching: find.byType(IconButton)));
      await t.pump();
    }

    await press('full-list');
    await spin(t, () => f.posts == 1);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    await press('compact');
    await spin(t, () => refresh == 1 || f.posts == 2);
    expect(gate.isCompleted, false);
    expect(f.posts, 1);
    expect(
        (f.requests.last['uri'] as Uri).queryParameters['actionId'], original);
    expect(legacy, 0);
  });
  widgetTest('remount while original POST stays pending only queries its tuple',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    f.replyMode = 'not_recorded';
    f.override = (request) async {
      if (request.method == 'POST') await gate.future;
      await f.handle(request);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => f.posts == 1);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    await t.pumpWidget(const SizedBox());
    await mount(t);
    await click(t, '查询原重试');
    await spin(t, () => refresh == 1);
    expect(gate.isCompleted, false);
    expect(f.posts, 1);
    expect(
        (f.requests.last['uri'] as Uri).queryParameters['actionId'], original);
  });
  widgetTest('concurrent successful preflights consume only one shared POST',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    var preflights = 0;
    f.override = (request) async {
      if (request.uri.path.endsWith('/recovery/v1') &&
          request.uri.queryParameters.isEmpty) {
        preflights++;
        await gate.future;
      }
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    Widget button(String id) => HttpTaskRetryButton(
        key: ValueKey(id),
        session: session,
        task: sample(),
        legacyContinue: () async {
          legacy++;
        },
        refresh: () async {
          refresh++;
        },
        notice: messages.add);
    await t.pumpWidget(MaterialApp(
        home: Scaffold(body: Row(children: [button('a'), button('b')]))));
    for (final id in ['a', 'b']) {
      await t.tap(find.descendant(
          of: find.byKey(ValueKey(id)), matching: find.byType(IconButton)));
      await t.pump();
    }
    await spin(t, () => preflights == 2);
    gate.complete();
    await spin(t, () => refresh == 2);
    expect(f.posts, 1);
    expect(legacy, 0);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    final originalQueries = f.requests.where(
        (r) => (r['uri'] as Uri).queryParameters.containsKey('actionId'));
    expect(originalQueries.length, 1);
    expect((originalQueries.single['uri'] as Uri).queryParameters['actionId'],
        original);
  });
  for (final change in ['dataset', 'epoch']) {
    widgetTest('shared reference survives remount and isolates changed $change',
        (t) async {
      f.override = (request) async {
        if (request.method == 'POST') f.replyMode = 'lostAck';
        await f.handle(request);
      };
      await mount(t);
      await click(t, '重试');
      await spin(t, () => refresh == 1);
      await t.pumpWidget(const SizedBox());
      f.override = null;
      f.replyMode = 'managed';
      if (change == 'dataset') {
        f.dataset = 'c' * 32;
      } else {
        f.init();
      }
      final queries = f.scopedGets;
      await mount(t);
      await click(t, '查询原重试');
      await spin(t, () => messages.length == 2);
      expect(f.posts, 1);
      expect(f.scopedGets, queries);
      expect(legacy, 0);
    });
  }
  widgetTest('different task retains its own explicit action independently',
      (t) async {
    f.override = (request) async {
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    final originalId = f.taskId;
    f.taskId = 'other-task';
    try {
      await mount(t);
      await click(t, '重试');
      await spin(t, () => refresh == 2);
      expect(f.posts, 2);
      final actions = f.requests
          .where((r) => r['method'] == 'POST')
          .map((r) => (r['body'] as Map)['actionId'])
          .toSet();
      expect(actions.length, 2);
    } finally {
      f.taskId = originalId;
    }
  });
  widgetTest(
      'unconfirmed old action cannot retry even when current episode changed',
      (t) async {
    f.override = (request) async {
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    final original = (f.requests
        .singleWhere((r) => r['method'] == 'POST')['body'] as Map)['actionId'];
    await t.pumpWidget(const SizedBox());
    f.override = null;
    f.replyMode = 'stale';
    f.episode = 'ep-b';
    await mount(t);
    await click(t, '查询原重试');
    await spin(t, () => refresh == 2);
    expect(f.posts, 1);
    expect(find.byTooltip('查询原重试'), findsOneWidget);
    expect(
        (f.requests.last['uri'] as Uri).queryParameters['actionId'], original);
  });
  widgetTest('acknowledged retry cannot reset while Core says running',
      (t) async {
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    f.episode = 'ep-b';
    f.status = 'running';
    await click(t, '再次重试');
    await spin(t, () => messages.length == 2);
    expect(f.posts, 1);
    expect(legacy, 0);
    expect(messages.last, contains('未处于可重试'));
  });
  widgetTest('two next-episode clicks only reserve one new action', (t) async {
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    f.episode = 'ep-b';
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    var reads = 0;
    f.override = (request) async {
      if (request.uri.path.endsWith('/recovery/v1') &&
          request.uri.queryParameters.isEmpty) {
        reads++;
        await gate.future;
      }
      if (request.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(request);
    };
    Widget button(String id) => HttpTaskRetryButton(
        key: ValueKey(id),
        session: session,
        task: sample(),
        legacyContinue: () async {
          legacy++;
        },
        refresh: () async {
          refresh++;
        },
        notice: messages.add);
    await t.pumpWidget(MaterialApp(
        home: Scaffold(
            body: Row(children: [button('next-a'), button('next-b')]))));
    for (final id in ['next-a', 'next-b']) {
      await t.tap(find.descendant(
          of: find.byKey(ValueKey(id)), matching: find.byType(IconButton)));
      await t.pump();
    }
    await spin(t, () => reads == 2);
    gate.complete();
    await spin(t, () => refresh == 3);
    expect(f.posts, 2);
    final posts = f.requests.where((r) => r['method'] == 'POST').toList();
    final next = posts.last['body'] as Map;
    expect(next['expectedEpisode'], 'ep-b');
    expect(next['actionId'], isNot((posts.first['body'] as Map)['actionId']));
    expect(legacy, 0);
  });
  for (final mode in ['lostAck', '409', '503', 'redirect', 'html']) {
    widgetTest('POST $mode retains action and subsequent click is only GET',
        (t) async {
      // Capability/current state is read successfully before this POST fault.
      f.override = (q) async {
        if (q.method == 'POST') {
          f.replyMode = mode;
        }
        await f.handle(q);
      };
      await mount(t);
      await click(t, '重试');
      await spin(t, () => refresh == 1);
      expect(f.posts, 1);
      expect(legacy, 0);
      f.override = null;
      f.replyMode = 'not_recorded';
      await click(t, '查询原重试');
      await spin(t, () => refresh == 2);
      expect(f.posts, 1);
      expect(legacy, 0);
      expect(
          (f.requests.last['uri'] as Uri).queryParameters['actionId'],
          (f.requests.singleWhere((r) => r['method'] == 'POST')['body']
              as Map)['actionId']);
    });
  }
  for (final mode in [
    'held',
    'html',
    'badVersion',
    'wrongDataset',
    'redirect'
  ]) {
    widgetTest('preflight $mode never POSTs or falls back', (t) async {
      f.replyMode = mode;
      await mount(t);
      await click(t, '重试');
      await spin(t, () => messages.isNotEmpty);
      expect(f.posts, 0);
      expect(legacy, 0);
    });
  }
  widgetTest('only explicit same-domain legacy uses original Continue',
      (t) async {
    f.replyMode = 'legacy';
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    expect(legacy, 1);
    expect(f.posts, 0);
  });
  widgetTest(
      'POST gate stays pending after stop and query still makes zero new POST',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    f.override = (q) async {
      if (q.method == 'POST') {
        await gate.future;
      }
      await f.handle(q);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => f.posts == 1);
    await click(t, '停止等待');
    expect(gate.isCompleted, false);
    expect(find.byTooltip('查询原重试'), findsOneWidget);
    await click(t, '查询原重试');
    await spin(t, () => refresh == 1);
    expect(f.posts, 1);
    expect(gate.isCompleted, false);
    gate.complete();
    await spin(t, () => f.inFlight == 0);
    expect(refresh, 1);
    expect(legacy, 0);
  });
  widgetTest('stopped initial read cannot POST when its gate later opens',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    f.override = (q) async {
      await gate.future;
      await f.handle(q);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => f.requests.isNotEmpty);
    await click(t, '停止等待');
    expect(find.byTooltip('重试'), findsOneWidget);
    gate.complete();
    await spin(t, () => f.inFlight == 0);
    expect(f.posts, 0);
    expect(legacy, 0);
    expect(refresh, 0);
  });
  widgetTest('late stopped POST cannot release a newer query confirmation',
      (t) async {
    final post = Completer<void>(), query = Completer<void>();
    cleanup.add(() {
      if (!post.isCompleted) post.complete();
      if (!query.isCompleted) query.complete();
    });
    var queryWaiting = false;
    f.override = (request) async {
      if (request.method == 'POST') await post.future;
      if (request.uri.queryParameters.containsKey('actionId')) {
        queryWaiting = true;
        await query.future;
      }
      await f.handle(request);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => f.posts == 1);
    await click(t, '停止等待');
    await click(t, '查询原重试');
    await spin(t, () => queryWaiting);
    post.complete();
    await spin(t, () => f.inFlight == 1);
    expect(find.byTooltip('停止等待'), findsOneWidget);
    expect(refresh, 0);
    query.complete();
    await spin(t, () => refresh == 1);
    expect(f.posts, 1);
    expect(legacy, 0);
  });
  widgetTest('dataset changes between preflight and send grant zero POST',
      (t) async {
    f.override = (request) async {
      await f.handle(request);
      if (request.uri.path.endsWith('/recovery/v1')) f.dataset = 'c' * 32;
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => messages.isNotEmpty);
    expect(f.posts, 0);
    expect(legacy, 0);
  });
  widgetTest('unknown action cannot cross dataset or API epoch', (t) async {
    f.override = (q) async {
      if (q.method == 'POST') f.replyMode = 'lostAck';
      await f.handle(q);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => refresh == 1);
    f.override = null;
    f.replyMode = 'managed';
    f.dataset = 'c' * 32;
    final gets = f.scopedGets;
    await click(t, '查询原重试');
    await spin(t, () => messages.length == 2);
    expect(f.scopedGets, gets);
    expect(f.posts, 1);
    f.init('d');
    await click(t, '查询原重试');
    await t.pump();
    expect(f.scopedGets, gets);
    expect(f.posts, 1);
    expect(legacy, 0);
  });
  widgetTest('dispose while initial query pending has no late action',
      (t) async {
    final gate = Completer<void>();
    cleanup.add(() {
      if (!gate.isCompleted) gate.complete();
    });
    f.override = (q) async {
      await gate.future;
      await f.handle(q);
    };
    await mount(t);
    await click(t, '重试');
    await spin(t, () => f.requests.isNotEmpty);
    await t.pumpWidget(const SizedBox());
    gate.complete();
    await spin(t, () => f.inFlight == 0);
    expect(f.posts, 0);
    expect(legacy, 0);
    expect(messages, isEmpty);
  });
  widgetTest('non-HTTP and non-error tasks cannot consume reset', (t) async {
    for (final task in [
      sample()..protocol = Protocol.bt,
      sample()..status = Status.pause
    ]) {
      await mount(t, task: task);
      await click(t, '重试');
      await t.pump();
    }
    expect(f.posts, 0);
    expect(f.requests, isEmpty);
    expect(legacy, 0);
  });
}
