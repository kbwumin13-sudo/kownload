// Standalone subprocess fixture; never imported by production.
import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:gopeed/identity/app_identity.dart';
import 'package:gopeed/identity/native_manifest_service.dart';
import 'package:path/path.dart' as path;

class _InterruptedFile implements File {
  final File file;
  _InterruptedFile(this.file);
  @override
  String get path => file.path;
  @override
  Future<File> create({bool recursive = false, bool exclusive = false}) =>
      file.create(recursive: recursive, exclusive: exclusive);
  @override
  Future<File> writeAsBytes(List<int> bytes,
      {FileMode mode = FileMode.write, bool flush = false}) async {
    await file.writeAsBytes(bytes.take(5).toList(), mode: mode, flush: true);
    stdout.writeln(jsonEncode({'interrupted': true, 'path': file.path}));
    await stdout.flush();
    exit(0); // Actual process exit; service catch/finally cannot run.
  }

  @override
  dynamic noSuchMethod(Invocation invocation) =>
      throw UnsupportedError('unexpected interrupted-file operation');
}

void main(List<String> args) async {
  if (args.length != 2) {
    exit(2);
  }
  final home = path.normalize(path.absolute(args[0]));
  final storage = path.normalize(path.absolute(args[1]));
  final resolvedHome = await Directory(home).resolveSymbolicLinks();
  final resolvedStorage = await Directory(storage).resolveSymbolicLinks();
  final root = path.dirname(resolvedStorage);
  final temp = await Directory.systemTemp.resolveSymbolicLinks();
  if (!path.isWithin(temp, root) ||
      !path.basename(root).startsWith('k14-manifest-') ||
      path.dirname(resolvedHome) != root ||
      path.basename(resolvedHome) != 'home' ||
      path.basename(resolvedStorage) != 'data') {
    exit(2);
  }
  final outer = Zone.current;
  final service = NativeManifestService(
      home: home,
      storageDir: storage,
      loadAsset: () async => const [7, 8, 9, 10]);
  await IOOverrides.runZoned(() => service.connectBrowser('chrome'),
      fseGetType: (name, follow) =>
          outer.run(() => FileSystemEntity.type(name, followLinks: follow)),
      fseGetTypeSync: (name, follow) =>
          outer.run(() => FileSystemEntity.typeSync(name, followLinks: follow)),
      stat: (name) => outer.run(() => FileStat.stat(name)),
      statSync: (name) => outer.run(() => FileStat.statSync(name)),
      createFile: (name) {
        final file = outer.run(() => File(name));
        final backup = path.join(storage, 'native-manifest-backups', 'chrome');
        if (path.isWithin(backup, name) &&
            (name.endsWith('${AppIdentity.hostName}.json') ||
                name.endsWith('/original'))) {
          return _InterruptedFile(file);
        }
        return file;
      });
  stderr.writeln('interruption point not reached');
  exit(3);
}
