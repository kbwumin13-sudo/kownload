import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/bootstrap.dart';

void main() {
  test('bootstrap calls importer before opening stores and passes exact roots',
      () async {
    final dir = await Directory.systemTemp.createTemp('identity-fixture-');
    try {
      var calls = 0;
      await runIdentityImport(
          source: '${dir.path}/old',
          target: '${dir.path}/new',
          run: (args) async {
            calls++;
            expect(args,
                ['--source', '${dir.path}/old', '--target', '${dir.path}/new']);
            expect(await File('${dir.path}/new/database.hive').exists(), false);
            return ProcessResult(1, 0, jsonEncode({'ok': true}), '');
          });
      expect(calls, 1);
    } finally {
      await dir.delete(recursive: true);
    }
  });
  test(
      'failure and malformed result stop initialization without exposing values',
      () async {
    for (final reply in [
      ProcessResult(1, 1, '{"ok":false,"error":"private"}', ''),
      ProcessResult(1, 0, 'private', '')
    ]) {
      try {
        await runIdentityImport(
            source: '/nonexistent-fixture',
            target: '/nonexistent-target',
            run: (_) async => reply);
        fail('bootstrap should fail');
      } on IdentityBootstrapException catch (e) {
        expect(e.message, isNot(contains('private')));
      }
    }
  });
}
