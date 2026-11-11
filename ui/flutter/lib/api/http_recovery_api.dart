import 'dart:convert';
import 'package:dio/dio.dart';
import 'api.dart' as api;
import 'model/http_recovery.dart';
import 'model/task_batch_catalog.dart';

class HttpRecoveryReply {
  final int? statusCode, businessCode;
  final HttpRecovery? recovery;
  const HttpRecoveryReply(this.statusCode, this.businessCode, this.recovery);
}

/// Explicit retry uses the current API and dataset. No fallback or automatic
/// POST retries; a failed POST leaves its caller's original RAM tuple intact.
class HttpRecoveryApi {
  final api.ActiveApiSnapshot snapshot;
  final DatasetIdentity identity;
  final String taskId;
  HttpRecoveryApi(
      {required this.snapshot, required this.identity, required this.taskId}) {
    if (!identity.supported ||
        RegExp(r'^[A-Za-z0-9_-]{1,128}$').stringMatch(taskId) != taskId) {
      throw const FormatException('invalid_recovery_scope');
    }
  }
  static Future<DatasetIdentity?> readIdentity(api.ActiveApiSnapshot snapshot,
      {CancelToken? cancelToken}) async {
    final r = await api.httpRecoveryRequest(
        snapshot, 'GET', '/api/v1/dataset/identity',
        cancelToken: cancelToken);
    final body = _json(r.data);
    if (!snapshot.isCurrent ||
        r.statusCode != 200 ||
        body is! Map ||
        body['code'] is! int ||
        body['code'] != 0 ||
        body['data'] is! Map) {
      return null;
    }
    try {
      final id =
          DatasetIdentity.fromJson(Map<String, dynamic>.from(body['data']));
      return id.supported ? id : null;
    } on Object {
      return null;
    }
  }

  Future<bool> sameDataset({CancelToken? cancelToken}) async =>
      (await readIdentity(snapshot, cancelToken: cancelToken))?.datasetId ==
      identity.datasetId;
  String get _root =>
      '/api/v1/datasets/${identity.datasetId}/tasks/${Uri.encodeComponent(taskId)}/recovery/v1';
  Future<HttpRecoveryReply> query(
          {HttpRetryAction? action, CancelToken? cancelToken}) =>
      _send('GET', _root, action, cancelToken);
  Future<HttpRecoveryReply> retry(HttpRetryAction action,
          {CancelToken? cancelToken}) =>
      _send('POST', '$_root/retry', action, cancelToken);
  Future<HttpRecoveryReply> _send(String method, String path,
      HttpRetryAction? action, CancelToken? cancelToken) async {
    Response? r;
    try {
      r = await api.httpRecoveryRequest(snapshot, method, path,
          data: method == 'POST' ? action!.toJson() : null,
          queryParameters: method == 'GET' ? action?.toQuery() : null,
          cancelToken: cancelToken);
      final body = _json(r.data);
      final code =
          body is Map && body['code'] is int ? body['code'] as int : null;
      HttpRecovery? recovery;
      if (snapshot.isCurrent &&
          r.statusCode == 200 &&
          code == 0 &&
          body is Map) {
        final d = body['data'];
        if (d is Map &&
            d['scopeVersion'] is int &&
            d['scopeVersion'] == 1 &&
            d['datasetId'] == identity.datasetId &&
            d['recovery'] is Map) {
          recovery = HttpRecovery.parse(d['recovery'], taskId, action: action);
        }
      }
      return HttpRecoveryReply(r.statusCode, code, recovery);
    } on DioException {
      return HttpRecoveryReply(r?.statusCode, null, null);
    } on FormatException {
      return HttpRecoveryReply(r?.statusCode, null, null);
    }
  }

  static Object? _json(Object? data) {
    if (data is! String) return data;
    try {
      return jsonDecode(data);
    } on FormatException {
      return null;
    }
  }
}
