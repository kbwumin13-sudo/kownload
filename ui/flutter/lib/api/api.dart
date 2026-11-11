import 'dart:async' as async;
import 'dart:io';

import 'package:dio/dio.dart';
import 'package:dio/io.dart';
import 'package:flutter/foundation.dart';
import 'package:get/get.dart' as getx;

import '../app/routes/app_pages.dart';
import '../database/database.dart';
import '../database/task_batch_journal.dart';
import '../util/util.dart';
import 'model/create_task.dart';
import 'model/create_task_batch.dart';
import 'model/downloader_config.dart';
import 'model/extension.dart';
import 'model/install_extension.dart';
import 'model/login.dart';
import 'model/resolve_result.dart';
import 'model/resolve_task.dart';
import 'model/result.dart';
import 'model/switch_extension.dart';
import 'model/task.dart';
import 'model/task_stats.dart';
import 'model/task_batch_catalog.dart';
import 'model/http_recovery.dart';
import 'model/update_check_extension_resp.dart';
import 'model/update_extension_settings.dart';

class _Client {
  static _Client? _instance;

  late Dio dio;
  late final String network, address;
  late final String _initialToken;

  _Client._internal();

  factory _Client(String network, String address, String apiToken) {
    if (_instance == null) {
      _instance = _Client._internal()
        ..network = network
        ..address = address
        .._initialToken = apiToken;
      var dio = Dio();
      final isUnixSocket = network == 'unix';
      var baseUrl = 'http://127.0.0.1/';
      if (!isUnixSocket) {
        if (Util.isWeb()) {
          baseUrl = kDebugMode ? 'http://127.0.0.1:9999/' : '';
        } else {
          baseUrl = 'http://$address/';
        }
      }
      dio.options.baseUrl = baseUrl;
      dio.options.contentType = Headers.jsonContentType;
      dio.options.sendTimeout = const Duration(seconds: 5);
      dio.options.connectTimeout = const Duration(seconds: 5);
      dio.options.receiveTimeout = const Duration(seconds: 60);
      dio.interceptors.add(InterceptorsWrapper(
        onRequest: (options, handler) {
          if (apiToken.isNotEmpty) {
            options.headers['X-Api-Token'] = apiToken;
          }
          if (Util.isWeb()) {
            final token = Database.instance.getWebToken();
            if (token != null) {
              options.headers['Authorization'] = 'Bearer $token';
            }
          }
          handler.next(options);
        },
        onError: (error, handler) {
          // Only web version has a login page
          if (Util.isWeb() && error.response?.statusCode == 401) {
            getx.Get.rootDelegate.offAndToNamed(Routes.LOGIN);
          }
          handler.next(error);
        },
      ));

      _instance!.dio = dio;
      if (isUnixSocket) {
        (_instance!.dio.httpClientAdapter as IOHttpClientAdapter)
            .createHttpClient = () {
          final client = HttpClient();
          client.connectionFactory =
              (Uri uri, String? proxyHost, int? proxyPort) {
            return Socket.startConnect(
                InternetAddress(address, type: InternetAddressType.unix), 0);
          };
          return client;
        };
      }
    }
    return _instance!;
  }
}

class TimeoutException implements Exception {
  final String message;

  TimeoutException(this.message);

  @override
  String toString() => message;
}

class ConnectionException implements Exception {
  final String message;

  ConnectionException(this.message);

  @override
  String toString() => message;
}

late _Client _client;

/// Immutable reference to the actual initialized transport. Contains no token.
class ActiveApiSnapshot {
  final String network, address;
  final JournalRouteRef? taskBatchRoute;
  final _Client _transport;
  const ActiveApiSnapshot._(
      this.network, this.address, this.taskBatchRoute, this._transport);
  bool get isCurrent => identical(_activeApiSnapshot, this);
}

ActiveApiSnapshot? _activeApiSnapshot;
final _activeApiChanges = ValueNotifier<ActiveApiSnapshot?>(null);
ValueListenable<ActiveApiSnapshot?> get activeApiSnapshotChanges =>
    _activeApiChanges;
ActiveApiSnapshot? get activeApiSnapshot => _activeApiSnapshot;

void init(String network, String address, String apiToken,
    {JournalRouteRef? taskBatchRoute}) {
  _client = _Client(network, address, apiToken);
  // Legacy init keeps its singleton semantics. Never relabel that live client
  // with a later configuration that it did not actually adopt.
  final matches = _client.network == network &&
      _client.address == address &&
      _client._initialToken == apiToken;
  _activeApiSnapshot = ActiveApiSnapshot._(
      _client.network,
      _client.address,
      matches && taskBatchRoute?.supported == true ? taskBatchRoute : null,
      _client);
  _activeApiChanges.value = _activeApiSnapshot;
}

/// Only the frozen read endpoints and dataset-scoped three actions. No redirect
/// replay, arbitrary URL, legacy POST fallback or global parser changes.
Future<Response<dynamic>> scopedTaskBatchRequest(
    ActiveApiSnapshot snapshot, String method, String path,
    {Map<String, dynamic>? data,
    Map<String, dynamic>? queryParameters,
    CancelToken? cancelToken}) async {
  var allowed = false;
  if (method == 'GET' && data == null) {
    if (path == '/api/v1/dataset/identity' && queryParameters == null) {
      allowed = true;
    } else if (path == '/api/v1/task-batches/operations') {
      final q = queryParameters ?? const <String, dynamic>{};
      allowed = q.keys.every(const {'filter', 'limit', 'cursor'}.contains) &&
          (q['filter'] == null ||
              const ['all', 'pending'].contains(q['filter'])) &&
          (q['limit'] == null ||
              (q['limit'] is int && q['limit'] >= 1 && q['limit'] <= 100)) &&
          (q['cursor'] == null ||
              (q['cursor'] is String &&
                  q['cursor'].isNotEmpty &&
                  q['cursor'].length <= 65536));
    }
  }
  final cap =
      RegExp(r'^/api/v1/datasets/[0-9a-f]{32}/verified-http/v1/capability$')
          .firstMatch(path);
  if (cap != null &&
      cap[0] == path &&
      method == 'GET' &&
      data == null &&
      queryParameters == null) {
    allowed = true;
  }
  final match = RegExp(
          r'^/api/v1/datasets/([0-9a-f]{32})/(?:task-batches/v1|verified-http/v1)/operations(?:/([^/]+)(/cancel)?)?$')
      .firstMatch(path);
  if (match != null && match[0] == path && queryParameters == null) {
    if (match[2] == null) {
      allowed = method == 'POST' && data != null;
    } else if (data == null) {
      try {
        final id = Uri.decodeComponent(match[2]!);
        allowed =
            RegExp(r'^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$').stringMatch(id) ==
                    id &&
                Uri.encodeComponent(id) == match[2] &&
                ((method == 'GET' && match[3] == null) ||
                    (method == 'POST' && match[3] == '/cancel'));
      } on FormatException {
        allowed = false;
      }
    }
  }
  if (!allowed) throw ArgumentError('invalid_scoped_batch_endpoint');
  if (!snapshot.isCurrent || snapshot.taskBatchRoute?.supported != true) {
    throw StateError('task_batch_route_changed');
  }
  try {
    return await snapshot._transport.dio.request(path,
        data: data,
        queryParameters: queryParameters,
        cancelToken: cancelToken,
        options: Options(method: method, followRedirects: false));
  } on DioException catch (error) {
    if (error.response != null) return error.response!;
    rethrow;
  }
}

/// Only the live completion stream. No query, recovery header or redirect.
Future<Response<dynamic>> verifiedCompletionEventsRequest(
    ActiveApiSnapshot snapshot,
    String datasetId,
    CancelToken cancelToken) async {
  if (RegExp(r'^[0-9a-f]{32}$').stringMatch(datasetId) != datasetId) {
    throw ArgumentError('invalid_completion_dataset');
  }
  if (!snapshot.isCurrent || snapshot.taskBatchRoute?.supported != true) {
    throw StateError('completion_api_changed');
  }
  final pending = snapshot._transport.dio.get<ResponseBody>(
      '/api/v1/datasets/$datasetId/verified-http/v1/completion-events',
      cancelToken: cancelToken,
      options: Options(
          responseType: ResponseType.stream,
          followRedirects: false,
          receiveTimeout: Duration.zero,
          headers: {'Accept': 'text/event-stream'}));
  try {
    // A local header deadline; the established stream may be idle indefinitely.
    return await pending.timeout(const Duration(seconds: 10));
  } on async.TimeoutException {
    cancelToken.cancel('completion_headers_timeout');
    // Keep the real request fence until cancellation/late response settles.
    try {
      final late = await pending;
      final raw = late.data;
      if (raw != null) {
        await raw.stream.listen((_) {}, onError: (Object _) {}).cancel();
      }
    } catch (_) {}
    throw ConnectionException('completion_headers_timeout');
  } on DioException catch (error) {
    if (error.response != null) return error.response!;
    rethrow;
  }
}

/// Only the versioned recovery read/retry routes, on the initialized client.
/// A response error remains data; redirects never replay a retry POST.
Future<Response<dynamic>> httpRecoveryRequest(
    ActiveApiSnapshot snapshot, String method, String path,
    {Map<String, dynamic>? data,
    Map<String, dynamic>? queryParameters,
    CancelToken? cancelToken}) async {
  var allowed = method == 'GET' &&
      path == '/api/v1/dataset/identity' &&
      data == null &&
      queryParameters == null;
  final match = RegExp(
          r'^/api/v1/datasets/[0-9a-f]{32}/tasks/[A-Za-z0-9_-]{1,128}/recovery/v1(/retry)?$')
      .firstMatch(path);
  if (match != null && match[0] == path) {
    if (method == 'GET' && match[1] == null && data == null) {
      allowed = queryParameters == null;
      if (queryParameters != null && queryParameters.length == 3) {
        final revision = queryParameters['expectedSourceRevision'];
        final value = revision is String ? int.tryParse(revision) : null;
        if (value != null && '$value' == revision) {
          try {
            final action = HttpRetryAction.fromJson({
              'version': 1,
              'decision': 'retry',
              ...queryParameters,
              'expectedSourceRevision': value
            });
            allowed =
                action.toQuery().keys.toSet().containsAll(queryParameters.keys);
          } on FormatException {
            allowed = false;
          }
        }
      }
    } else if (method == 'POST' &&
        match[1] == '/retry' &&
        queryParameters == null &&
        data != null) {
      try {
        HttpRetryAction.fromJson(data);
        allowed = true;
      } on FormatException {
        allowed = false;
      }
    }
  }
  if (!allowed) throw ArgumentError('invalid_http_recovery_endpoint');
  if (!snapshot.isCurrent || snapshot.taskBatchRoute?.supported != true) {
    throw StateError('http_recovery_route_changed');
  }
  try {
    return await snapshot._transport.dio.request(path,
        data: data,
        queryParameters: queryParameters,
        cancelToken: cancelToken,
        options: Options(method: method, followRedirects: false));
  } on DioException catch (error) {
    if (error.response != null) return error.response!;
    rethrow;
  }
}

/// S2-only raw envelope transport, sharing authentication and Unix routing.
/// Non-2xx data is retained; all other API parsing/redirect policies stay intact.
Future<Response<dynamic>> taskBatchOperationRequest(String method, String path,
    {Map<String, dynamic>? data}) async {
  const prefix = '/api/v1/task-batches/operations';
  var allowed = method == 'POST' && path == prefix;
  final match = RegExp(r'^/api/v1/task-batches/operations/([^/]+)(/cancel)?$')
      .firstMatch(path);
  if (match != null && data == null) {
    try {
      final id = Uri.decodeComponent(match[1]!);
      allowed =
          RegExp(r'^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$').stringMatch(id) ==
                  id &&
              Uri.encodeComponent(id) == match[1] &&
              ((method == 'GET' && match[2] == null) ||
                  (method == 'POST' && match[2] == '/cancel'));
    } on FormatException {
      allowed = false;
    }
  }
  if (!allowed) throw ArgumentError('invalid_task_batch_endpoint');
  try {
    return await _client.dio.request(path,
        data: data, options: Options(method: method, followRedirects: false));
  } on DioException catch (error) {
    if (error.response != null) return error.response!;
    rethrow;
  }
}

Future<T> _parse<T>(
  Future<Response> Function() fetch,
  T Function(dynamic json)? fromJsonT, {
  String timeoutMessage = '请求超时，请稍后重试',
}) async {
  try {
    var resp = await fetch();
    fromJsonT ??= (json) => null as T;
    final result = Result<T>.fromJson(resp.data, fromJsonT);
    if (result.code == 0) {
      return result.data as T;
    } else {
      throw Exception(result);
    }
  } on DioException catch (e) {
    if (e.type == DioExceptionType.connectionTimeout ||
        e.type == DioExceptionType.connectionError) {
      throw ConnectionException('无法连接下载服务，请检查服务是否运行及连接设置');
    }
    if (e.type == DioExceptionType.sendTimeout) {
      throw TimeoutException('发送请求超时，请检查下载服务连接或稍后重试');
    }
    if (e.type == DioExceptionType.receiveTimeout) {
      throw TimeoutException(timeoutMessage);
    }
    throw Exception(Result(code: 1000, msg: e.message));
  }
}

Future<ResolveResult> resolve(ResolveTask resolveTask) async {
  return _parse<ResolveResult>(
      () => _client.dio.post("api/v1/resolve", data: resolveTask),
      (data) => ResolveResult.fromJson(data),
      timeoutMessage:
          resolveTask.req?.url.toLowerCase().startsWith('magnet:') == true
              ? '获取种子信息超时，请检查网络或稍后重试'
              : '解析下载链接超时，请检查网络或稍后重试');
}

/// Fixed core endpoint; no fallback to ordinary create on unsupported cores.
Future<Map<String, dynamic>> browserHandoffRequest(
        String method, Map<String, dynamic> data) =>
    _parse<Map<String, dynamic>>(
        () => method == 'GET'
            ? _client.dio.get('/api/v1/browser/handoff')
            : _client.dio.post('/api/v1/browser/handoff', data: data),
        (value) => value is Map
            ? Map<String, dynamic>.from(value)
            : throw const FormatException('invalid_handoff_result'));

Future<String> createTask(CreateTask createTask, {String? operationId}) async {
  return _parse<String>(
      () => _client.dio.post("api/v1/tasks",
          data: createTask,
          options: operationId == null
              ? null
              : Options(headers: {
                  'X-Kownload-Operation-Id': operationId,
                })),
      (data) => data as String);
}

Future<List<String>> createTaskBatch(CreateTaskBatch createTaskBatch,
    {String? operationId}) async {
  return _parse<List<String>>(
      () => _client.dio.post("api/v1/tasks/batch",
          data: createTaskBatch,
          options: operationId == null
              ? null
              : Options(headers: {
                  'X-Kownload-Operation-Id': operationId,
                })),
      (data) => (data as List).map((e) => e as String).toList());
}

Future<void> patchTask(String id, ResolveTask patchTask) async {
  return _parse(
      () => _client.dio.patch("api/v1/tasks/$id", data: patchTask), null);
}

Future<List<Task>> getTasks(List<Status> statuses) async {
  final snapshot = activeApiSnapshot;
  final tasks = await _parse<List<Task>>(
      () => _client.dio.get(
          "/api/v1/tasks?${statuses.map((e) => "status=${e.name}").join("&")}"),
      (data) => (data as List).map((e) => Task.fromJson(e)).toList());
  await _bindCompletionViews(snapshot, tasks);
  return tasks;
}

/// Download diagnostics; older cores return only a connection list.
Future<TaskStats> getTaskStats(String id) async {
  return _parse<TaskStats>(() => _client.dio.get("api/v1/tasks/$id/stats"),
      (data) => TaskStats.fromJson(data));
}

Future<void> pauseTask(String id) async {
  return _parse(() => _client.dio.put("api/v1/tasks/$id/pause"), null);
}

Future<void> continueTask(String id) async {
  return _parse(() => _client.dio.put("api/v1/tasks/$id/continue"), null);
}

Future<void> pauseAllTasks(List<String>? ids) async {
  return _parse(
      () => _client.dio.put("api/v1/tasks/pause", queryParameters: {
            if (ids != null) "id": ids,
          }),
      null);
}

Future<void> continueAllTasks(List<String>? ids) async {
  return _parse(
      () => _client.dio.put("api/v1/tasks/continue", queryParameters: {
            if (ids != null) "id": ids,
          }),
      null);
}

Future<void> deleteTask(String id, bool force) async {
  return _parse(
      () => _client.dio.delete("api/v1/tasks/$id?force=$force"), null);
}

Future<void> deleteTasks(List<String>? ids, bool force) async {
  return _parse(
      () => _client.dio.delete("api/v1/tasks", queryParameters: {
            if (ids != null) "id": ids,
            "force": force,
          }),
      null);
}

Future<DownloaderConfig> getConfig() async {
  return _parse(() => _client.dio.get("api/v1/config"),
      (data) => DownloaderConfig.fromJson(data));
}

Future<void> putConfig(DownloaderConfig config) async {
  return _parse(() => _client.dio.put("api/v1/config", data: config), null);
}

Future<String> installExtension(InstallExtension installExtension) async {
  return _parse<String>(
      () => _client.dio.post("api/v1/extensions", data: installExtension),
      (data) => data as String);
}

Future<List<Extension>> getExtensions() async {
  return _parse<List<Extension>>(() => _client.dio.get("api/v1/extensions"),
      (data) => (data as List).map((e) => Extension.fromJson(e)).toList());
}

Future<void> updateExtensionSettings(
    String identity, UpdateExtensionSettings updateExtensionSettings) async {
  return _parse(
      () => _client.dio.put("api/v1/extensions/$identity/settings",
          data: updateExtensionSettings),
      null);
}

Future<void> switchExtension(
    String identity, SwitchExtension switchExtension) async {
  return _parse(
      () => _client.dio
          .put("api/v1/extensions/$identity/switch", data: switchExtension),
      null);
}

Future<void> deleteExtension(String identity) async {
  return _parse(() => _client.dio.delete("api/v1/extensions/$identity"), null);
}

Future<UpdateCheckExtensionResp> upgradeCheckExtension(String identity) async {
  return _parse(() => _client.dio.get("api/v1/extensions/$identity/update"),
      (data) => UpdateCheckExtensionResp.fromJson(data));
}

Future<void> updateExtension(String identity) async {
  return _parse(
      () => _client.dio.post("api/v1/extensions/$identity/update"), null);
}

Future<void> testWebhook(String url) async {
  return _parse(
      () => _client.dio.post("api/v1/webhook/test", data: {"url": url}), null);
}

Future<String> login(LoginReq loginReq) async {
  return _parse(() => _client.dio.post("api/web/login", data: loginReq),
      (data) => data as String);
}

Future<Response<String>> proxyRequest<T>(String uri,
    {data, Options? options}) async {
  options ??= Options();
  options.headers ??= {};
  options.headers!["X-Target-Uri"] = uri;

  // add timestamp to avoid cache
  return _client.dio.request(
      "/api/v1/proxy?t=${DateTime.now().millisecondsSinceEpoch}",
      data: data,
      options: options);
}

String join(String path) {
  final baseUrl = _client.dio.options.baseUrl;
  final cleanBaseUrl = baseUrl.endsWith('/')
      ? baseUrl.substring(0, baseUrl.length - 1)
      : baseUrl;
  return "$cleanBaseUrl/${Util.cleanPath(path)}";
}

/// Generic request method for API proxy
/// Directly forwards requests to gopeed REST API
Future<Response> forward(
  String path, {
  String method = 'GET',
  dynamic data,
  Map<String, dynamic>? queryParameters,
  Duration? receiveTimeout,
  String? operationId,
  bool preserveStatus = false,
}) async {
  return _client.dio.request(
    path,
    data: data,
    queryParameters: queryParameters,
    options: Options(
      method: method,
      receiveTimeout: receiveTimeout,
      headers:
          operationId == null ? null : {'X-Kownload-Operation-Id': operationId},
      followRedirects: preserveStatus ? false : null,
      validateStatus: preserveStatus ? (_) => true : null,
    ),
  );
}

/// Version of the running core, used when a development UI has no release define.
Future<String?> getCoreVersion() async {
  return _parse(() => _client.dio.get('api/v1/info'), (data) {
    if (data is! Map) return null;
    final version = data['version'];
    return version is String ? version : null;
  });
}

// Ordinary/legacy lists make no additional request. A decorated response is
// displayed only after the actual client confirms its logical dataset.
Future<void> _bindCompletionViews(
    ActiveApiSnapshot? snapshot, List<Task> tasks) async {
  if (tasks.every((task) => task.completionView == null) ||
      snapshot?.taskBatchRoute?.supported != true ||
      !snapshot!.isCurrent) {
    return;
  }
  try {
    final response = await scopedTaskBatchRequest(
        snapshot, 'GET', '/api/v1/dataset/identity');
    final body = response.data;
    if (!snapshot.isCurrent ||
        response.statusCode != 200 ||
        body is! Map ||
        body['code'] != 0 ||
        body['data'] is! Map) {
      return;
    }
    final identity =
        DatasetIdentity.fromJson(Map<String, dynamic>.from(body['data']));
    if (!identity.supported) return;
    for (final task in tasks) {
      task.completionView?.bindToDataset(identity.datasetId,
          isCurrent: () => snapshot.isCurrent);
    }
  } catch (_) {
    // Keep the Task list usable, with completion explicitly unconfirmed.
  }
}
