import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/util/asset_installer.dart';

void main() {
  test('executable updates replace bytes and retain execute permission',
      () async {
    final temp = await Directory.systemTemp.createTemp('gopeed-asset-test-');
    addTearDown(() => temp.delete(recursive: true));
    final file = File('${temp.path}/host');
    await writeInstalledAsset([1, 2, 3], file.path, executable: true);
    await writeInstalledAsset([4, 5], file.path, executable: true);
    expect(await file.readAsBytes(), [4, 5]);
    if (!Platform.isWindows) expect((await file.stat()).mode & 73, isNonZero);
    expect(await temp.list().length, 1);
  });

  final project = Directory.current.parent.parent.parent.path;
  final oldHost = File(
      '$project/artifacts/baseline/Gopeed.app/Contents/Frameworks/App.framework/Resources/flutter_assets/assets/exec/host');
  final newHost = File('$project/artifacts/build/media-core/host');
  test('macOS replacement remains executable after a Mach-O update', () async {
    final temp = await Directory.systemTemp.createTemp('gopeed-mach-test-');
    addTearDown(() => temp.delete(recursive: true));
    final file = File('${temp.path}/host');
    await writeInstalledAsset(await oldHost.readAsBytes(), file.path,
        executable: true);
    expect((await Process.run(file.path, [])).exitCode, 0);
    final before =
        (await Process.run('/usr/bin/stat', ['-f', '%i', file.path])).stdout;
    await writeInstalledAsset(await newHost.readAsBytes(), file.path,
        executable: true);
    final after =
        (await Process.run('/usr/bin/stat', ['-f', '%i', file.path])).stdout;
    expect(after, isNot(before));
    expect((await Process.run(file.path, [])).exitCode, 0);
  }, skip: !Platform.isMacOS || !oldHost.existsSync() || !newHost.existsSync());
}
