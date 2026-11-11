import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:path/path.dart' as path;

import '../util/util.dart';
import 'app_identity.dart';
import 'startup_policy.dart';

const _channel = MethodChannel('kownload/identity');

/// Runs before Hive.init or the engine. A failed import never starts downloads.
Future<IdentityStartupPolicy> bootstrapIdentity() async {
  if (!Util.isMacos()) return const IdentityStartupPolicy.normal();
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
  final tool = path.normalize(path.join(
      File(Platform.resolvedExecutable).parent.path,
      '..',
      'Helpers',
      AppIdentity.migrationExecutable));
  Future<ProcessResult> run(List<String> args) => Process.run(tool, args);
  final marker = File(path.join(target, '.kownload-identity.json'));
  if (!Util.isIsolatedPreview &&
      await _channel.invokeMethod<bool>('legacyRunning') == true) {
    if (!await marker.exists()) {
      throw const IdentityBootstrapException('请退出旧版 kownload 后重新打开，以便导入现有数据');
    }
    // After the import both apps would continue the same tasks; the importer
    // checks the legacy store lock as well.
    if (legacyGuardApplies(await marker.readAsString())) {
      throw IdentityBootstrapException(legacyRunningMessage,
          ignoreLegacyChanges: () =>
              ignoreLegacyChanges(source: source, target: target, run: run));
    }
  }
  final policy =
      await runIdentityImport(source: source, target: target, run: run);
  if (!Util.isIsolatedPreview) {
    await _channel.invokeMethod<void>('migratePreferences');
  }
  return policy;
}

typedef ImportRunner = Future<ProcessResult> Function(List<String> arguments);

Future<IdentityStartupPolicy> runIdentityImport(
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
    if (reply['ok'] != true && reply['error'] == 'legacy_running') {
      throw IdentityBootstrapException(legacyRunningMessage,
          ignoreLegacyChanges: () =>
              ignoreLegacyChanges(source: source, target: target, run: run));
    }
    if (reply['ok'] != true &&
        reply['error'] == 'legacy_changed_after_import') {
      throw IdentityBootstrapException(
          legacyChangedMessage(source: source, target: target),
          ignoreLegacyChanges: () =>
              ignoreLegacyChanges(source: source, target: target, run: run));
    }
    if (result.exitCode != 0 || reply['ok'] != true) {
      throw const IdentityBootstrapException(
          '数据导入未完成：旧库可能被占用，或目标已有未确认数据。请退出旧版后重试；原数据未改动');
    }
    try {
      return IdentityStartupPolicy.fromMarker(
          await File(path.join(target, '.kownload-identity.json'))
              .readAsString());
    } on FormatException {
      throw const IdentityBootstrapException(
          '无法识别任务启动政策，本次没有打开或继续任务。请使用配套版本和快照。');
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

/// Stores the persistent choice to keep this root and stop checking the
/// legacy root. Returns whether the importer confirmed it.
Future<bool> ignoreLegacyChanges(
    {required String source,
    required String target,
    required ImportRunner run}) async {
  try {
    final result = await run(
        ['--source', source, '--target', target, '--ignore-legacy-changes']);
    final reply = jsonDecode(result.stdout.toString());
    return result.exitCode == 0 && reply is Map && reply['ok'] == true;
  } catch (_) {
    return false;
  }
}

/// Shared with the persistent choice on the error page: the legacy root is
/// also upstream Gopeed's data directory.
const keepBothNote = '如果旧目录属于仍在使用的 Gopeed，或你想同时保留两个应用，可以点下面的按钮：'
    '保留本应用的数据，今后不再检查旧目录。两边如果是同一批任务，请在其中一个应用里关闭'
    '“打开应用时继续未完成的下载”（旧版中叫“自动开始任务”），避免两个应用同时写同一个文件。';

/// Whether the post-import guard applies: the import copied a legacy
/// database and the user has not chosen to keep this root for good. A legacy
/// root that appeared after a fresh setup belongs to another app (upstream
/// Gopeed shares it) and is not guarded.
bool legacyGuardApplies(String markerJson) {
  try {
    final marker = jsonDecode(markerJson);
    if (marker is! Map) return true;
    if (marker['ignoreLegacyChanges'] == true) return false;
    final source = marker['sourceDatabase'];
    return source is Map && source['present'] == true;
  } catch (_) {
    return true;
  }
}

const legacyRunningMessage = '旧版 kownload（或使用同一数据目录的 Gopeed）正在运行。'
    '两个版本同时运行会各自继续同一批未完成的任务、同时写同一个文件，所以本次没有打开下载数据，也没有继续任何任务。\n\n'
    '请先退出旧版，再重新打开本应用。\n\n'
    '$keepBothNote';

/// The old version used its data directory after the import (for example
/// after a rollback). Nothing is opened, resumed, moved or deleted; the user
/// keeps one side by renaming the other directory, or keeps this one for good.
String legacyChangedMessage({required String source, required String target}) {
  const stamp = r'$(date +%Y%m%d-%H%M%S)';
  return '旧版 kownload（或使用同一数据目录的 Gopeed）在迁移之后又使用过原来的数据目录，两边的下载记录已经不同。'
      '为避免沿用过期的记录，本次没有打开下载数据，也没有继续任何任务；两个目录都没有改动。\n\n'
      '请先退出所有版本的 kownload，在“终端”里执行下面任意一条命令，再重新打开本应用。\n\n'
      '以旧版现在的数据为准（回退后又在旧版里下载过，选这一条）：\n'
      'mv "$target" "$target.kept-$stamp"\n'
      '重新打开后会从旧目录重新导入。\n\n'
      '继续使用本应用迁移后的数据：\n'
      'mv "$source" "$source.kept-$stamp"\n\n'
      '两条命令都只给目录改名，改名后的目录可以随时改回原名。\n\n'
      '$keepBothNote';
}

class IdentityBootstrapException implements Exception {
  final String message;

  /// Offered on the error page when keeping this root for good is safe to
  /// choose: it records the choice in the import marker.
  final Future<bool> Function()? ignoreLegacyChanges;
  const IdentityBootstrapException(this.message, {this.ignoreLegacyChanges});
  @override
  String toString() => message;
}
