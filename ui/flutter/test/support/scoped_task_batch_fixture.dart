import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:crypto/crypto.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/database/task_batch_journal.dart';

class ScopedBatchFixture {
  late HttpServer server;
  late StreamSubscription<HttpRequest> subscription;
  String dataset = 'a' * 32;
  String mode = 'success';
  bool persistent = true, unsupportedPreflight = false, verified = false;
  Completer<void>? postGate;
  int inFlight = 0;
  final requests = <Map<String, dynamic>>[];
  final receipts = <String, Map<String, dynamic>>{};
  Future<void> Function(HttpRequest)? override;
  String get address => '127.0.0.1:${server.port}';
  JournalRouteRef route([String char = 'b']) => JournalRouteRef(
      product: 'kownload', identityProtocol: 1, routeScope: char * 64);
  void init([String char = 'b']) =>
      api.init('tcp', address, 'fixture-scoped-token',
          taskBatchRoute: route(char));
  Future<void> start() async {
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    subscription = server.listen((req) async {
      inFlight++;
      try {
        req.response.persistentConnection = false;
        final text = await utf8.decoder.bind(req).join();
        requests.add({
          'method': req.method,
          'uri': req.uri,
          'body': text.isEmpty ? null : jsonDecode(text),
          'token': req.headers.value('X-Api-Token')
        });
        if (override != null) {
          await override!(req);
          return;
        }
        await handle(req);
      } finally {
        inFlight--;
      }
    });
    init();
  }

  void reset() {
    dataset = 'a' * 32;
    mode = 'success';
    verified = false;
    persistent = true;
    unsupportedPreflight = false;
    postGate = null;
    override = null;
    requests.clear();
    receipts.clear();
    init();
  }

  Map<String, dynamic> receipt(String op,
      {String state = 'settled', int count = 2}) {
    final members = <Map<String, dynamic>>[];
    for (var i = 0; i < count; i++) {
      members.add({
        'index': i,
        'taskId': '$op-task-$i',
        'state': state == 'pending' && i > 0 ? 'pending' : 'created'
      });
    }
    if (state == 'unknown') members.clear();
    return {
      'version': 1,
      'operationId': op,
      'state': state,
      'members': members,
      'taskIds': members
          .where((m) => m['state'] == 'created')
          .map((m) => m['taskId'])
          .toList(),
      'createdCount': members.where((m) => m['state'] == 'created').length,
      'failedCount': 0,
      'pendingCount': members.where((m) => m['state'] == 'pending').length
    };
  }

  Map<String, dynamic> wrapper(Map<String, dynamic> operation,
          {String? domain}) =>
      {
        'scopeVersion': 1,
        'datasetId': domain ?? dataset,
        'namespace': 'task-batch.create/v1',
        'operation': operation,
        if (verified) 'capability': 'http-sha256-completion/v1'
      };
  Future<void> respond(HttpRequest req, Object? data,
      {int status = 200, int code = 0, String msg = ''}) async {
    req.response.statusCode = status;
    req.response.headers.contentType = ContentType.json;
    req.response.write(jsonEncode({'code': code, 'msg': msg, 'data': data}));
    await req.response.close();
  }

  Future<void> handle(HttpRequest req) async {
    final path = req.uri.path;
    if (path == '/api/v1/dataset/identity') {
      await respond(
          req, {'version': 1, 'datasetId': dataset, 'persistent': persistent});
      return;
    }
    if (path == '/api/v1/task-batches/operations' && req.method == 'GET') {
      await respond(req, {
        'version': 1,
        'namespace': 'task-batch.create/v1',
        'datasetId': dataset,
        'persistent': persistent,
        'consistency': 'live-keyset',
        'filter': req.uri.queryParameters['filter'] ?? 'all',
        'items': receipts.entries
            .map((e) => {
                  'keyHash': sha256.convert(utf8.encode(e.key)).toString(),
                  'operationId': e.key,
                  'state': e.value['state'],
                  'counts': {
                    'members': (e.value['members'] as List).length,
                    'created': e.value['createdCount'],
                    'failed': e.value['failedCount'],
                    'pending': e.value['pendingCount']
                  }
                })
            .toList(),
        'hasMore': false,
        'scanned': receipts.length
      });
      return;
    }
    final root = '/api/v1/datasets/$dataset/verified-http/v1';
    if (path == '$root/capability' && verified) {
      if (unsupportedPreflight) {
        req.response.statusCode = 404;
        req.response.write('<html>old core</html>');
        await req.response.close();
      } else {
        await respond(req, {
          'scopeVersion': 1,
          'datasetId': dataset,
          'capability': 'http-sha256-completion/v1',
          'requestVersion': 1,
          'projectionVersion': 1,
          'supported': true,
          'methods': {'create': 'POST', 'query': 'GET', 'cancel': 'POST'}
        });
      }
      return;
    }
    final prefix = verified
        ? '$root/operations'
        : '/api/v1/datasets/$dataset/task-batches/v1/operations';
    if (path != prefix && !path.startsWith('$prefix/')) {
      await respond(req, null,
          status: 409, code: 1000, msg: 'dataset_mismatch');
      return;
    }
    if (req.method == 'GET') {
      if (unsupportedPreflight) {
        req.response.statusCode = 404;
        req.response.write('<html>old core</html>');
        await req.response.close();
        return;
      }
      final op = Uri.decodeComponent(req.uri.pathSegments.last);
      await respond(
          req, wrapper(receipts[op] ?? receipt(op, state: 'unknown')));
      return;
    }
    final body = requests.last['body'] as Map?;
    final op = body?['operationId'] as String? ?? 'cancel';
    if (postGate != null) await postGate!.future;
    receipts[op] = receipt(op,
        state: mode == 'partial' && !verified ? 'pending' : 'settled',
        count: verified ? 1 : 2);
    if (mode == 'lostAck') {
      final socket = await req.response.detachSocket(writeHeaders: false);
      socket.destroy();
      return;
    }
    if (mode == 'html') {
      req.response.statusCode = 404;
      req.response.write('<html>unknown ack</html>');
      await req.response.close();
      return;
    }
    if (mode == 'redirect') {
      req.response.statusCode = 307;
      req.response.headers.set('location', '/must-not-follow');
      await req.response.close();
      return;
    }
    await respond(req,
        wrapper(receipts[op]!, domain: mode == 'wrongDomain' ? 'c' * 32 : null),
        status: mode == 'partial' ? 503 : 200,
        code: mode == 'partial' ? 1000 : 0,
        msg: mode == 'partial' ? 'operation_unavailable_query_same_id' : '');
  }

  int get posts => requests.where((r) => r['method'] == 'POST').length;
  int get scopedGets => requests
      .where((r) =>
          r['method'] == 'GET' && (r['uri'] as Uri).path.contains('/datasets/'))
      .length;
  Future<void> close() async {
    await subscription.cancel();
    await server.close(force: true);
  }
}
