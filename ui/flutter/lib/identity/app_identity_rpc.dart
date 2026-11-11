import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:path/path.dart' as path;

import '../app/rpc/rpc.dart';
import 'app_identity.dart';

/// Same canonical socket-path fingerprint as the Native host. This is a route
/// consistency hint, not a secret or proof of the publisher's identity.
Future<String> nativeRouteScope(String socket) async {
  if (!path.isAbsolute(socket)) {
    throw const FormatException('identity_unavailable');
  }
  var candidate = path.normalize(socket);
  final suffix = <String>[];
  while (true) {
    try {
      var resolved = await File(candidate).resolveSymbolicLinks();
      for (final part in suffix.reversed) {
        resolved = path.join(resolved, part);
      }
      return sha256.convert(utf8.encode(resolved)).toString();
    } on FileSystemException catch (error) {
      if (error.osError?.errorCode != 2 ||
          path.dirname(candidate) == candidate) {
        throw const FormatException('identity_unavailable');
      }
      suffix.add(path.basename(candidate));
      candidate = path.dirname(candidate);
    }
  }
}

/// Stateless, read-only RPC. It never registers a browser client, writes
/// preferences, opens UI, wakes core or probes a user-configured network API.
class AppIdentityRpc {
  final String socket;
  final bool preview;
  final String sourceCommit;
  final String releaseVersion;

  const AppIdentityRpc({
    required this.socket,
    required this.preview,
    this.sourceCommit = const String.fromEnvironment('KOWNLOAD_SOURCE_COMMIT',
        defaultValue: 'unknown'),
    this.releaseVersion = const String.fromEnvironment(
        'KOWNLOAD_RELEASE_VERSION',
        defaultValue: 'unknown'),
  });

  Future<Map<String, dynamic>> command(Map<String, dynamic> request) async {
    final params = request['params'];
    if (request['method'] != 'hostInfo' ||
        params is! Map ||
        params['version'] is! int ||
        params['version'] != 1) {
      throw const FormatException('invalid_request');
    }
    return {
      'version': 1,
      'product': AppIdentity.productName,
      'identityProtocol': 1,
      'build': {
        'sourceCommit': RegExp(r'^[0-9a-f]{40}$').hasMatch(sourceCommit)
            ? sourceCommit
            : 'unknown',
        'releaseVersion': RegExp(r'^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$')
                .hasMatch(releaseVersion)
            ? releaseVersion
            : 'unknown',
        'preview': preview,
      },
      'routeScope': await nativeRouteScope(socket),
    };
  }

  Future<void> handle(RpcContext ctx) async {
    try {
      if (ctx.request.method != 'POST') {
        throw const FormatException('invalid_request');
      }
      final data = await command(await ctx.readJSON());
      await ctx.writeJSON({'code': 0, 'data': data, 'msg': ''});
    } catch (_) {
      await ctx.writeJSON({
        'code': 1000,
        'data': null,
        'msg': '无法读取应用身份',
        'error': 'identity_unavailable',
      }, statusCode: 400);
    }
  }
}
