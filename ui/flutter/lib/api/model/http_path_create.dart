import 'dart:convert';

import 'create_task_batch.dart';

const httpPathCreateKind = 'http-path-create/v1';
const httpPathCapability = 'single-http-path-freeze/v1';

enum PathCreateVariant {
  http('http-path', httpPathCreateKind, httpPathCapability, 'http'),
  bt('bt-path', 'bt-path-create/v1', 'bt-path-freeze/v1', 'bt'),
  ed2k('ed2k-path', 'ed2k-path-create/v1', 'ed2k-path-freeze/v1', 'ed2k'),
  netdisk('netdisk-path', 'netdisk-path-create/v1', 'netdisk-path-freeze/v1',
      'netdisk'),
  media('media-path', 'media-path-create/v1', 'media-path-freeze/v1', 'media');

  final String prefix, kind, capability, protocol;
  const PathCreateVariant(
      this.prefix, this.kind, this.capability, this.protocol);
}

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
  final PathCreateVariant variant;
  final String? batchId;
  final String? _request, _reqs;

  HttpPathCreateRequest(CreateTaskBatch batch,
      {required this.pathIntent,
      this.expectedSHA256,
      this.variant = PathCreateVariant.http})
      : batchId = null,
        _reqs = null,
        _request = _validate(batch, pathIntent, expectedSHA256, variant);

  /// Retains the original prepared array, including session/account/deferred
  /// metadata. It does not prepare again or split a directory into operations.
  HttpPathCreateRequest.netdisk(
      {required List<Map<String, dynamic>> reqs,
      required String batchId,
      required this.pathIntent})
      : variant = PathCreateVariant.netdisk,
        expectedSHA256 = null,
        batchId = batchId,
        _request = null,
        _reqs = _validatePrepared(reqs, batchId, pathIntent);

  List<String> get requiredCapabilities => [
        variant.capability,
        if (expectedSHA256 != null) 'http-sha256-completion/v1',
      ];

  CreateTaskBatch get batch => CreateTaskBatch.fromJson({
        'reqs': _reqs == null ? [jsonDecode(_request!)] : jsonDecode(_reqs!),
      });
  bool matchesBatch(CreateTaskBatch batch) => _reqs == null
      ? _validate(batch, pathIntent, expectedSHA256, variant) == _request
      : jsonEncode(batch.toJson()) == jsonEncode(this.batch.toJson());

  static String _validate(CreateTaskBatch batch, HttpPathIntent intent,
      String? expected, PathCreateVariant variant) {
    if (batch.reqs?.length != 1 || batch.opts != null) {
      throw ArgumentError('http_path_single_request_required');
    }
    final item = batch.reqs!.single;
    final req = item.req;
    if (variant == PathCreateVariant.netdisk) {
      throw ArgumentError('netdisk_prepared_batch_required');
    }
    if (expected != null && variant != PathCreateVariant.http) {
      throw ArgumentError('unsupported_integrity_variant');
    }
    final uri = Uri.tryParse(req?.url ?? '');
    if (req == null ||
        req.url.isEmpty ||
        (req.protocol != null && req.protocol != variant.protocol)) {
      throw ArgumentError('path_variant_protocol_mismatch');
    }
    if (variant == PathCreateVariant.http &&
        (uri == null ||
            !const {'http', 'https'}.contains(uri.scheme) ||
            uri.host.isEmpty ||
            item.opts?.selectFiles.isNotEmpty == true)) {
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

  static String _validatePrepared(
      List<Map<String, dynamic>> reqs, String batchId, HttpPathIntent intent) {
    if (batchId.isEmpty || reqs.isEmpty)
      throw ArgumentError('invalid_prepared_batch');
    final copy = jsonDecode(jsonEncode(reqs)) as List;
    for (final item in copy) {
      final req = item is Map ? item['req'] : null;
      final opts = item is Map ? item['opts'] : null;
      if (req is! Map ||
          req['protocol'] != 'netdisk' ||
          opts is! Map ||
          opts['path'] != intent.path ||
          req['extra'] is! Map ||
          req['extra']['batchId'] != batchId) {
        throw ArgumentError('prepared_path_intent_mismatch');
      }
    }
    return jsonEncode(copy);
  }

  Map<String, dynamic> toJson(String operationId) => {
        'version': 1,
        'requiredCapabilities': requiredCapabilities,
        'operationId': operationId,
        'request': _request == null ? null : jsonDecode(_request!),
        if (_reqs != null) 'reqs': jsonDecode(_reqs!),
        if (batchId != null) 'batchId': batchId,
        'pathIntent': pathIntent.toJson(),
        if (expectedSHA256 != null)
          'integrity': {
            'algorithm': 'sha256',
            'expectedSHA256': expectedSHA256
          },
      };
}
