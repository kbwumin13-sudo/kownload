import 'package:dio/dio.dart' hide Options;

import 'api.dart' as api;
import 'model/options.dart';
import 'model/request.dart';
import 'model/task.dart';

/// Public page hosts only. APIHosts are never used to classify pasted links.
const netdiskProviders = <String, String>{
  'baidu': '百度网盘',
  'aliyun': '阿里云盘',
  'mcloud': '中国移动云盘',
  'tcloud': '天翼云盘',
  'xunlei': '迅雷云盘',
  'quark': '夸克网盘',
  'uc': 'UC 网盘',
  '123pan': '123 云盘',
  'guangya': '光鸭云盘',
};

const _pageHosts = <String, List<String>>{
  'baidu': ['pan.baidu.com', 'yun.baidu.com'],
  'aliyun': ['aliyundrive.com', 'alipan.com'],
  'mcloud': ['yun.139.com', 'caiyun.139.com'],
  'tcloud': ['cloud.189.cn'],
  'xunlei': ['pan.xunlei.com'],
  'quark': ['pan.quark.cn'],
  'uc': ['drive.uc.cn'],
  '123pan': [
    '123pan.com',
    '123pan.cn',
    '123684.com',
    '123865.com',
    '123952.com',
    '123912.com',
  ],
  'guangya': ['guangyapan.com'],
};

String? netdiskProviderForUrl(String value) {
  final uri = Uri.tryParse(value);
  if (uri == null ||
      !['http', 'https'].contains(uri.scheme.toLowerCase()) ||
      uri.userInfo.isNotEmpty) {
    return null;
  }
  final host = uri.host.toLowerCase();
  for (final entry in _pageHosts.entries) {
    if (entry.value.any((h) =>
        host == h ||
        (entry.key == 'aliyun' || entry.key == 'guangya'
            ? host == 'www.$h'
            : host.endsWith('.$h')))) {
      return entry.key;
    }
  }
  return null;
}

class NetdiskSource {
  final String provider;
  final String url;
  final String shareId;
  final String password;
  final String parentId;
  const NetdiskSource(
      {required this.provider,
      required this.url,
      this.shareId = '',
      this.password = '',
      this.parentId = ''});

  factory NetdiskSource.fromJson(Map<String, dynamic> json) => NetdiskSource(
        provider: json['provider'] as String? ?? '',
        url: json['url'] as String? ?? '',
        shareId: json['shareId'] as String? ?? '',
        password: json['password'] as String? ?? '',
        parentId: json['parentId'] as String? ?? '',
      );

  NetdiskSource inDirectory(String id) => NetdiskSource(
        provider: provider,
        url: url,
        shareId: shareId,
        password: password,
        parentId: id,
      );

  Map<String, dynamic> toJson() => {
        'provider': provider,
        'url': url,
        if (shareId.isNotEmpty) 'shareId': shareId,
        if (password.isNotEmpty) 'password': password,
        if (parentId.isNotEmpty) 'parentId': parentId,
      };
}

class NetdiskFile {
  final String id;
  final String name;
  final String path;
  final int size;
  final bool isDir;
  final String hash;
  final Map<String, String> meta;
  const NetdiskFile(
      {required this.id,
      required this.name,
      this.path = '',
      this.size = 0,
      this.isDir = false,
      this.hash = '',
      this.meta = const {}});
  String get key => '$id:$path';
  factory NetdiskFile.fromJson(Map<String, dynamic> json) => NetdiskFile(
        id: json['id'] as String,
        name: json['name'] as String,
        path: json['path'] as String? ?? '',
        size: (json['size'] as num?)?.toInt() ?? 0,
        isDir: json['isDir'] as bool? ?? false,
        hash: json['hash'] as String? ?? '',
        meta: Map<String, String>.from(json['meta'] as Map? ?? {}),
      );
  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'path': path,
        'size': size,
        'isDir': isDir,
        if (hash.isNotEmpty) 'hash': hash,
        if (meta.isNotEmpty) 'meta': meta,
      };
}

class NetdiskInput {
  final NetdiskSource source;
  final String sessionId;
  final List<NetdiskFile> files;
  const NetdiskInput(this.source, {this.sessionId = '', this.files = const []});

  /// Native create requests use the same confirmation flow as pasted links.
  factory NetdiskInput.fromRequest(Request request) {
    final extra = Map<String, dynamic>.from(request.extra as Map? ?? {});
    final raw = Map<String, dynamic>.from(extra['source'] as Map? ?? {});
    raw['url'] ??= request.url;
    final provider = netdiskProviderForUrl(raw['url'] as String);
    if (provider == null ||
        (extra['provider'] != null && extra['provider'] != provider) ||
        (raw['provider'] != null && raw['provider'] != provider)) {
      throw const NetdiskException('网盘类型与来源页面不一致', 'invalid_source');
    }
    raw['provider'] = provider;
    final files = extra['files'] as List? ??
        (extra['file'] == null ? [] : [extra['file']]);
    return NetdiskInput(
      NetdiskSource.fromJson(raw),
      sessionId: extra['sessionId'] as String? ?? '',
      files: files
          .map((f) => NetdiskFile.fromJson(Map<String, dynamic>.from(f as Map)))
          .toList(),
    );
  }
}

/// Extract URLs from share prose; each code belongs to its adjacent URL.
/// This function has no persistence or authentication side effects.
List<NetdiskInput> parseNetdiskInputs(String text) {
  final matches =
      RegExp(r'''https?://[^\s<>"'，。；、（）【】]+''', caseSensitive: false)
          .allMatches(text)
          .toList();
  final result = <NetdiskInput>[];
  final seen = <String>{};
  for (var i = 0; i < matches.length; i++) {
    final match = matches[i];
    final url = match.group(0)!.replaceFirst(RegExp(r'[),;.!?\]}]+$'), '');
    final provider = netdiskProviderForUrl(url);
    if (provider == null) continue;
    final uri = Uri.parse(url);
    final parts = uri.pathSegments.where((p) => p.isNotEmpty).toList();
    var shareId = '';
    for (var p = 0; p + 1 < parts.length; p++) {
      if (['s', 'share', 't'].contains(parts[p])) {
        shareId = parts[p + 1];
        break;
      }
    }
    final fragment = uri.hasFragment ? uri.toString().split('#').last : '';
    final fragmentUri = Uri.tryParse(fragment);
    if (provider == 'baidu' && parts.join('/') == 'share/init') {
      final surl = uri.queryParameters['surl'] ?? '';
      shareId = surl.isEmpty ? '' : '1$surl';
    }
    if (provider == 'mcloud' &&
        parts.where((p) => p.isNotEmpty).join('/') == 'shareweb') {
      final route = fragmentUri?.pathSegments ?? <String>[];
      if (route.length >= 3 && route[0] == 'w' && route[1] == 'i') {
        shareId = route[2];
      }
    }
    for (final key in ['shareId', 'shareid', 'linkID', 'code']) {
      if (shareId.isEmpty) shareId = uri.queryParameters[key] ?? '';
    }
    var password = '';
    for (final key in ['pwd', 'password', 'pass_code', 'receive_code']) {
      if (password.isEmpty) password = uri.queryParameters[key] ?? '';
    }
    if (password.isEmpty && fragment.startsWith('pwd=')) {
      password = Uri.splitQueryString(fragment)['pwd'] ?? '';
    }
    final prose = text.substring(
        match.end, i + 1 < matches.length ? matches[i + 1].start : text.length);
    final codePattern = RegExp(
        r'(?:提取码|访问码|密码|口令|验证码)\s*[:：=]?\s*([A-Za-z0-9]{4,8})(?![A-Za-z0-9])');
    final code = codePattern.firstMatch(prose) ??
        (i == 0
            ? codePattern.firstMatch(text.substring(0, match.start))
            : null);
    if (password.isEmpty) password = code?.group(1) ?? '';
    if (!seen.add('$provider:$url:$password')) continue;
    result.add(NetdiskInput(NetdiskSource(
        provider: provider, url: url, shareId: shareId, password: password)));
  }
  return result;
}

/// The ordinary part of a mixed submission, with share prose removed.
List<String> ordinaryNetdiskCompanionLinks(String text) {
  final result = <String>[];
  for (final line in text.split(RegExp(r'[\r\n]+'))) {
    final matches =
        RegExp(r'''https?://[^\s<>"'，。；、（）【】]+''', caseSensitive: false)
            .allMatches(line)
            .toList();
    if (matches.isNotEmpty) {
      for (final match in matches) {
        final url = match.group(0)!.replaceFirst(RegExp(r'[),;.!?\]}]+$'), '');
        if (netdiskProviderForUrl(url) == null) result.add(url);
      }
    } else {
      final value = line.trim();
      if (value.toLowerCase().startsWith('magnet:') ||
          value.toLowerCase().startsWith('ed2k:') ||
          value.toLowerCase().endsWith('.torrent')) {
        result.add(value);
      }
    }
  }
  return result;
}

class NetdiskProviderInfo {
  final String id;
  final String name;
  final List<String> hosts;
  final List<String> apiHosts;
  NetdiskProviderInfo.fromJson(Map<String, dynamic> json)
      : id = json['id'] as String,
        name = json['name'] as String,
        hosts = List<String>.from(json['hosts'] as List? ?? []),
        apiHosts = List<String>.from(json['apiHosts'] as List? ?? []);
}

class NetdiskListing {
  final NetdiskSource source;
  final String sessionId;
  final List<NetdiskFile> files;
  final String nextPage;
  final String title;
  NetdiskListing.fromJson(Map<String, dynamic> json)
      : source =
            NetdiskSource.fromJson(Map<String, dynamic>.from(json['source'])),
        sessionId = json['sessionId'] as String? ?? '',
        files = (json['files'] as List? ?? [])
            .map((f) => NetdiskFile.fromJson(Map<String, dynamic>.from(f)))
            .toList(),
        nextPage = json['nextPage'] as String? ?? '',
        title = json['title'] as String? ?? '';
}

class NetdiskBrowserRequest {
  final String id;
  final NetdiskSource source;
  final String state;
  final String sessionId;
  final String message;
  final String instanceId;
  NetdiskBrowserRequest.fromJson(Map<String, dynamic> json)
      : id = json['id'] as String,
        source =
            NetdiskSource.fromJson(Map<String, dynamic>.from(json['source'])),
        state = json['state'] as String,
        sessionId = json['sessionId'] as String? ?? '',
        message = json['message'] as String? ?? '',
        instanceId = json['instanceId'] as String? ?? '';
}

class NetdiskPrepared {
  final String batchId;
  final List<Map<String, dynamic>> reqs;
  NetdiskPrepared.fromJson(Map<String, dynamic> json)
      : batchId = json['batchId'] as String,
        reqs = (json['reqs'] as List)
            .map((r) => Map<String, dynamic>.from(r))
            .toList();
}

class NetdiskException implements Exception {
  final String message;
  final String reasonCode;
  final dynamic data;
  const NetdiskException(this.message, this.reasonCode, [this.data]);
  @override
  String toString() => message;
}

typedef NetdiskForward = Future<Response> Function(String path,
    {required String method,
    dynamic data,
    Map<String, dynamic>? queryParameters});

class NetdiskApi {
  final NetdiskForward _forward;
  NetdiskApi({NetdiskForward? forward})
      : _forward = forward ??
            ((path, {required method, data, queryParameters}) => api.forward(
                path,
                method: method,
                data: data,
                queryParameters: queryParameters,
                // Allow the backend's 300-second preparation limit to finish.
                receiveTimeout: path == '/api/v1/netdisk/prepare'
                    ? const Duration(minutes: 5, seconds: 10)
                    : null));

  Future<dynamic> _call(String path,
      {String method = 'GET', dynamic data}) async {
    dynamic envelope;
    Response? response;
    try {
      response = await _forward('/api/v1$path', method: method, data: data);
    } on DioException catch (e) {
      response = e.response;
      if (response == null) {
        throw switch (e.type) {
          DioExceptionType.connectionTimeout ||
          DioExceptionType.connectionError =>
            const NetdiskException('无法连接下载服务，请稍后重试', 'connection_error'),
          DioExceptionType.receiveTimeout => const NetdiskException(
              '等待下载服务响应超时，网盘解析尚未完成，请稍后重试', 'receive_timeout'),
          DioExceptionType.sendTimeout => const NetdiskException(
              '向下载服务发送请求超时，网盘解析尚未完成，请稍后重试', 'send_timeout'),
          DioExceptionType.cancel =>
            const NetdiskException('网盘请求已取消', 'cancelled'),
          DioExceptionType.badCertificate => const NetdiskException(
              '下载服务连接验证失败，请检查服务配置后重试', 'certificate_error'),
          DioExceptionType.badResponse =>
            const NetdiskException('下载服务返回了无效响应', 'invalid_response'),
          DioExceptionType.unknown =>
            const NetdiskException('下载服务请求编码或响应解析失败，请重试', 'request_error'),
        };
      }
    }
    final status = response.statusCode;
    if (path == '/netdisk/providers' && [404, 405, 501].contains(status)) {
      throw const NetdiskException(
          '当前下载服务不支持网盘功能，请更新下载服务后重试。', 'unsupported_service');
    }
    envelope = response.data;
    if (envelope is! Map || envelope['code'] is! num) {
      if (status != null && status >= 400) {
        throw NetdiskException(
            '下载服务请求失败（HTTP $status），请检查服务状态后重试。', 'http_error');
      }
      throw const NetdiskException('下载服务返回了无效响应', 'invalid_response');
    }
    if (envelope['code'] != 0) {
      final detail = envelope['data'];
      throw NetdiskException(
          envelope['msg'] as String? ?? '网盘请求失败',
          detail is Map
              ? detail['reasonCode'] as String? ?? 'request_failed'
              : 'request_failed',
          detail);
    }
    return envelope['data'];
  }

  Future<List<NetdiskProviderInfo>> providers() async =>
      (await _call('/netdisk/providers') as List)
          .map(
              (p) => NetdiskProviderInfo.fromJson(Map<String, dynamic>.from(p)))
          .toList();

  Future<Task> getTask(String id) async =>
      Task.fromJson(Map<String, dynamic>.from(
          await _call('/tasks/${Uri.encodeComponent(id)}')));

  Future<void> continueTask(String id) async {
    await _call('/tasks/${Uri.encodeComponent(id)}/continue', method: 'PUT');
  }

  Future<NetdiskBrowserRequest> requestBrowser(NetdiskSource source,
          {String? instanceId}) async =>
      NetdiskBrowserRequest.fromJson(
        Map<String, dynamic>.from(
            await _call('/netdisk/browser/request', method: 'POST', data: {
          'source': source.toJson(),
          if (instanceId?.isNotEmpty ?? false) 'instanceId': instanceId
        })),
      );

  Future<NetdiskBrowserRequest> browserStatus(String id) async =>
      NetdiskBrowserRequest.fromJson(Map<String, dynamic>.from(
          await _call('/netdisk/browser/requests/${Uri.encodeComponent(id)}')));
  Future<void> cancelBrowser(String id) async {
    await _call('/netdisk/browser/requests/${Uri.encodeComponent(id)}',
        method: 'DELETE');
  }

  Future<NetdiskListing> list(NetdiskSource source, String sessionId,
          {String cursor = '', RequestProxy? proxy}) async =>
      NetdiskListing.fromJson(Map<String, dynamic>.from(
          await _call('/netdisk/list', method: 'POST', data: {
        'source': source.toJson(),
        'sessionId': sessionId,
        if (cursor.isNotEmpty) 'cursor': cursor,
        if (proxy != null) 'proxy': proxy.toJson()
      })));

  Future<NetdiskPrepared> prepare(NetdiskSource source, String sessionId,
          List<NetdiskFile> files, Options opts, {RequestProxy? proxy}) async =>
      NetdiskPrepared.fromJson(Map<String, dynamic>.from(
          await _call('/netdisk/prepare', method: 'POST', data: {
        'source': source.toJson(),
        'sessionId': sessionId,
        'files': files.map((f) => f.toJson()).toList(),
        'opts': opts.toJson(),
        if (proxy != null) 'proxy': proxy.toJson()
      })));

  Future<List<String>> createBatch(NetdiskPrepared prepared) async {
    final data = await _call('/tasks/batch',
        method: 'POST', data: {'reqs': prepared.reqs});
    if (data is! List ||
        data.any((id) => id is! String || id.isEmpty) ||
        data.length != prepared.reqs.length) {
      throw const NetdiskException('批次返回的任务数量不完整，请检查任务列表', 'partial_batch');
    }
    return List<String>.from(data);
  }

  Future<List<Map<String, dynamic>>> transfers() async =>
      (await _call('/netdisk/transfers') as List)
          .map((e) => Map<String, dynamic>.from(e))
          .toList();

  Future<void> cleanup(String batchId) async {
    await _call('/netdisk/cleanup', method: 'POST', data: {'batchId': batchId});
  }
}
