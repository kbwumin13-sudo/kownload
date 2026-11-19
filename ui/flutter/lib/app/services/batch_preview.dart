import 'dart:convert';

import 'package:crypto/crypto.dart';

import '../rpc/rpc.dart';

const _maxEntries = 200;
const _maxBytes = 64 * 1024;
const _maxFieldBytes = 8192;
final _scope = RegExp(r'^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$');
final _http = RegExp(r'^https?://', caseSensitive: false);
final _invalidURL = RegExp(r'[\s\x00-\x1f\x7f\\]|%(?![0-9a-fA-F]{2})');
final _control = RegExp(r'[\x00-\x1f\x7f]');
const _headerNames = {
  'authorization': 'Authorization',
  'x-api-key': 'X-Api-Key',
  'x-auth-token': 'X-Auth-Token',
  'x-playback-token': 'X-Playback-Token',
  'cookie': 'Cookie',
  'referer': 'Referer',
  'origin': 'Origin',
  'user-agent': 'User-Agent',
  'accept-encoding': 'Accept-Encoding',
};

Uri? _parseURL(String raw) {
  if (!_http.hasMatch(raw) || _invalidURL.hasMatch(raw)) return null;
  final uri = Uri.tryParse(raw);
  if (uri == null ||
      uri.host.isEmpty ||
      uri.authority.contains('@') ||
      uri.port < 1 ||
      uri.port > 65535) {
    return null;
  }
  return uri;
}

enum BatchLinkIssue { unsupported, malformed, tooLong }

class BatchPreviewRow {
  final int number;
  final String url;
  final Map<String, String> headers;
  final BatchLinkIssue? issue;
  final String? label;

  BatchPreviewRow._(
    this.number,
    this.url,
    Map<String, String> headers,
    this.issue,
    this.label,
  ) : headers = Map.unmodifiable(headers);
}

class BatchPreviewDraft {
  final List<BatchPreviewRow> rows;
  final int duplicates;
  final bool fromBrowser;
  final bool tooLarge;

  BatchPreviewDraft._(
    List<BatchPreviewRow> rows,
    this.duplicates, {
    required this.fromBrowser,
    this.tooLarge = false,
  }) : rows = List.unmodifiable(rows);

  factory BatchPreviewDraft.text(String text) {
    final lines =
        text.length > _maxBytes || utf8.encode(text).length > _maxBytes
            ? null
            : const LineSplitter()
                .convert(text)
                .map((line) => line.trim())
                .where((line) => line.isNotEmpty)
                .toList();
    if (lines == null || lines.length > _maxEntries) {
      return BatchPreviewDraft._([], 0, fromBrowser: false, tooLarge: true);
    }
    return _build(
      lines.map((url) => (url, <String, String>{}, '')),
      fromBrowser: false,
    );
  }

  factory BatchPreviewDraft.browser(Map<String, dynamic> json) {
    const invalid = FormatException('invalid_batch_preview');
    final items = json['items'];
    if (json.length != 2 ||
        json['version'] is! int ||
        json['version'] != 1 ||
        items is! List ||
        items.isEmpty ||
        items.length > _maxEntries) {
      throw invalid;
    }
    final input = <(String, Map<String, String>, String)>[];
    for (final item in items) {
      if (item is! Map ||
          item.keys.any(
            (k) => !const {'url', 'header', 'context'}.contains(k),
          )) {
        throw invalid;
      }
      final url = item['url'];
      final raw = item['header'];
      final scope = item['context'] ?? '';
      if (url is! String ||
          utf8.encode(url).length > _maxFieldBytes ||
          scope is! String ||
          (raw != null && raw is! Map)) {
        throw invalid;
      }
      final headers = <String, String>{};
      final target = _parseURL(url);
      if (raw is Map) {
        for (final e in raw.entries) {
          final name = e.key is String
              ? _headerNames[(e.key as String).toLowerCase()]
              : null;
          final value = e.value;
          if (name == null ||
              value is! String ||
              value.isEmpty ||
              utf8.encode(value).length > _maxFieldBytes ||
              _control.hasMatch(value) ||
              headers.containsKey(name)) {
            throw invalid;
          }
          if ((name == 'Referer' || name == 'Origin') &&
              (target == null || _parseURL(value)?.origin != target.origin)) {
            throw invalid;
          }
          headers[name] = value;
        }
      }
      if (headers.isEmpty
          ? scope.isNotEmpty
          : target == null || !_scope.hasMatch(scope)) {
        throw invalid;
      }
      input.add((url, headers, scope));
    }
    return _build(input, fromBrowser: true);
  }

  static BatchPreviewDraft _build(
    Iterable<(String, Map<String, String>, String)> input, {
    required bool fromBrowser,
  }) {
    final seen = <String>{};
    final rows = <BatchPreviewRow>[];
    var number = 0, duplicates = 0;
    for (final (url, header, context) in input) {
      number++;
      final uri = _parseURL(url);
      final issue = utf8.encode(url).length > _maxFieldBytes
          ? BatchLinkIssue.tooLong
          : !_http.hasMatch(url)
              ? BatchLinkIssue.unsupported
              : uri == null
                  ? BatchLinkIssue.malformed
                  : null;
      if (issue != null) {
        rows.add(BatchPreviewRow._(number, '', const {}, issue, null));
        continue;
      }
      final names = header.keys.toList()..sort();
      final key = jsonEncode([
        url,
        context,
        for (final name in names) [name, header[name]],
      ]);
      if (!seen.add(key)) {
        duplicates++;
        continue;
      }
      // Path segments themselves also sometimes contain signed tokens.
      final marker =
          sha256.convert(utf8.encode(url)).toString().substring(0, 8);
      final path = uri!.pathSegments
          .where((s) => s.isNotEmpty)
          .map((_) => '…')
          .join('/');
      final host = uri.host.contains(':') ? '[${uri.host}]' : uri.host;
      rows.add(
        BatchPreviewRow._(
          number,
          url,
          header,
          null,
          '$host${uri.hasPort ? ':${uri.port}' : ''}/$path${uri.hasQuery ? '?…' : ''} · $marker',
        ),
      );
    }
    return BatchPreviewDraft._(rows, duplicates, fromBrowser: fromBrowser);
  }
}

/// Private Unix socket import. Acknowledges admission to the UI only.
class BatchPreviewInbox {
  Future<bool> Function(BatchPreviewDraft)? open;

  Future<void> handle(RpcContext ctx) async {
    if (ctx.request.method != 'POST' ||
        ctx.request.contentLength <= 0 ||
        ctx.request.contentLength > _maxBytes) {
      await ctx.writeError('invalid_batch_preview', 400);
      return;
    }
    final BatchPreviewDraft draft;
    try {
      draft = BatchPreviewDraft.browser(await ctx.readJSON());
    } catch (_) {
      await ctx.writeError('invalid_batch_preview', 400);
      return;
    }
    if (await open?.call(draft) != true) {
      await ctx.writeError('batch_preview_busy', 409);
      return;
    }
    await ctx.writeJSON({
      'code': 0,
      'data': {'version': 1},
    });
  }
}
