import 'dart:io';
import 'scoped_task_batch_fixture.dart';

class HttpRecoveryFixture extends ScopedBatchFixture {
  String replyMode = 'managed';
  String taskId = '_task-1';
  String episode = 'ep-a', status = 'error';
  @override
  void reset() {
    super.reset();
    replyMode = 'managed';
    episode = 'ep-a';
    status = 'error';
  }

  Map<String, dynamic> view({String management = 'managed', Map? action}) {
    final result = <String, dynamic>{
      'version': 1,
      'taskId': taskId,
      'status': status,
      'management': management,
      'retryAllowed': management == 'managed' && status == 'error',
      'reason': management == 'managed'
          ? status == 'error'
              ? 'explicit_retry_available'
              : 'not_error'
          : management == 'legacy'
              ? 'not_managed'
              : 'recovery_held'
    };
    if (management != 'managed') return result;
    result.addAll({
      'episode': action == null
          ? episode
          : action['expectedEpisode'] == 'ep-b'
              ? 'ep-c'
              : 'ep-b',
      'generation': 7,
      'sourceRevision': 1,
      'totalUsed': 6,
      'transportUsed': 6
    });
    if (action != null) {
      final accepted = !const ['stale', 'not_recorded'].contains(replyMode);
      if (accepted) {
        result['lastExplicitRetry'] = {
          'version': 1,
          'actionId': action['actionId'],
          'fromEpisode': action['expectedEpisode'],
          'toEpisode': action['expectedEpisode'] == 'ep-b' ? 'ep-c' : 'ep-b',
          'sourceRevision': int.parse('${action['expectedSourceRevision']}')
        };
      }
      result['receipt'] = {
        'version': 1,
        'actionId': action['actionId'],
        'expectedEpisode': action['expectedEpisode'],
        'expectedSourceRevision':
            int.parse('${action['expectedSourceRevision']}'),
        'decision': 'retry',
        'state': accepted ? 'accepted' : replyMode,
        if (accepted)
          'newEpisode': action['expectedEpisode'] == 'ep-b' ? 'ep-c' : 'ep-b',
        'queryRequired': replyMode == 'not_recorded'
      };
    }
    return result;
  }

  @override
  Future<void> handle(HttpRequest q) async {
    if (q.uri.path == '/api/v1/dataset/identity') return super.handle(q);
    if (!q.uri.path.contains('/recovery/v1')) return super.handle(q);
    if (replyMode == 'html') {
      q.response.statusCode = 404;
      q.response.write('<html>old</html>');
      await q.response.close();
      return;
    }
    if (replyMode == 'redirect') {
      q.response.statusCode = 307;
      q.response.headers.set('location', '/must-not-follow');
      await q.response.close();
      return;
    }
    if (q.method == 'POST' && replyMode == 'lostAck') {
      final socket = await q.response.detachSocket(writeHeaders: false);
      socket.destroy();
      return;
    }
    if (const ['409', '503'].contains(replyMode)) {
      return respond(q, null,
          status: int.parse(replyMode),
          code: 1000,
          msg: 'explicit_retry_unavailable_query_same_action');
    }
    final body = requests.last['body'] as Map?;
    final action = q.method == 'POST'
        ? body
        : q.uri.queryParameters.isEmpty
            ? null
            : q.uri.queryParameters;
    final data = view(
        management: const ['legacy', 'held'].contains(replyMode)
            ? replyMode
            : 'managed',
        action: action);
    if (replyMode == 'badVersion') data['version'] = 2;
    await respond(q, {
      'scopeVersion': 1,
      'datasetId': replyMode == 'wrongDataset' ? 'c' * 32 : dataset,
      'recovery': data
    });
  }
}
