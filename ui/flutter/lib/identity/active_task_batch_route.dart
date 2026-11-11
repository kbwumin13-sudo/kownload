import 'dart:io';

import '../database/task_batch_journal.dart';
import 'app_identity.dart';
import 'app_identity_rpc.dart';

/// Called after this App's core boot succeeds, then supplied to Api.init with
/// that boot's actual endpoint. This is the existing App route reference, not
/// DB identity, authentication or evidence that a remote endpoint is this App.
Future<JournalRouteRef?> localBootTaskBatchRoute(String hostSocket) async {
  try {
    return JournalRouteRef(
      product: AppIdentity.productName,
      identityProtocol: 1,
      routeScope: await nativeRouteScope(hostSocket),
    );
  } on FormatException {
    return null;
  } on FileSystemException {
    return null;
  }
}
