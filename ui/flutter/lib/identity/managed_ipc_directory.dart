import 'dart:convert';
import 'dart:io';

import 'package:path/path.dart' as path;

import 'app_identity.dart';

class ManagedIpcDirectoryException implements Exception {
  final String reason;
  const ManagedIpcDirectoryException(this.reason);

  @override
  String toString() => '无法准备应用专用 IPC 目录（$reason）。'
      '请检查应用数据目录的归属和权限；未改用其它连接地址。';
}

Future<String> _uid(String executable, List<String> arguments) async {
  final result = await Process.run(executable, arguments);
  final value = result.stdout.toString().trim();
  if (result.exitCode != 0 || !RegExp(r'^\d+$').hasMatch(value)) {
    throw const ManagedIpcDirectoryException('ownership_unverified');
  }
  return value;
}

Future<void> _ownedDirectory(String directory, String uid) async {
  if (await FileSystemEntity.type(directory, followLinks: false) !=
      FileSystemEntityType.directory) {
    throw const ManagedIpcDirectoryException('directory_conflict');
  }
  if (await _uid('/usr/bin/stat', ['-f', '%u', directory]) != uid) {
    throw const ManagedIpcDirectoryException('foreign_directory');
  }
}

/// macOS default App IPC only, before the core or listeners are started.
/// Existing owned directories may become private; children are never changed.
/// Custom Unix addresses are deliberately not accepted by this interface.
Future<void> ensureManagedIpcDirectory({required String storageRoot}) async {
  final root = path.normalize(storageRoot);
  final directory = path.join(root, 'ipc');
  if (!Platform.isMacOS ||
      !path.isAbsolute(storageRoot) ||
      path.equals(root, path.separator) ||
      path.dirname(path.join(root, AppIdentity.apiSocket)) != directory) {
    throw const ManagedIpcDirectoryException('invalid_managed_root');
  }
  if (utf8.encode(path.join(root, AppIdentity.apiSocket)).length >= 104) {
    throw const ManagedIpcDirectoryException('ipc_path_too_long');
  }
  try {
    final uid = await _uid('/usr/bin/id', ['-u']);
    await _ownedDirectory(root, uid);
    if (((await Directory(root).stat()).mode & 0x12) != 0) {
      throw const ManagedIpcDirectoryException('unsafe_root_permissions');
    }
    final type = await FileSystemEntity.type(directory, followLinks: false);
    if (type == FileSystemEntityType.notFound) {
      // The importer already created the owned root. Never create/chmod any
      // parent or follow a root symlink to repair an unrelated path.
      await Directory(directory).create();
    }
    await _ownedDirectory(directory, uid);
    if (((await Directory(directory).stat()).mode & 0x1ff) != 0x1c0) {
      // -h does not follow a final symlink if one replaces the checked entry.
      // No recursion and no changes to sockets, task data or child files.
      final result = await Process.run('/bin/chmod', ['-h', '700', directory]);
      if (result.exitCode != 0) {
        throw const ManagedIpcDirectoryException('permission_failed');
      }
    }
    await _ownedDirectory(root, uid);
    await _ownedDirectory(directory, uid);
    if (((await Directory(directory).stat()).mode & 0x1ff) != 0x1c0) {
      throw const ManagedIpcDirectoryException('permission_failed');
    }
  } on ManagedIpcDirectoryException {
    rethrow;
  } on FileSystemException {
    throw const ManagedIpcDirectoryException('permission_failed');
  } on ProcessException {
    throw const ManagedIpcDirectoryException('ownership_unverified');
  }
}
