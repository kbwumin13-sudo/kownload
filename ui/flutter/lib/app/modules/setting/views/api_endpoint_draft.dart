import 'dart:convert';
import 'dart:io';

import 'package:path/path.dart' as path;

import '../../../../core/common/start_config.dart';

/// TCP drafts are local to the dialog; Unix always returns to the App IPC path.
class ApiEndpointDraft {
  final Future<String> Function() defaultUnixAddress;
  final _addresses = <String, String>{};
  Future<String>? _managedUnix;
  ApiEndpointDraft({required this.defaultUnixAddress});

  Future<void> select(StartConfig config, String network) async {
    if (network == config.network) return;
    if (config.network == 'tcp' &&
        validationError('tcp', config.address) == null) {
      _addresses['tcp'] = config.address;
    }
    if (network == 'unix') {
      await useManagedUnix(config);
    } else {
      config.network = network;
      config.address = _addresses[network] ?? '127.0.0.1:0';
    }
  }

  Future<void> useManagedUnix(StartConfig config) async {
    final address = await (_managedUnix ??= defaultUnixAddress());
    config.network = 'unix';
    config.address = address;
  }

  /// Read-only checks at Save. No unlink, mkdir, bind or following of links.
  /// This is not a substitute for safe cleanup at the later Core startup.
  static String? unixTargetError(
      {required String address,
      required String expectedAddress,
      required String storageRoot}) {
    if (address != expectedAddress ||
        !path.isAbsolute(storageRoot) ||
        !path.isWithin(storageRoot, expectedAddress)) {
      return '自定义 Unix 地址仅展示、未验证。请选择“使用应用 IPC 路径”或切换 TCP 后保存。';
    }
    try {
      var current = storageRoot;
      final candidates = <String>[current];
      for (final part
          in path.split(path.relative(expectedAddress, from: storageRoot))) {
        current = path.join(current, part);
        candidates.add(current);
      }
      for (final candidate in candidates) {
        final type = FileSystemEntity.typeSync(candidate, followLinks: false);
        if (candidate == expectedAddress) {
          if (type != FileSystemEntityType.notFound &&
              type != FileSystemEntityType.unixDomainSock) {
            return '应用 IPC 路径已有非 socket 对象，未保存。请先核实并保留该文件、目录或链接；本页不会删除它。';
          }
        } else if (type != FileSystemEntityType.directory &&
            type != FileSystemEntityType.notFound) {
          return '应用 IPC 目录不是普通目录或含链接，未保存。请先核实目录；本页不会修改它。';
        }
      }
    } on FileSystemException {
      return '无法确认应用 IPC 路径类型，未保存。请检查目录权限后重试。';
    }
    return null;
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
