import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/setting/views/api_endpoint_draft.dart';
import 'package:gopeed/core/common/start_config.dart';
import 'package:gopeed/database/entity.dart';
import 'package:gopeed/util/local_server_config.dart';

void main() {
  test(
      'switching preserves TCP draft and explicitly returns Unix to managed path',
      () async {
    var defaults = 0;
    final editor = ApiEndpointDraft(defaultUnixAddress: () async {
      defaults++;
      return '/tmp/api-default.sock';
    });
    final cfg = StartConfig()
      ..network = 'unix'
      ..address = '/tmp/custom-api.sock';
    await editor.select(cfg, 'tcp');
    expect(cfg.address, '127.0.0.1:0');
    cfg.address = '127.0.0.2:12345';
    await editor.select(cfg, 'unix');
    expect(cfg.address, '/tmp/api-default.sock');
    await editor.select(cfg, 'tcp');
    expect(cfg.address, '127.0.0.2:12345');
    expect(defaults, 1);
  });

  test(
      'TCP to Unix uses the project default instead of accepting a custom target',
      () async {
    var defaults = 0;
    final editor = ApiEndpointDraft(defaultUnixAddress: () async {
      defaults++;
      return '/tmp/api-default.sock';
    });
    final cfg = StartConfig()
      ..network = 'tcp'
      ..address = '127.0.0.1:9';
    await editor.select(cfg, 'unix');
    expect(cfg.address, '/tmp/api-default.sock');
    cfg.address = '/tmp/custom-api.sock';
    await editor.select(cfg, 'tcp');
    await editor.select(cfg, 'unix');
    expect(cfg.address, '/tmp/api-default.sock');
    expect(defaults, 1);
  });

  test(
      'persistence/reload preserves each protocol pair and frontend uses actual TCP port',
      () async {
    final root = await Directory('/tmp').createTemp('api-endpoint-review-');
    ServerSocket? server;
    try {
      final editor = ApiEndpointDraft(
          defaultUnixAddress: () async => '${root.path}/api.sock');
      final cfg = StartConfig()
        ..network = 'unix'
        ..address = '${root.path}/api.sock'
        ..apiToken = 'fixture';
      for (final network in ['tcp', 'unix']) {
        await editor.select(cfg, network);
        final persisted = StartConfigEntity.fromJson(StartConfigEntity(
                network: cfg.network,
                address: cfg.address,
                apiToken: cfg.apiToken)
            .toJson());
        final reloaded = resolveLocalServerConfig(
            defaults: StartConfigEntity(
                network: 'unix',
                address: '${root.path}/api.sock',
                apiToken: ''),
            saved: persisted,
            legacyUnixAddress: '/tmp/former.sock');
        expect(
            ApiEndpointDraft.validationError(
                reloaded.network, reloaded.address),
            isNull);
        final app = AppController();
        app.startConfig.value.network = reloaded.network;
        app.startConfig.value.address = reloaded.address;
        if (network == 'tcp') {
          server = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
          app.runningPort.value = server.port;
          expect(server.port, greaterThan(0));
          expect(app.runningAddress(), '127.0.0.1:${server.port}');
          expect(app.runningAddress(), isNot('127.0.0.1:0'));
        } else {
          server = await ServerSocket.bind(
              InternetAddress(reloaded.address, type: InternetAddressType.unix),
              0);
          expect(app.runningAddress(), reloaded.address);
        }
        await server.close();
        server = null;
      }
    } finally {
      await server?.close();
      await root.delete(recursive: true);
    }
  });

  test('invalid or mismatched endpoints cannot be persisted by the editor', () {
    for (final address in [
      '/tmp/api.sock',
      '127.0.0.1',
      '127.0.0.1:',
      '127.0.0.1:65536',
      ':80',
      '999.999.999.999:80',
      '127.0.0.1:-1',
      '[::1]:80'
    ]) {
      expect(ApiEndpointDraft.validationError('tcp', address), isNotNull,
          reason: address);
    }
    for (final address in [
      '127.0.0.1:0',
      '',
      '/',
      '/tmp/',
      '/tmp/\u0000.sock',
      '/tmp/${'x' * 104}'
    ]) {
      expect(ApiEndpointDraft.validationError('unix', address), isNotNull,
          reason: address);
    }
    expect(ApiEndpointDraft.validationError('tcp', '127.0.0.1:0'), isNull);
    expect(ApiEndpointDraft.validationError('tcp', 'localhost:8080'), isNull);
    expect(ApiEndpointDraft.validationError('unix', '/tmp/api.sock'), isNull);
  });

  for (final kind in ['file', 'directory', 'link']) {
    test(
        'Unix Save refuses managed target occupied by $kind without touching bytes',
        () async {
      final root = Directory('/tmp').createTempSync('api-unix-guard-');
      try {
        Directory('${root.path}/ipc').createSync();
        final target = '${root.path}/ipc/api.sock';
        final original = File('${root.path}/original.bin')
          ..writeAsBytesSync([0, 1, 127, 255]);
        if (kind == 'file') File(target).writeAsBytesSync([65, 0, 66, 255]);
        if (kind == 'directory') Directory(target).createSync();
        if (kind == 'link') Link(target).createSync(original.path);
        final type = FileSystemEntity.typeSync(target, followLinks: false);
        final content = kind == 'file' ? File(target).readAsBytesSync() : null;
        final error = ApiEndpointDraft.unixTargetError(
            address: target, expectedAddress: target, storageRoot: root.path);
        expect(error, isNotNull);
        expect(FileSystemEntity.typeSync(target, followLinks: false), type);
        if (content != null) expect(File(target).readAsBytesSync(), content);
        if (kind == 'link') expect(Link(target).targetSync(), original.path);
        expect(original.readAsBytesSync(), [0, 1, 127, 255]);
      } finally {
        root.deleteSync(recursive: true);
      }
    });
  }

  test(
      'Unix Save rejects parent links and unmanaged existing files without unlinking',
      () {
    final root = Directory('/tmp').createTempSync('api-unix-parent-');
    try {
      final elsewhere = Directory('${root.path}/elsewhere')..createSync();
      final original = File('${elsewhere.path}/api.sock')
        ..writeAsStringSync('keep existing file');
      Link('${root.path}/ipc').createSync(elsewhere.path);
      final canonical = '${root.path}/ipc/api.sock';
      expect(
          ApiEndpointDraft.unixTargetError(
              address: canonical,
              expectedAddress: canonical,
              storageRoot: root.path),
          isNotNull);
      expect(
          ApiEndpointDraft.unixTargetError(
              address: original.path,
              expectedAddress: canonical,
              storageRoot: root.path),
          isNotNull);
      expect(original.readAsStringSync(), 'keep existing file');
      expect(Link('${root.path}/ipc').targetSync(), elsewhere.path);
    } finally {
      root.deleteSync(recursive: true);
    }
  });

  test(
      'Unix Save allows missing managed target and real managed socket without deleting either',
      () async {
    final root = Directory('/tmp').createTempSync('api-unix-socket-');
    ServerSocket? socket;
    try {
      final target = '${root.path}/ipc/api.sock';
      expect(
          ApiEndpointDraft.unixTargetError(
              address: target, expectedAddress: target, storageRoot: root.path),
          isNull);
      expect(Directory('${root.path}/ipc').existsSync(), isFalse);
      Directory('${root.path}/ipc').createSync();
      socket = await ServerSocket.bind(
          InternetAddress(target, type: InternetAddressType.unix), 0);
      expect(FileSystemEntity.typeSync(target, followLinks: false),
          FileSystemEntityType.unixDomainSock);
      expect(
          ApiEndpointDraft.unixTargetError(
              address: target, expectedAddress: target, storageRoot: root.path),
          isNull);
      expect(FileSystemEntity.typeSync(target, followLinks: false),
          FileSystemEntityType.unixDomainSock);
    } finally {
      await socket?.close();
      root.deleteSync(recursive: true);
    }
  });
}
