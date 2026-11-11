import 'dart:convert';
import 'dart:io';

import 'package:dart_ipc/dart_ipc.dart';

import '../../util/util.dart';
import '../../identity/app_identity.dart';
import '../../identity/managed_ipc_directory.dart';

/// RPC context class that contains request information and response methods
class RpcContext {
  final HttpRequest request;
  final HttpResponse response;
  String? _bodyCache;

  RpcContext(this.request, this.response);

  /// Read and parse JSON request body
  Future<Map<String, dynamic>> readJSON() async {
    _bodyCache ??=
        await request.cast<List<int>>().transform(utf8.decoder).join();

    if (_bodyCache!.isEmpty) {
      return {};
    }

    return jsonDecode(_bodyCache!) as Map<String, dynamic>;
  }

  Future<String> readText() async {
    _bodyCache ??=
        await request.cast<List<int>>().transform(utf8.decoder).join();

    return _bodyCache ?? '';
  }

  /// Write JSON response
  Future<void> writeJSON(Map<String, dynamic> data, {int? statusCode}) async {
    if (statusCode != null) response.statusCode = statusCode;
    response.headers.contentType = ContentType.json;
    response.write(jsonEncode(data));
    await response.close();
  }

  /// Write error response
  Future<void> writeError(String message, [int statusCode = 500]) async {
    response.statusCode = statusCode;
    await writeJSON({
      'code': 1000,
      'msg': message,
      'data': null,
      'error': 'request_failed',
    });
  }
}

/// Route handler type definition
typedef RouteHandler = Future<void> Function(RpcContext ctx);

/// Route registry
class RouteRegistry {
  final Map<String, RouteHandler> _routes = {};

  /// Register a route
  void register(String path, RouteHandler handler) {
    _routes[path] = handler;
  }

  /// Get route handler
  RouteHandler? getHandler(String path) {
    return _routes[path];
  }

  /// Get all registered routes
  List<String> get routes => _routes.keys.toList();
}

/// Start RPC server
Future<void> startRpcServer([Map<String, RouteHandler>? routes]) async {
  // Core initialization can fail while the UI still reaches onReady. Enforce
  // the same managed-directory prerequisite here, before mkdir/unlink/bind.
  if (Util.isMacos()) {
    await ensureManagedIpcDirectory(storageRoot: Util.getStorageDir());
  }
  String path;
  if (Util.isWindows()) {
    path = r'\\.\pipe\gopeed_host';
  } else {
    path = await Util.homePathJoin(
        Util.isMacos() ? AppIdentity.hostSocket : "gopeed_host.sock");
    if (!Util.isMacos()) {
      await File(path).parent.create(recursive: true);
    }
    if (Util.isMacos() && utf8.encode(path).length >= 104) {
      throw StateError('IPC 路径过长，请使用更短的数据目录');
    }
    // try to delete existing socket file
    final socketFile = File(path);
    if (await socketFile.exists()) {
      try {
        await socketFile.delete();
      } catch (e) {
        // ignore
      }
    }
  }

  // Create route registry
  final registry = RouteRegistry();

  // Register provided routes
  if (routes != null) {
    routes.forEach((path, handler) {
      registry.register(path, handler);
    });
  }

  final serverSocket = await bind(path);
  final httpServer = HttpServer.listenOn(serverSocket);

  httpServer.forEach((HttpRequest request) async {
    final ctx = RpcContext(request, request.response);

    try {
      // Get request path
      final requestPath = request.uri.path;
      // Find route handler
      final handler = registry.getHandler(requestPath);
      if (handler != null) {
        // Execute route handler
        await handler(ctx);
      } else {
        // 404 Route not found
        await ctx.writeError('请求未完成（invalid_request）', 404);
      }
    } catch (e) {
      await ctx.writeError('请求未完成（request_failed）', 500);
    } finally {
      await request.response.close();
    }
  });
}
