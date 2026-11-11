import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/setting/views/api_endpoint_draft.dart';
import 'package:gopeed/core/common/start_config.dart';
import 'package:gopeed/database/entity.dart';
import 'package:gopeed/util/local_server_config.dart';

void main() {
  test('switching restores valid per-protocol addresses and initializes TCP',
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
    expect(cfg.address, '/tmp/custom-api.sock');
    await editor.select(cfg, 'tcp');
    expect(cfg.address, '127.0.0.2:12345');
    expect(defaults, 0);
  });

  test(
      'TCP to Unix obtains the project default once and restores edited socket',
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
    expect(cfg.address, '/tmp/custom-api.sock');
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
}
