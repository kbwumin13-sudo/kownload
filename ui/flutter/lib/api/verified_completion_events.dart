import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';
import 'package:dio/dio.dart';
import 'api.dart' hide TimeoutException;
import 'model/verified_completion_event.dart';

class CompletionEventsException implements Exception {
  final String reason;
  final bool terminal;
  const CompletionEventsException(this.reason, {required this.terminal});
}

/// Owns one live response including the case where its stream was never read.
class VerifiedCompletionConnection {
  final Stream<VerifiedCompletionEvent> events;
  final Future<void> Function() _close;
  Future<void>? _closing;
  VerifiedCompletionConnection(
      {required this.events, required Future<void> Function() close})
      : _close = close;
  Future<void> close() => _closing ??= _close();
}

typedef CompletionEventsOpener = Future<VerifiedCompletionConnection> Function(
    ActiveApiSnapshot snapshot, String datasetId, CancelToken cancelToken);

Future<VerifiedCompletionConnection> openVerifiedCompletionEvents(
    ActiveApiSnapshot snapshot,
    String datasetId,
    CancelToken cancelToken) async {
  final response =
      await verifiedCompletionEventsRequest(snapshot, datasetId, cancelToken);
  final raw = response.data;
  if (raw is! ResponseBody) {
    cancelToken.cancel('completion_protocol');
    throw const CompletionEventsException('response_protocol', terminal: true);
  }
  Future<void> discard() async {
    cancelToken.cancel('completion_response_closed');
    await raw.stream.listen((_) {}, onError: (Object _) {}).cancel();
  }

  String header(String name) {
    try {
      return response.headers.value(name)?.trim().toLowerCase() ?? '';
    } catch (_) {
      return '';
    }
  }

  if (response.statusCode != 200) {
    var transient = false;
    var consumed = false;
    try {
      if (response.statusCode == 503 &&
          header(Headers.contentTypeHeader).split(';').first ==
              'application/json') {
        consumed = true;
        final pending = _errorBody(raw.stream);
        Object? body;
        try {
          body = await pending.timeout(const Duration(seconds: 5));
        } on TimeoutException {
          cancelToken.cancel('completion_error_timeout');
          try {
            await pending;
          } catch (_) {}
        }
        transient = body is Map &&
            body.length == 3 &&
            body.keys.every(const {'code', 'msg', 'data'}.contains) &&
            body['code'] is int &&
            body['code'] == 1000 &&
            body['data'] == null &&
            const [
              'completion_events_capacity',
              'completion_events_unavailable'
            ].contains(body['msg']);
      }
    } finally {
      // _errorBody already consumed the response when present.
      cancelToken.cancel('completion_http_error');
      if (!consumed) {
        await raw.stream.listen((_) {}, onError: (Object _) {}).cancel();
      }
    }
    throw CompletionEventsException('http_status', terminal: !transient);
  }
  if (header(Headers.contentTypeHeader) != 'text/event-stream' ||
      header('cache-control') != 'no-store') {
    await discard();
    throw const CompletionEventsException('response_protocol', terminal: true);
  }
  late StreamController<VerifiedCompletionEvent> output;
  StreamSubscription<Uint8List>? upstream;
  var closed = false;
  Future<void>? closing;
  Future<void> close() => closing ??= () async {
        closed = true;
        cancelToken.cancel('completion_stream_closed');
        if (upstream != null) {
          await upstream!.cancel();
        } else {
          await raw.stream.listen((_) {}, onError: (Object _) {}).cancel();
        }
        if (!output.isClosed) unawaited(output.close());
      }();
  final frame = <int>[];
  var lineStart = 0, lineCount = 0;
  VerifiedCompletionEvent? event;
  void fail() {
    if (closed) return;
    output.addError(
        const CompletionEventsException('frame_protocol', terminal: true));
    unawaited(close());
  }

  void chunk(Uint8List bytes) {
    for (final byte in bytes) {
      if (closed) return;
      if (frame.length >= 4096) {
        fail();
        return;
      }
      frame.add(byte);
      if (byte != 10) continue;
      try {
        if (frame.length - 1 - lineStart >= 3 &&
            frame[lineStart] == 0xef &&
            frame[lineStart + 1] == 0xbb &&
            frame[lineStart + 2] == 0xbf) {
          fail();
          return;
        }
        final line = utf8.decode(frame.sublist(lineStart, frame.length - 1));
        lineStart = frame.length;
        if (lineCount == 0) {
          if (line != 'event: accepted-done') {
            fail();
            return;
          }
          lineCount = 1;
        } else if (lineCount == 1) {
          if (!line.startsWith('data: ')) {
            fail();
            return;
          }
          event =
              VerifiedCompletionEvent.fromJson(jsonDecode(line.substring(6)));
          if (event!.datasetId != datasetId) {
            fail();
            return;
          }
          lineCount = 2;
        } else {
          if (line.isNotEmpty) {
            fail();
            return;
          }
          output.add(event!);
          frame.clear();
          lineStart = 0;
          lineCount = 0;
          event = null;
        }
      } catch (_) {
        fail();
        return;
      }
    }
  }

  output = StreamController<VerifiedCompletionEvent>(
      sync: true,
      onListen: () {
        upstream = raw.stream.listen(chunk, onError: (Object _) {
          if (!closed) {
            output.addError(const CompletionEventsException(
                'stream_disconnected',
                terminal: false));
            unawaited(close());
          }
        }, onDone: () {
          // A partial frame at EOF carries no permission.
          if (!closed) unawaited(close());
        });
      },
      onCancel: close);
  return VerifiedCompletionConnection(events: output.stream, close: close);
}

Future<Object?> _errorBody(Stream<Uint8List> stream) async {
  final bytes = <int>[];
  await for (final chunk in stream) {
    if (bytes.length + chunk.length > 4096) {
      throw const CompletionEventsException('error_protocol', terminal: true);
    }
    bytes.addAll(chunk);
  }
  try {
    return jsonDecode(utf8.decode(bytes));
  } catch (_) {
    throw const CompletionEventsException('error_protocol', terminal: true);
  }
}
