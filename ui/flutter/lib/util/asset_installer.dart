import 'dart:io';

/// Replace executable assets with a fresh inode on macOS. Overwriting a Mach-O
/// in place can leave the kernel's cached code signature attached to old bytes.
Future<void> writeInstalledAsset(List<int> bytes, String targetPath,
    {bool executable = false}) async {
  final target = File(targetPath);
  await target.parent.create(recursive: true);
  if (!Platform.isMacOS) {
    await target.writeAsBytes(bytes, flush: true);
    if (executable && !Platform.isWindows) {
      final result = await Process.run('chmod', ['+x', targetPath]);
      if (result.exitCode != 0)
        throw FileSystemException('chmod failed', targetPath);
    }
    return;
  }
  final staging = await target.parent.createTemp('.gopeed-asset-');
  final stagedFile = File('${staging.path}/payload');
  try {
    await stagedFile.writeAsBytes(bytes, flush: true);
    if (executable) {
      final result = await Process.run('chmod', ['+x', stagedFile.path]);
      if (result.exitCode != 0)
        throw FileSystemException('chmod failed', targetPath);
    }
    await stagedFile.rename(targetPath);
  } finally {
    if (await staging.exists()) await staging.delete(recursive: true);
  }
}
