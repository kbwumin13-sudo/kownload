import '../database/entity.dart';

/// Upgrade the former cache socket without changing custom server endpoints.
StartConfigEntity resolveLocalServerConfig({
  required StartConfigEntity defaults,
  required StartConfigEntity? saved,
  required String legacyUnixAddress,
}) {
  final network = saved?.network ?? defaults.network;
  final address = saved?.address ?? defaults.address;
  return StartConfigEntity(
    network: network,
    address: network == 'unix' && address == legacyUnixAddress
        ? defaults.address
        : address,
    apiToken: saved?.apiToken ?? defaults.apiToken,
  );
}
