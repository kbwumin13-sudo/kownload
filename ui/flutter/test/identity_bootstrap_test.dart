import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/bootstrap.dart';

void main() {
  test('occupied Hive OS lock rejects import before tool invocation', () async {
    final root = await Directory.systemTemp.createTemp('hive-lock-fixture-');
    final file = File('${root.path}/database.lock');
    await file.writeAsString('');
    final holder = await Process.start('/usr/bin/python3', [
      '-u',
      '-c',
      'import sys,fcntl; f=open(sys.argv[1],"r+b"); fcntl.lockf(f,fcntl.LOCK_EX|fcntl.LOCK_NB); print("ready",flush=True); sys.stdin.read()',
      file.path
    ]);
    try {
      await holder.stdout.transform(utf8.decoder).first;
      var called = false;
      await expectLater(
          runIdentityImport(
              source: root.path,
              target: '${root.path}/target',
              run: (_) async {
                called = true;
                return ProcessResult(1, 0, '{"ok":true}', '');
              }),
          throwsA(isA<IdentityBootstrapException>()));
      expect(called, false);
      await Directory('${root.path}/target').create();
      await File('${root.path}/target/.kownload-identity.json')
          .writeAsString('{"version":1}');
      await runIdentityImport(
          source: root.path,
          target: '${root.path}/target',
          run: (_) async {
            called = true;
            return ProcessResult(1, 0, '{"ok":true}', '');
          });
      expect(called, true,
          reason: 'completed imports must not depend on the old lock');
    } finally {
      await holder.stdin.close();
      await holder.exitCode;
      await root.delete(recursive: true);
    }
  });
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
  test('legacy use after the import stops with commands to keep either side',
      () async {
    try {
      await runIdentityImport(
          source: '/fixture/legacy root',
          target: '/fixture/new root',
          run: (_) async => ProcessResult(
              1, 1, '{"ok":false,"error":"legacy_changed_after_import"}', ''));
      fail('bootstrap should stop');
    } on IdentityBootstrapException catch (e) {
      expect(e.message, contains('没有继续任何任务'));
      expect(
          e.message,
          contains(
              'mv "/fixture/new root" "/fixture/new root.kept-\$(date +%Y%m%d-%H%M%S)"'));
      expect(
          e.message,
          contains(
              'mv "/fixture/legacy root" "/fixture/legacy root.kept-\$(date +%Y%m%d-%H%M%S)"'));
      expect(e.message, contains('关闭“打开应用时继续未完成的下载”'));
      expect(e.ignoreLegacyChanges, isNotNull);
    }
  });
  test('keeping the new data for good runs the importer with the choice',
      () async {
    final calls = <List<String>>[];
    var reply = '{"ok":true}';
    Future<ProcessResult> run(List<String> args) async {
      calls.add(args);
      return ProcessResult(1, reply.contains('true') ? 0 : 1, reply, '');
    }

    expect(await ignoreLegacyChanges(source: '/old', target: '/new', run: run),
        true);
    expect(calls.single,
        ['--source', '/old', '--target', '/new', '--ignore-legacy-changes']);
    reply = '{"ok":false,"error":"invalid_marker"}';
    expect(await ignoreLegacyChanges(source: '/old', target: '/new', run: run),
        false);
    expect(
        await ignoreLegacyChanges(
            source: '/old',
            target: '/new',
            run: (_) async => throw const FileSystemException('missing tool')),
        false);
  });
}
