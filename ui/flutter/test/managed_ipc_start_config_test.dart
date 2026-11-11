import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/identity/managed_ipc_directory.dart';
import 'package:gopeed/util/util.dart';
import 'package:path/path.dart' as path;

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test(
    'failed default IPC preparation never caches a usable launch config',
    () async {
      final root = await Directory.systemTemp.createTemp('k14-ipc-cfg-');
      try {
        await Util.initStorageDir(previewStorage: root.path);
        final blocker = await File(
          path.join(root.path, 'ipc'),
        ).writeAsString('kept, never a socket directory');
        final app = AppController();
        final original = app.startConfig.value;
        original.startupTaskPolicy = 'imported-review';
        original.network = 'unix';
        original.address = path.join(root.path, 'custom.sock');
        for (var attempt = 0; attempt < 2; attempt++) {
          await expectLater(
            app.loadStartConfig(),
            throwsA(isA<ManagedIpcDirectoryException>()),
          );
          expect(identical(app.startConfig.value, original), true);
          expect(original.startupTaskPolicy, 'imported-review');
          expect(original.network, 'unix');
          expect(original.address, path.join(root.path, 'custom.sock'));
          expect(
            await blocker.readAsString(),
            'kept, never a socket directory',
          );
        }
      } finally {
        await root.delete(recursive: true);
      }
    },
  );
}
