import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:path/path.dart' as path;

import '../util/util.dart';
import 'app_identity.dart';

const _channel = MethodChannel('kownload/identity');

/// Runs before Hive.init or the engine. A failed import never starts downloads.
Future<void> bootstrapIdentity() async {
  if (!Util.isMacos()) return;
  final target = Util.getStorageDir();
  const previewSource = String.fromEnvironment('KOWNLOAD_MIGRATION_SOURCE');
  final source = Util.isIsolatedPreview
      ? (previewSource.isEmpty ? '$target.no-legacy-source' : previewSource)
      : path.join(Platform.environment['HOME']!, 'Library',
          'Application Support', AppIdentity.legacyBundleId);
  if (Util.isIsolatedPreview &&
      previewSource.isEmpty &&
      await Directory(source).exists()) {
    throw const IdentityBootstrapException('预览空源目录已存在，请更换预览目录');
  }
  if (!Util.isIsolatedPreview &&
      !await File(path.join(target, '.kownload-identity.json')).exists() &&
      await _channel.invokeMethod<bool>('legacyRunning') == true) {
    throw const IdentityBootstrapException('请退出旧版 kownload 后重新打开，以便导入现有数据');
  }
  final tool = path.normalize(path.join(
      File(Platform.resolvedExecutable).parent.path,
      '..',
      'Helpers',
      AppIdentity.migrationExecutable));
  await runIdentityImport(
      source: source, target: target, run: (args) => Process.run(tool, args));
  if (!Util.isIsolatedPreview) {
    await _channel.invokeMethod<void>('migratePreferences');
  }
}

typedef ImportRunner = Future<ProcessResult> Function(List<String> arguments);

Future<void> runIdentityImport(
    {required String source,
    required String target,
    required ImportRunner run}) async {
  RandomAccessFile? legacyHiveLock;
  try {
    final lock = File(path.join(source, 'database.lock'));
    final imported =
        await File(path.join(target, '.kownload-identity.json')).exists();
    if (!imported && await lock.exists()) {
      legacyHiveLock = await lock.open(mode: FileMode.append);
      await legacyHiveLock.lock(FileLock.exclusive);
    }
    final result = await run(['--source', source, '--target', target]);
    final reply = jsonDecode(result.stdout.toString()) as Map<String, dynamic>;
    if (result.exitCode != 0 || reply['ok'] != true) {
      throw const IdentityBootstrapException(
          '数据导入未完成：旧库可能被占用，或目标已有未确认数据。请退出旧版后重试；原数据未改动');
    }
  } on IdentityBootstrapException {
    rethrow;
  } catch (_) {
    throw const IdentityBootstrapException('无法取得旧库锁或运行导入工具。请退出旧版后重试');
  } finally {
    if (legacyHiveLock != null) {
      try {
        await legacyHiveLock.unlock();
      } finally {
        await legacyHiveLock.close();
      }
    }
  }
}

class IdentityBootstrapException implements Exception {
  final String message;
  const IdentityBootstrapException(this.message);
  @override
  String toString() => message;
}
