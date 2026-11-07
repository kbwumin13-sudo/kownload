import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:get/get.dart';
import 'package:url_launcher/url_launcher.dart';

import '../api/api.dart';
import 'release_version.dart';

export 'release_version.dart' show VersionInfo;

// Release builds set this from config/release.json. Direct development builds
// read the running core's actual version; an unknown version never offers an update.
const bundledReleaseVersion =
    String.fromEnvironment('KOWNLOAD_RELEASE_VERSION');

Future<VersionInfo?> checkUpdate() async {
  try {
    final current = bundledReleaseVersion.isEmpty
        ? await getCoreVersion()
        : bundledReleaseVersion;
    if (current == null) return null;
    final response = await proxyRequest(
        'https://api.github.com/repos/kbwumin13-sudo/kownload/releases/latest');
    final data = jsonDecode(response.data ?? '');
    if (data is! Map<String, dynamic>) return null;
    return newerRelease(data, current);
  } catch (_) {
    return null;
  }
}

/// Notification only. Application files, tasks and configuration are untouched.
Future<void> showUpdateDialog(BuildContext context, VersionInfo release) {
  return showDialog<void>(
    context: context,
    builder: (dialogContext) => AlertDialog(
      title: Text('newVersionTitle'.trParams({'version': release.version})),
      content: SizedBox(
        width: 480,
        child: SingleChildScrollView(child: Text(release.changeLog)),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(dialogContext).pop(),
          child: Text('newVersionLater'.tr),
        ),
        TextButton(
          onPressed: () => launchUrl(release.releasePage,
              mode: LaunchMode.externalApplication),
          child: Text(Get.locale?.languageCode == 'zh'
              ? '打开更新页面'
              : 'Open release page'),
        ),
      ],
    ),
  );
}
