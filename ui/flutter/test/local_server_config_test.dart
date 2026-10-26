import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/database/entity.dart';
import 'package:gopeed/util/local_server_config.dart';

void main() {
  const legacy = '/user/Library/Caches/gopeed.sock';
  final defaults = StartConfigEntity(
      network: 'unix',
      address:
          '/user/Library/Application Support/com.gopeed.gopeed/gopeed.sock',
      apiToken: '');
  test('new local server uses the persistent application directory', () {
    final config = resolveLocalServerConfig(
        defaults: defaults, saved: null, legacyUnixAddress: legacy);
    expect(config.address, defaults.address);
    expect(config.network, 'unix');
  });
  test('saved cache socket moves without losing authentication', () {
    final config = resolveLocalServerConfig(
        defaults: defaults,
        saved: StartConfigEntity(
            network: 'unix', address: legacy, apiToken: 'fixture'),
        legacyUnixAddress: legacy);
    expect(config.address, defaults.address);
    expect(config.apiToken, 'fixture');
  });
  test('custom Unix and remote TCP endpoints are preserved', () {
    for (final saved in [
      StartConfigEntity(
          network: 'unix', address: '/custom/core.sock', apiToken: 'fixture'),
      StartConfigEntity(
          network: 'tcp', address: '127.0.0.1:9988', apiToken: 'fixture'),
    ]) {
      expect(
          resolveLocalServerConfig(
                  defaults: defaults, saved: saved, legacyUnixAddress: legacy)
              .toJson(),
          saved.toJson());
    }
  });
}
