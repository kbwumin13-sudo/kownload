import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/rpc/browser_submission.dart';
import 'package:gopeed/app/rpc/rpc.dart';
import 'package:gopeed/app/services/browser_integration.dart';

Response<dynamic> response(dynamic data, [int status = 200]) => Response(
    requestOptions: RequestOptions(path: '/api/v1/tasks'),
    statusCode: status,
    data: data);
Map<String, dynamic> created(String id) => {
      'code': 0,
      'data': 'task-one',
      'operationId': id,
      'submission': {
        'state': 'created',
        'taskIds': ['task-one']
      }
    };

class _RealHttp extends HttpOverrides {}

void main() {
  const body =
      '{"req":{"url":"https://fixture.test/file?signature=synthetic"}}';
  test(
      'App confirmation reaches created through the existing operation, without resubmitting',
      () async {
    var opened = 0;
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async {
          sends++;
          throw StateError('unexpected second pipeline');
        },
        showCreate: (_) async {},
        log: (_) {},
        showConfirmation: (_, id, capture) async {
          expect(id, 'app-confirm');
          opened++;
        });
    await rpc.create(body, operationId: 'app-confirm', confirm: true);
    expect(rpc.confirmationStatus('app-confirm'),
        {'state': 'accepted', 'taskIds': []});
    rpc.completeConfirmation('app-confirm', taskIds: ['created-one']);
    final reply =
        await rpc.create(body, operationId: 'app-confirm', confirm: true);
    expect(reply.body['submission'], {
      'state': 'created',
      'taskIds': ['created-one']
    });
    expect(reply.body['data'], 'created-one');
    expect(rpc.confirmationStatus('app-confirm')['state'], 'created');
    rpc.completeConfirmation('app-confirm', cancelled: true);
    expect(rpc.confirmationStatus('app-confirm')['state'], 'created');
    expect(opened, 1);
    expect(sends, 0);
  });

  test(
      'new capture resets unrelated UI binding once, before showing confirmation',
      () async {
    var resets = 0;
    final rpc = BrowserSubmissionRpc(
        beginBrowserCapture: () => resets++,
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {
          expect(resets, 1);
        },
        log: (_) {});
    await rpc.create(body,
        operationId: 'fresh-capture', confirm: true, browserCapture: true);
    await rpc.create(body,
        operationId: 'fresh-capture', confirm: true, browserCapture: true);
    expect(resets, 1,
        reason: 'a status retry must not clear another active draft');
    await rpc.create(body, operationId: 'manual', confirm: true);
    expect(resets, 1);
  });

  test('passive complete closes only the matching uncommitted capture once',
      () async {
    final ended = <String>[];
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async {
          sends++;
          throw StateError('unexpected');
        },
        showCreate: (_) async {},
        log: (_) {},
        onBrowserCaptureCompleted: ended.add);
    await rpc.create(body,
        operationId: 'observe',
        confirm: true,
        browserCapture: true,
        captureClientId: 'chrome-A',
        captureBrowser: 'chrome');
    for (final pair in [('chrome-B', 'chrome'), ('chrome-A', 'edge')]) {
      expect(
          rpc.observeBrowserState(
              'observe', pair.$1, pair.$2, {'state': 'complete'})['state'],
          'accepted');
    }
    expect(
        rpc.observeBrowserState(
            'other-op', 'chrome-A', 'chrome', {'state': 'complete'})['state'],
        'unknown');
    expect(
        rpc.observeBrowserState('observe', 'chrome-A', 'chrome',
            {'state': 'in_progress', 'received': 10, 'total': 10})['state'],
        'accepted');
    expect(ended, isEmpty);
    expect(
        rpc.observeBrowserState(
            'observe', 'chrome-A', 'chrome', {'state': 'complete'})['state'],
        'browser_completed');
    rpc.observeBrowserState(
        'observe', 'chrome-A', 'chrome', {'state': 'complete'});
    expect(ended, ['observe']);
    expect(await rpc.requestCommit('observe'), false);
    expect(
        (await rpc.create(body, operationId: 'observe', confirm: true))
            .body['submission']['state'],
        'browser_completed');
    expect(sends, 0);
    await rpc.create(body,
        operationId: 'manual',
        confirm: true,
        captureClientId: 'chrome-A',
        captureBrowser: 'chrome');
    expect(
        rpc.observeBrowserState(
            'manual', 'chrome-A', 'chrome', {'state': 'complete'})['state'],
        'accepted');
    expect(ended, ['observe']);
  });

  test(
      'passive completion releases a waiting gate but never changes a committed or created receipt',
      () async {
    final ended = <String>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {},
        onBrowserCaptureCompleted: ended.add);
    Future<void> capture(String id) => rpc
        .create(body,
            operationId: id,
            confirm: true,
            browserCapture: true,
            captureClientId: 'one',
            captureBrowser: 'edge')
        .then((_) {});
    await capture('waiting');
    final gate = rpc.requestCommit('waiting');
    final oldToken =
        rpc.confirmationStatus('waiting')['confirmToken'] as String;
    rpc.observeBrowserState('waiting', 'one', 'edge', {'state': 'complete'});
    expect(await gate, false);
    expect(
        rpc.decideConfirmation('waiting', oldToken, 'commit',
            {'state': 'in_progress', 'paused': true})['state'],
        'browser_completed');
    await capture('started');
    final startedGate = rpc.requestCommit('started');
    final token = rpc.confirmationStatus('started')['confirmToken'] as String;
    rpc.decideConfirmation(
        'started', token, 'commit', {'state': 'in_progress', 'paused': true});
    expect(await startedGate, true);
    expect(
        rpc.observeBrowserState(
            'started', 'one', 'edge', {'state': 'complete'})['commitStarted'],
        true);
    rpc.completeConfirmation('started', taskIds: ['real-created']);
    expect(
        rpc.observeBrowserState(
            'started', 'one', 'edge', {'state': 'complete'}),
        {
          'state': 'created',
          'taskIds': ['real-created']
        });
    await capture('cancelled');
    rpc.completeConfirmation('cancelled', cancelled: true);
    expect(
        rpc.observeBrowserState(
            'cancelled', 'one', 'edge', {'state': 'complete'})['state'],
        'cancelled');
    expect(
        rpc.observeBrowserState(
            'unknown', 'one', 'edge', {'state': 'complete'})['state'],
        'unknown');
    expect(ended, ['waiting']);
  });

  test(
      'passive complete cannot close an automatic REST create in flight or change its created receipt',
      () async {
    final upstream = Completer<Response<dynamic>>();
    var sends = 0;
    final ended = <String>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) {
          sends++;
          return upstream.future;
        },
        showCreate: (_) async {},
        log: (_) {},
        onBrowserCaptureCompleted: ended.add);
    final future = rpc.create(body,
        operationId: 'automatic',
        confirm: false,
        browserCapture: true,
        captureClientId: 'one',
        captureBrowser: 'chrome');
    expect(sends, 1);
    expect(rpc.confirmationStatus('automatic')['state'], 'unknown',
        reason:
            'automatic creation is never an unsubmitted accepted confirmation');
    expect(
        rpc.observeBrowserState(
            'automatic', 'one', 'chrome', {'state': 'complete'})['state'],
        'unknown');
    expect(ended, isEmpty);
    upstream.complete(response(created('automatic')));
    final reply = await future;
    expect(reply.body['submission'], {
      'state': 'created',
      'taskIds': ['task-one']
    });
    rpc.observeBrowserState(
        'automatic', 'one', 'chrome', {'state': 'complete'});
    final retry = await rpc.create(body,
        operationId: 'automatic',
        confirm: false,
        browserCapture: true,
        captureClientId: 'one',
        captureBrowser: 'chrome');
    expect(retry.body, reply.body);
    expect(sends, 1);
    expect(ended, isEmpty);
  });

  test('only explicit cancellation marks an accepted confirmation cancelled',
      () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    expect(rpc.confirmationStatus('unknown')['state'], 'unknown');
    expect((await rpc.create(body, confirm: true, browserCapture: true)).status,
        400);
    await rpc.create(body, operationId: 'cancel-one', confirm: true);
    expect(rpc.confirmationStatus('cancel-one')['state'], 'accepted');
    rpc.completeConfirmation('cancel-one', cancelled: true);
    expect(rpc.confirmationStatus('cancel-one'),
        {'state': 'cancelled', 'taskIds': []});
    await rpc.create(body, operationId: 'cancel-one', confirm: true);
    expect(rpc.confirmationStatus('cancel-one')['state'], 'cancelled');
  });

  test(
      'browser capture waits for a fresh token decision and preserves real browser state',
      () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    await rpc.create(body,
        operationId: 'gated', confirm: true, browserCapture: true);
    var released = false;
    final gate = rpc.requestCommit('gated').then((value) {
      released = true;
      return value;
    });
    final waiting = rpc.confirmationStatus('gated');
    expect(waiting['commitRequested'], true);
    final token = waiting['confirmToken'] as String;
    rpc.decideConfirmation('gated', 'wrong-token', 'commit',
        {'state': 'in_progress', 'paused': true});
    await Future<void>.delayed(Duration.zero);
    expect(released, false);
    expect(
        () => rpc.decideConfirmation('gated', token, 'browser_completed', {
              'state': 'in_progress',
              'paused': true,
              'received': 10,
              'total': 10
            }),
        throwsFormatException);
    rpc.decideConfirmation('gated', token, 'commit',
        {'state': 'in_progress', 'paused': true, 'received': 10, 'total': 10});
    expect(await gate, true,
        reason: 'fully buffered is not the complete terminal state');
    expect(rpc.confirmationStatus('gated')['state'], 'accepted');
    expect(rpc.confirmationStatus('gated')['commitStarted'], true);
    rpc.completeConfirmation('gated', cancelled: true);
    expect(rpc.confirmationStatus('gated')['state'], 'accepted');
    rpc.completeConfirmation('gated', taskIds: ['final-task']);
    expect(
        rpc.decideConfirmation('gated', token, 'commit',
            {'state': 'in_progress', 'paused': true})['state'],
        'created');
  });

  test('a browser-complete gate never releases a second download', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    await rpc.create(body,
        operationId: 'complete-browser', confirm: true, browserCapture: true);
    final gate = rpc.requestCommit('complete-browser');
    final token =
        rpc.confirmationStatus('complete-browser')['confirmToken'] as String;
    rpc.decideConfirmation('complete-browser', token, 'browser_completed',
        {'state': 'complete', 'paused': true});
    expect(await gate, false);
    expect(rpc.confirmationStatus('complete-browser'),
        {'state': 'browser_completed', 'taskIds': []});
    expect(await rpc.requestCommit('complete-browser'), false);
  });

  test('browser results show once per silent receipt and once per batch',
      () async {
    final results = <List<String>>[];
    final errors = <String?>[];
    var batch = false;
    final rpc = BrowserSubmissionRpc(
      send: (_, id, __) async => response(batch
          ? {
              'code': 0,
              'data': ['a', 'b'],
              'submission': {
                'state': 'created',
                'taskIds': ['a', 'b']
              },
            }
          : created(id!)),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        results.add(ids);
        errors.add(error);
      },
    );
    await rpc.create(body, operationId: 'silent-panel', confirm: false);
    await rpc.create(body, operationId: 'silent-panel', confirm: false);
    await rpc.create(body, operationId: 'confirm-panel', confirm: true);
    expect(results, [
      ['task-one']
    ]);
    batch = true;
    await rpc.forward({'method': 'POST', 'path': '/api/v1/tasks/batch'});
    expect(results, [
      ['task-one'],
      ['a', 'b']
    ]);
    expect(errors, [null, null]);
    await rpc.forward({'method': 'GET', 'path': '/api/v1/tasks'});
    expect(results, hasLength(2));
  });
  test('local failure cause is shown but not exposed in native receipts',
      () async {
    final errors = <String?>[];
    final rpc = BrowserSubmissionRpc(
      send: (_, __, ___) async => response({
        'code': 1000,
        'msg': 'unsupported protocol https://fixture.test/secret?token=hidden'
      }),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        expect(ids, isEmpty);
        errors.add(error);
      },
    );
    final reply = await rpc.create(body, confirm: false);
    expect(errors.single, contains('unsupported protocol'));
    expect(errors.single, isNot(contains('hidden')));
    expect(jsonEncode(reply.body), isNot(contains('unsupported')));
    final disconnected = BrowserSubmissionRpc(
      send: (_, __, ___) async => throw StateError('socket unavailable'),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        errors.add(error);
      },
    );
    await disconnected.create(body, confirm: false);
    expect(errors.last, contains('无法连接下载服务'));
  });
  test('panel failure cannot change a created receipt', () async {
    final rpc = BrowserSubmissionRpc(
      send: (_, id, __) async => response(created(id!)),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (_, __) async => throw StateError('window unavailable'),
    );
    expect(
        (await rpc.create(body, operationId: 'panel-failure', confirm: false))
            .body['submission']['state'],
        'created');
  });
  test('accepted coalesces confirmation and logs domains only', () async {
    var opens = 0;
    final events = <Map<String, dynamic>>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {
          opens++;
        },
        log: events.add);
    final a = await rpc.create(body, operationId: 'confirm-one', confirm: true);
    final b = await rpc.create(body, operationId: 'confirm-one', confirm: true);
    expect(opens, 1);
    expect(a.status, 202);
    expect(b.body, a.body);
    expect(a.body['submission'], {'state': 'accepted', 'taskIds': []});
    expect(events.first['domain'], 'fixture.test');
    expect(jsonEncode(events), isNot(contains('signature')));
  });
  test('a new operation reopens confirmation after an accepted one', () async {
    var opens = 0;
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {
          opens++;
        },
        log: (_) {});
    await rpc.create(body, operationId: 'click-one', confirm: true);
    await rpc.create(body, operationId: 'click-one', confirm: true);
    expect(opens, 1, reason: 'a resend of one click must not reopen the panel');
    final again =
        await rpc.create(body, operationId: 'click-two', confirm: true);
    expect(opens, 2);
    expect(again.status, 202);
    expect(again.body['submission'], {'state': 'accepted', 'taskIds': []});
  });
  test('a new operation runs again after a definitive failure', () async {
    final calls = <String?>[];
    var fixed = false;
    final rpc = BrowserSubmissionRpc(
        send: (_, id, __) async {
          calls.add(id);
          return fixed
              ? response(created(id!))
              : response({'code': 1000, 'msg': 'not allowed', 'data': null});
        },
        showCreate: (_) async => fail('unexpected'),
        log: (_) {});
    final failed =
        await rpc.create(body, operationId: 'click-one', confirm: false);
    expect(failed.body['error'], 'business_error');
    fixed = true;
    final done =
        await rpc.create(body, operationId: 'click-two', confirm: false);
    expect(done.body['submission']['state'], 'created');
    expect(calls, ['click-one', 'click-two']);
  });
  test('silent created retains data and uses ten seconds', () async {
    final rpc = BrowserSubmissionRpc(
        send: (value, id, budget) async {
          expect(budget, const Duration(seconds: 10));
          expect(value['path'], '/api/v1/tasks');
          return response(created(id!));
        },
        showCreate: (_) async => fail('unexpected'),
        log: (_) {});
    final result =
        await rpc.create(body, operationId: 'silent-one', confirm: false);
    expect(result.body['data'], 'task-one');
    expect(result.body['submission']['state'], 'created');
  });
  for (final mode in [
    'http',
    'business',
    'empty',
    'code',
    'submission',
    'wrong ID',
    'throw'
  ]) {
    test('$mode failure is explicit and redacted', () async {
      final rpc = BrowserSubmissionRpc(
          send: (_, id, __) async {
            switch (mode) {
              case 'http':
                return response({
                  'code': 0,
                  'msg': 'fixture-secret',
                  'data': 'fixture-secret'
                }, 500);
              case 'business':
                return response({
                  'code': 1000,
                  'msg': 'fixture-secret',
                  'data': 'fixture-secret'
                });
              case 'empty':
                return response('');
              case 'code':
                return response({'data': 'fixture-secret'});
              case 'submission':
                return response({'code': 0, 'data': 'task-one'});
              case 'wrong ID':
                return response(created('other'));
              default:
                throw StateError('fixture-secret');
            }
          },
          showCreate: (_) async {},
          log: (_) {});
      final r =
          await rpc.create(body, operationId: 'failed-one', confirm: false);
      expect(r.status, mode == 'http' || mode == 'throw' ? 500 : 200);
      expect(r.body['data'], isNull);
      expect(r.body['submission'], isNull);
      expect(jsonEncode(r.body), isNot(contains('fixture-secret')));
      expect(
          r.body['error'],
          mode == 'http'
              ? 'http_error'
              : mode == 'business'
                  ? 'business_error'
                  : mode == 'throw'
                      ? 'request_failed'
                      : 'invalid_response');
    });
  }
  test('forward keeps data and the sixty second budget', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, budget) async {
          expect(budget, const Duration(seconds: 60));
          return response({
            'code': 0,
            'data': {
              'nested': [1, 2]
            }
          });
        },
        showCreate: (_) async {},
        log: (_) {});
    final r = await rpc.forward({'method': 'GET', 'path': '/api/v1/config'},
        operationId: 'get-one');
    expect(r.body['data'], {
      'nested': [1, 2]
    });
    expect(r.body['operationId'], 'get-one');
    expect(
        (await rpc.forward(
                {'method': 'POST', 'path': 'https://foreign.test/api/v1/tasks'},
                operationId: 'get-one'))
            .status,
        400);
  });
  test('legacy base64 create and invalid ID', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (value) async {
          expect(value['opts']['name'], '合成测试');
        },
        log: (_) {});
    final encoded = base64Encode(utf8.encode('{"opts":{"name":"合成测试"}}'));
    expect((await rpc.create(jsonEncode(encoded), confirm: true)).status, 202);
    expect(
        (await rpc.create(body, operationId: 'bad/id', confirm: true)).status,
        400);
  });
  test(
      'legacy one-phase browser capture is rejected regardless of confirmation preference',
      () async {
    var enabled = true;
    var confirm = true;
    var opens = 0;
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
      capturePreferences: (_, __) => {'enabled': enabled, 'confirm': confirm},
      send: (_, id, __) async {
        sends++;
        return response(created(id!));
      },
      showCreate: (_) async {
        opens++;
      },
      log: (_) {},
    );
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) => rpc.handle(RpcContext(r, r.response),
        create: true, forceConfirmation: true));
    final client = _RealHttp().createHttpClient(null);
    Future<int> capture(String id, bool silent) async {
      final request = await client
          .postUrl(Uri.parse('http://127.0.0.1:${server.port}/create'));
      request.headers.set(operationHeader, id);
      request.headers.set(
          'X-Kownload-Host-Meta',
          jsonEncode({
            'source': 'browser-capture',
            'clientId': 'chrome-one',
            'browser': 'chrome',
            'silent': silent,
          }));
      request.write(body);
      final result = await request.close();
      await result.drain<void>();
      return result.statusCode;
    }

    try {
      expect(await capture('canonical-confirm', true), 409);
      enabled = false;
      expect(await capture('canonical-off', true), 409);
      enabled = true;
      confirm = false;
      expect(await capture('canonical-direct', false), 409);
      expect(sends, 0);
      expect(opens, 0);
    } finally {
      client.close(force: true);
      await server.close(force: true);
    }
  });

  test(
      'HTTP automatic ACK loss can query the strictly created receipt without replay',
      () async {
    final upstream = Completer<Response<dynamic>>(),
        entered = Completer<void>(),
        presented = Completer<void>();
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
        capturePreferences: (_, __) => {'enabled': true, 'confirm': false},
        send: (_, __, ___) {
          sends++;
          entered.complete();
          return upstream.future;
        },
        showCreate: (_) async {},
        log: (_) {},
        showResult: (_, __) async {
          presented.complete();
        });
    final app = BrowserIntegration(
        read: () => null,
        write: (_) async {
          fail('receipt must not persist');
        },
        submission: rpc.confirmationStatus);
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) async {
      try {
        final ctx = RpcContext(r, r.response);
        if (r.uri.path == '/create')
          await rpc.handle(ctx, create: true);
        else
          await app.handle(ctx);
      } catch (_) {
        try {
          await r.response.close();
        } catch (_) {}
      }
    });
    final lostClient = _RealHttp().createHttpClient(null),
        queryClient = _RealHttp().createHttpClient(null);
    try {
      final r = await lostClient
          .postUrl(Uri.parse('http://127.0.0.1:${server.port}/create'));
      r.headers.set(operationHeader, 'ack-lost');
      r.headers.set(
          'X-Kownload-Host-Meta',
          jsonEncode({
            'source': 'browser-media',
            'clientId': 'one',
            'browser': 'chrome',
            'silent': true
          }));
      r.write(body);
      final dropped = () async {
        try {
          final reply = await r.close();
          await reply.drain<void>();
        } catch (_) {}
      }();
      await entered.future;
      lostClient.close(force: true);
      await dropped;
      upstream.complete(response(created('ack-lost')));
      await presented.future;
      final query = await queryClient
          .postUrl(Uri.parse('http://127.0.0.1:${server.port}/browser'));
      query.write(
          jsonEncode({'action': 'submission', 'operationId': 'ack-lost'}));
      final result = await query.close();
      final value = jsonDecode(await utf8.decoder.bind(result).join());
      expect(value['data'], {
        'state': 'created',
        'taskIds': ['task-one']
      });
      expect(sends, 1);
    } finally {
      lostClient.close(force: true);
      queryClient.close(force: true);
      await server.close(force: true);
    }
  });

  test('HTTP RPC fixture preserves accepted and upstream 500', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => response(
            {'code': 1000, 'msg': 'fixture-secret', 'data': 'fixture-secret'},
            500),
        showCreate: (_) async {},
        log: (_) {});
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) =>
        rpc.handle(RpcContext(r, r.response), create: r.uri.path == '/create'));
    final client = _RealHttp().createHttpClient(null);
    try {
      for (final route in ['/create', '/forward']) {
        final request = await client
            .postUrl(Uri.parse('http://127.0.0.1:${server.port}$route'));
        request.headers.set(operationHeader, 'rpc-fixture');
        request.write(route == '/create'
            ? body
            : jsonEncode({
                'method': 'POST',
                'path': '/api/v1/tasks',
                'data': jsonDecode(body)
              }));
        final reply = await request.close();
        final data = jsonDecode(await utf8.decoder.bind(reply).join());
        expect(reply.statusCode, route == '/create' ? 202 : 500);
        expect(data['operationId'], 'rpc-fixture');
        expect(jsonEncode(data), isNot(contains('fixture-secret')));
      }
    } finally {
      client.close(force: true);
      await server.close(force: true);
    }
  });
  test('silent response loss retries REST with the original operation',
      () async {
    var calls = 0;
    final ids = <String?>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, id, __) async {
          ids.add(id);
          if (calls++ == 0) throw StateError('simulated response loss');
          return response(created(id!));
        },
        showCreate: (_) async {},
        log: (_) {});
    expect(
        (await rpc.create(body, operationId: 'lost-reply', confirm: false))
            .body['error'],
        'request_failed');
    expect(
        (await rpc.create(body, operationId: 'lost-reply', confirm: false))
            .body['data'],
        'task-one');
    expect(ids, ['lost-reply', 'lost-reply']);
  });
}
