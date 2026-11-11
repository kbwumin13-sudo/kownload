import 'dart:convert';
import 'dart:io';

import 'package:path/path.dart' as path;

import '../../../../core/common/start_config.dart';

/// Per-dialog addresses only; switching protocols never persists a second map.
class ApiEndpointDraft {
  final Future<String> Function() defaultUnixAddress;
  final _addresses = <String, String>{};
  ApiEndpointDraft({required this.defaultUnixAddress});

  Future<void> select(StartConfig config, String network) async {
    if (network == config.network) return;
    if (validationError(config.network, config.address) == null) {
      _addresses[config.network] = config.address;
    }
    final address = _addresses[network] ??
        (network == 'tcp' ? '127.0.0.1:0' : await defaultUnixAddress());
    config.network = network;
    config.address = address;
  }

  static (String, String)? tcpFields(String address) {
    final match = RegExp(r'^([^:\s/\\]+):([0-9]+)$').firstMatch(address);
    return match == null ? null : (match[1]!, match[2]!);
  }

  static String tcpAddress(String host, String port) => '$host:$port';

  /// Syntax only: does not bind a socket, resolve DNS or restart the core.
  static String? validationError(String network, String address) {
    if (network == 'unix') {
      if (!path.isAbsolute(address) ||
          address == '/' ||
          address.endsWith('/') ||
          address.contains('\u0000') ||
          utf8.encode(address).length >= 104) {
        return 'Unix 地址须为完整的 socket 文件路径，且 UTF-8 长度小于 104 字节。';
      }
      return null;
    }
    if (network != 'tcp') return '请选择 TCP 或 Unix 协议。';
    final fields = tcpFields(address);
    final port = fields == null ? null : int.tryParse(fields.$2);
    if (fields == null || port == null || port > 65535) {
      return 'TCP 地址须包含有效主机与 0–65535 端口。';
    }
    final host = fields.$1;
    final ip = InternetAddress.tryParse(host);
    if (ip == null &&
        (RegExp(r'^[0-9.]+$').hasMatch(host) ||
            !RegExp(r'^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$')
                .hasMatch(host))) {
      return 'TCP 主机地址无效。';
    }
    return null;
  }
}
