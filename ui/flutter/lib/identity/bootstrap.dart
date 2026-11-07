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
    if (reply['ok'] != true && reply['error'] == 'legacy_changed_after_import') {
      throw IdentityBootstrapException(
          legacyChangedMessage(source: source, target: target));
    }
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

/// The old version used its data directory after the import (for example
/// after a rollback). Nothing is opened, resumed, moved or deleted; the user
/// keeps one side by renaming the other directory.
String legacyChangedMessage({required String source, required String target}) {
  const stamp = r'$(date +%Y%m%d-%H%M%S)';
  return '旧版 kownload 在迁移之后又使用过原来的数据目录，两边的下载记录已经不同。'
      '为避免沿用过期的记录，本次没有打开下载数据，也没有继续任何任务；两个目录都没有改动。\n\n'
      '请先退出所有版本的 kownload，在“终端”里执行下面任意一条命令，再重新打开本应用。\n\n'
      '以旧版现在的数据为准（回退后又在旧版里下载过，选这一条）：\n'
      'mv "$target" "$target.kept-$stamp"\n'
      '重新打开后会从旧目录重新导入。\n\n'
      '继续使用本应用迁移后的数据：\n'
      'mv "$source" "$source.kept-$stamp"\n\n'
      '两条命令都只给目录改名，改名后的目录可以随时改回原名。';
}

class IdentityBootstrapException implements Exception {
  final String message;
  const IdentityBootstrapException(this.message);
  @override
  String toString() => message;
}
