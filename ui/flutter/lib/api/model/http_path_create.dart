import 'dart:convert';

import 'create_task_batch.dart';

const httpPathCreateKind = 'http-path-create/v1';
const httpPathCapability = 'single-http-path-freeze/v1';

/// Caller provenance, never a frozen Core decision or an IO permission.
class HttpPathIntent {
  final String path;
  final String origin;
  HttpPathIntent({required this.path, required this.origin}) {
    if (!const {
      'default',
      'user_selected',
      'manual_category',
      'api_explicit',
      'source_suggested',
      'unknown'
    }.contains(origin)) {
      throw ArgumentError('invalid_path_origin');
    }
  }
  Map<String, dynamic> toJson() =>
      {'version': 1, 'path': path, 'origin': origin};
}

/// The caller's immutable intent. Server configuration and frozen output
/// layout belong to R/F, and are deliberately absent from this request.
class HttpPathCreateRequest {
  final HttpPathIntent pathIntent;
  final String? expectedSHA256;
  final String _request;

  HttpPathCreateRequest(CreateTaskBatch batch,
      {required this.pathIntent, this.expectedSHA256})
      : _request = _validate(batch, pathIntent, expectedSHA256);

  List<String> get requiredCapabilities => [
        httpPathCapability,
        if (expectedSHA256 != null) 'http-sha256-completion/v1',
      ];

  bool matchesBatch(CreateTaskBatch batch) =>
      _validate(batch, pathIntent, expectedSHA256) == _request;

  static String _validate(
      CreateTaskBatch batch, HttpPathIntent intent, String? expected) {
    if (batch.reqs?.length != 1 || batch.opts != null) {
      throw ArgumentError('http_path_single_request_required');
    }
    final item = batch.reqs!.single;
    final req = item.req;
    final uri = Uri.tryParse(req?.url ?? '');
    if (req == null ||
        uri == null ||
        !const {'http', 'https'}.contains(uri.scheme) ||
        uri.host.isEmpty ||
        (req.protocol != null && req.protocol != 'http') ||
        item.opts?.selectFiles.isNotEmpty == true) {
      throw ArgumentError('http_path_direct_request_required');
    }
    if (item.opts == null || item.opts!.path != intent.path) {
      throw ArgumentError('path_intent_mismatch');
    }
    if (expected != null &&
        RegExp(r'^[0-9a-f]{64}$').stringMatch(expected) != expected) {
      throw ArgumentError('invalid_expected_sha256');
    }
    final raw = jsonDecode(jsonEncode(item.toJson())) as Map;
    final extra = (raw['opts'] as Map?)?['extra'];
    if (expected != null &&
        extra is Map &&
        (extra['autoTorrent'] == true || extra['autoExtract'] == true)) {
      throw ArgumentError('unsupported_option');
    }
    return jsonEncode(raw);
  }

  Map<String, dynamic> toJson(String operationId) => {
        'version': 1,
        'requiredCapabilities': requiredCapabilities,
        'operationId': operationId,
        'request': jsonDecode(_request),
        'pathIntent': pathIntent.toJson(),
        if (expectedSHA256 != null)
          'integrity': {
            'algorithm': 'sha256',
            'expectedSHA256': expectedSHA256
          },
      };
}
