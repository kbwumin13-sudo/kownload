import 'dart:io';

import 'package:file_picker/file_picker.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

import '../api/netdisk.dart';

/// The picker must belong to this app so macOS grants this app file access.
class BrowserProfileAccess {
  static const channel = MethodChannel('gopeed/browser_profiles');

  static Future<String> read(String path) async {
    if (!kIsWeb && Platform.isMacOS) {
      try {
        return (await channel.invokeMethod<String>('read', {'path': path}))!;
      } on MissingPluginException {
        // Unit tests and older native hosts can still use ordinary file access.
      } on PlatformException catch (e) {
        throw NetdiskException(e.message ?? '无法读取浏览器个人资料', e.code);
      }
    }
    return File(path).readAsString();
  }

  static Future<String?> choose(
      {required String title, required String path}) async {
    if (!kIsWeb && Platform.isMacOS) {
      try {
        return await channel
            .invokeMethod<String>('choose', {'title': title, 'path': path});
      } on PlatformException catch (e) {
        throw NetdiskException(e.message ?? '无法选择浏览器个人资料', e.code);
      }
    }
    final result = await FilePicker.platform.pickFiles(dialogTitle: title);
    return result?.files.single.path;
  }
}
