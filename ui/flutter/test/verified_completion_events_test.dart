import 'dart:convert';
import 'dart:io';
import 'dart:isolate';
import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/verified_completion_event.dart';
import 'package:gopeed/api/verified_completion_events.dart';
import 'package:gopeed/database/task_batch_journal.dart';
import 'support/scoped_task_batch_fixture.dart';

Map<String, Object> eventJson(String domain,
        {String task = 'task', String cid = 'completion'}) =>
    {'version': 1, 'datasetId': domain, 'taskId': task, 'completionId': cid};
String frame(Map<String, Object> event) =>
    'event: accepted-done\ndata: ${jsonEncode(event)}\n\n';
Future<void> unixEvents(List<Object> args) async {
  final send = args[0] as SendPort;
  try {
    api.init('unix', args[1] as String, 'fixture-event-unix-token',
        taskBatchRoute: JournalRouteRef(
            product: 'kownload', identityProtocol: 1, routeScope: 'b' * 64));
    final c = await openVerifiedCompletionEvents(
        api.activeApiSnapshot!, args[2] as String, CancelToken());
    final events = await c.events.toList();
    await c.close();
    Isolate.exit(send, {
      'count': events.length,
      'task': events.single.taskId,
      'network': api.activeApiSnapshot!.network
    });
  } catch (error) {
    Isolate.exit(send, {'error': error.runtimeType.toString()});
  }
}

void main() {
  final f = ScopedBatchFixture();
  final held = <HttpResponse>[];
  setUpAll(f.start);
  setUp(f.reset);
  tearDown(() async {
    for (final response in held) {
      try {
        await response.close();
      } catch (_) {}
    }
    held.clear();
  });
  tearDownAll(f.close);
  Future<void> respond(HttpRequest q, List<int> bytes,
      {bool keep = false, bool split = false}) async {
    q.response.headers.set('content-type', 'text/event-stream');
    q.response.headers.set('cache-control', 'no-store');
    if (split) {
      for (var start = 0; start < bytes.length; start += 17) {
        q.response
            .add(bytes.sublist(start, (start + 17).clamp(0, bytes.length)));
        await q.response.flush();
      }
    } else {
      q.response.add(bytes);
      await q.response.flush();
    }
    if (keep) {
      held.add(q.response);
    } else {
      await q.response.close();
    }
  }

  Future<VerifiedCompletionConnection> open() => openVerifiedCompletionEvents(
      api.activeApiSnapshot!, f.dataset, CancelToken());
  test(
      'actual shared auth wire accepts fragmented UTF8 and never sends recovery inputs',
      () async {
    f.override = (q) => respond(
        q, utf8.encode(frame(eventJson(f.dataset, task: '你' * 42))),
        split: true);
    final c = await open();
    final events = await c.events.toList();
    await c.close();
    expect(events.single.taskId, '你' * 42);
    final r = f.requests.single;
    expect(r['method'], 'GET');
    expect(r['body'], isNull);
    expect((r['uri'] as Uri).query, isEmpty);
    expect(r['token'], 'fixture-scoped-token');
  });
  test(
      'exact 4096 byte frame is accepted; only closed frames at EOF are emitted',
      () async {
    final base = frame(eventJson(f.dataset));
    final padded =
        '${base.substring(0, base.length - 2)}${' ' * (4096 - utf8.encode(base).length)}\n\n';
    expect(utf8.encode(padded).length, 4096);
    f.override = (q) =>
        respond(q, utf8.encode(padded + base.substring(0, base.length - 1)));
    final c = await open();
    expect(await c.events.toList(), hasLength(1));
    await c.close();
  });
  for (final bad in [
    'bom',
    'crlf',
    'comment',
    'id',
    'retry',
    'hello',
    'multiData',
    'oversized',
    'utf8',
    'version',
    'extra',
    'scope',
    'task129',
    'cid129',
    'unpaired'
  ]) {
    test('reject $bad frame as terminal protocol error without a permission',
        () async {
      var raw = eventJson(f.dataset);
      if (bad == 'version') raw['version'] = 2;
      if (bad == 'extra') raw['fresh'] = true;
      if (bad == 'scope') raw['datasetId'] = 'b' * 32;
      if (bad == 'task129') raw['taskId'] = 'x' * 129;
      if (bad == 'cid129') raw['completionId'] = '你' * 43;
      if (bad == 'unpaired') raw['completionId'] = '\ud800';
      var text = frame(raw);
      if (bad == 'bom') text = '\ufeff$text';
      if (bad == 'crlf') text = text.replaceAll('\n', '\r\n');
      if (const ['comment', 'id', 'retry', 'hello'].contains(bad)) {
        final prefix = {
          'comment': ': noop\n',
          'id': 'id: 1\n',
          'retry': 'retry: 1\n',
          'hello': 'event: hello\n'
        }[bad]!;
        text = '$prefix$text';
      }
      if (bad == 'multiData') {
        text = text.replaceFirst('\n\n', '\ndata: {}\n\n');
      }
      if (bad == 'oversized') {
        text = 'event: accepted-done\ndata: ${' ' * 4097}';
      }
      final bytes = bad == 'utf8'
          ? <int>[...utf8.encode('event: accepted-done\ndata: '), 255, 10, 10]
          : utf8.encode(text);
      f.override = (q) => respond(q, bytes);
      final c = await open();
      await expectLater(
          c.events.toList(),
          throwsA(isA<CompletionEventsException>()
              .having((e) => e.terminal, 'terminal', true)));
      await c.close();
    });
  }
  for (final status in [
    301,
    302,
    303,
    307,
    308,
    400,
    401,
    403,
    404,
    409,
    501
  ]) {
    test('$status stops without redirect or legacy fallback', () async {
      f.override = (q) async {
        q.response.statusCode = status;
        q.response.headers.set('location', '/not-followed');
        await q.response.close();
      };
      await expectLater(
          open(),
          throwsA(isA<CompletionEventsException>()
              .having((e) => e.terminal, 'terminal', true)));
      expect(f.requests.length, 1);
      expect((f.requests.single['uri'] as Uri).path,
          contains('/completion-events'));
    });
  }
  for (final msg in [
    'completion_events_capacity',
    'completion_events_unavailable',
    'other'
  ]) {
    test('503 $msg is classified with bounded JSON before any retry permission',
        () async {
      f.override = (q) => f.respond(q, null, status: 503, code: 1000, msg: msg);
      await expectLater(
          open(),
          throwsA(isA<CompletionEventsException>()
              .having((e) => e.terminal, 'terminal', msg == 'other')));
      expect(f.requests.length, 1);
    });
  }
  for (final header in ['type', 'cache']) {
    test('wrong $header header is terminal', () async {
      f.override = (q) async {
        q.response.headers.set('content-type',
            header == 'type' ? 'application/json' : 'text/event-stream');
        q.response.headers
            .set('cache-control', header == 'cache' ? 'public' : 'no-store');
        q.response.write('{}');
        await q.response.close();
      };
      await expectLater(
          open(),
          throwsA(isA<CompletionEventsException>()
              .having((e) => e.terminal, 'terminal', true)));
    });
  }
  test('invalid dataset and stale actual API snapshot send zero HTTP',
      () async {
    final snapshot = api.activeApiSnapshot!;
    await expectLater(
        api.verifiedCompletionEventsRequest(snapshot, 'A' * 32, CancelToken()),
        throwsArgumentError);
    f.init('c');
    await expectLater(
        api.verifiedCompletionEventsRequest(snapshot, f.dataset, CancelToken()),
        throwsStateError);
    expect(f.requests, isEmpty);
  });
  test('cancel closes an idle connection, and EOF never invents a final event',
      () async {
    f.override = (q) =>
        respond(q, utf8.encode('event: accepted-done\ndata: '), keep: true);
    final c = await open();
    final seen = <VerifiedCompletionEvent>[];
    final sub = c.events.listen(seen.add, onError: (Object _) {});
    await c.close();
    await sub.cancel();
    expect(seen, isEmpty);
    expect(f.posts, 0);
  });
  test('real Unix adapter and original synthetic auth are reused', () async {
    final dir = Directory('/tmp').createTempSync('k16-event-unix-');
    final path = '${dir.path}/api.sock';
    final server = await HttpServer.bind(
        InternetAddress(path, type: InternetAddressType.unix), 0);
    final requests = <HttpRequest>[];
    final subscription = server.listen((q) async {
      requests.add(q);
      await respond(q, utf8.encode(frame(eventJson(f.dataset))));
    });
    final port = ReceivePort();
    try {
      await Isolate.spawn(unixEvents, [port.sendPort, path, f.dataset]);
      final result = await port.first.timeout(const Duration(seconds: 10));
      expect(result, {'count': 1, 'task': 'task', 'network': 'unix'});
      expect(requests.single.headers.value('X-Api-Token'),
          'fixture-event-unix-token');
      expect(requests.single.headers.value('Last-Event-ID'), isNull);
      expect(requests.single.uri.query, isEmpty);
    } finally {
      port.close();
      await subscription.cancel();
      await server.close(force: true);
      dir.deleteSync(recursive: true);
    }
  });
}
