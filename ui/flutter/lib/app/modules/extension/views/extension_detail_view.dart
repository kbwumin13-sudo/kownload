import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter_markdown_plus/flutter_markdown_plus.dart';
import 'package:get/get.dart';
import 'package:path/path.dart' as path;
import 'package:url_launcher/url_launcher.dart';
import 'package:gopeed/macos/mac_widgets.dart';

import '../../../../api/model/extension.dart';
import '../../../../api/model/store_extension.dart';
import '../../../../util/message.dart';
import '../../../../util/util.dart';
import '../controllers/extension_controller.dart';

class ExtensionDetailDrawer extends GetView<ExtensionController> {
  const ExtensionDetailDrawer({
    super.key,
    required this.extension,
    required this.onClose,
    this.installed,
  });

  final StoreExtension extension;
  final Extension? installed;
  final VoidCallback onClose;

  @override
  Widget build(BuildContext context) {
    final mac = Util.isMacos();
    final colors = MacColors.of(context);
    return Material(
      color: mac ? colors.surface : Theme.of(context).colorScheme.surface,
      child: SafeArea(
        child: Column(
          children: [
            Padding(
              padding: mac
                  ? const EdgeInsets.fromLTRB(18, 12, 12, 10)
                  : const EdgeInsets.fromLTRB(16, 14, 12, 10),
              child: Row(
                children: [
                  Expanded(
                    child: Text(
                      extension.title,
                      style: mac
                          ? TextStyle(
                              color: colors.text,
                              fontSize: 15,
                              fontWeight: FontWeight.w600)
                          : Theme.of(context).textTheme.titleLarge,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                  ),
                  IconButton(
                    onPressed: onClose,
                    icon: Icon(mac ? CupertinoIcons.xmark : Icons.close,
                        size: mac ? 16 : 24),
                  )
                ],
              ),
            ),
            const Divider(height: 1),
            Expanded(
              child: Obx(() {
                final localInstalled =
                    installed ?? controller.findInstalled(extension);
                final canUpdate = controller.canUpdateFromStore(extension);
                final busy =
                    controller.busyExtensionIds.contains(extension.id) ||
                        (localInstalled != null &&
                            controller.busyExtensionIds
                                .contains(localInstalled.identity));

                return ListView(
                  padding: EdgeInsets.all(mac ? 18 : 16),
                  children: [
                    Row(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        _buildIcon(),
                        SizedBox(width: mac ? 10 : 12),
                        Expanded(
                          child: Column(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Text(
                                  '${extension.author} • v${extension.version}'),
                              const SizedBox(height: 6),
                              Text(
                                extension.description,
                                style: Theme.of(context).textTheme.bodyMedium,
                              ),
                            ],
                          ),
                        ),
                      ],
                    ),
                    SizedBox(height: mac ? 12 : 14),
                    Wrap(
                      spacing: 8,
                      runSpacing: 8,
                      children: [
                        if (localInstalled == null)
                          mac
                              ? MacButton(
                                  primary: true,
                                  onPressed: busy
                                      ? null
                                      : () async {
                                          try {
                                            await controller
                                                .installFromStore(extension);
                                            showMessage('tip'.tr,
                                                'extensionInstallSuccess'.tr);
                                          } catch (e) {
                                            showErrorMessage(e);
                                          }
                                        },
                                  icon: CupertinoIcons.arrow_down_to_line,
                                  child: Text('extensionInstall'.tr),
                                )
                              : ElevatedButton.icon(
                                  onPressed: busy
                                      ? null
                                      : () async {
                                          try {
                                            await controller
                                                .installFromStore(extension);
                                            showMessage('tip'.tr,
                                                'extensionInstallSuccess'.tr);
                                          } catch (e) {
                                            showErrorMessage(e);
                                          }
                                        },
                                  icon: busy
                                      ? const SizedBox(
                                          width: 14,
                                          height: 14,
                                          child: CircularProgressIndicator(
                                              strokeWidth: 2),
                                        )
                                      : const Icon(Icons.download),
                                  label: Text('extensionInstall'.tr),
                                ),
                        if (localInstalled != null && canUpdate)
                          mac
                              ? MacButton(
                                  primary: true,
                                  onPressed: busy
                                      ? null
                                      : () async {
                                          try {
                                            await controller.upgradeExtension(
                                                localInstalled);
                                            showMessage('tip'.tr,
                                                'extensionUpdateSuccess'.tr);
                                          } catch (e) {
                                            showErrorMessage(e);
                                          }
                                        },
                                  icon: CupertinoIcons.arrow_clockwise,
                                  child: Text('newVersionUpdate'.tr),
                                )
                              : ElevatedButton.icon(
                                  onPressed: busy
                                      ? null
                                      : () async {
                                          try {
                                            await controller.upgradeExtension(
                                                localInstalled);
                                            showMessage('tip'.tr,
                                                'extensionUpdateSuccess'.tr);
                                          } catch (e) {
                                            showErrorMessage(e);
                                          }
                                        },
                                  icon: busy
                                      ? const SizedBox(
                                          width: 14,
                                          height: 14,
                                          child: CircularProgressIndicator(
                                              strokeWidth: 2),
                                        )
                                      : const Icon(Icons.refresh_rounded),
                                  label: Text('newVersionUpdate'.tr),
                                ),
                        if ((extension.homepage ?? '').isNotEmpty)
                          mac
                              ? MacButton(
                                  onPressed: () =>
                                      launchUrl(Uri.parse(extension.homepage!)),
                                  icon: CupertinoIcons.house,
                                  child: Text('homepage'.tr),
                                )
                              : OutlinedButton.icon(
                                  onPressed: () =>
                                      launchUrl(Uri.parse(extension.homepage!)),
                                  icon: const Icon(Icons.home_outlined),
                                  label: Text('homepage'.tr),
                                ),
                        mac
                            ? MacButton(
                                onPressed: () =>
                                    launchUrl(Uri.parse(extension.repoUrl)),
                                icon: CupertinoIcons.chevron_left,
                                child: const Text('GitHub'),
                              )
                            : OutlinedButton.icon(
                                onPressed: () =>
                                    launchUrl(Uri.parse(extension.repoUrl)),
                                icon: const Icon(Icons.code),
                                label: const Text('GitHub'),
                              ),
                      ],
                    ),
                    const SizedBox(height: 14),
                    const Divider(height: 1),
                    const SizedBox(height: 12),
                    _buildReadme(context, localInstalled),
                  ],
                );
              }),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildReadme(BuildContext context, Extension? installed) {
    return FutureBuilder<_ReadmeInfo>(
      future: _loadReadme(installed),
      builder: (context, snapshot) {
        final info = snapshot.data;
        final markdown = info?.content ?? extension.readme ?? '';
        if (markdown.trim().isEmpty) {
          return Text('No README',
              style: Theme.of(context).textTheme.bodyMedium);
        }

        return MarkdownBody(
          data: markdown,
          selectable: true,
          onTapLink: (text, href, title) {
            if (href == null || href.isEmpty) return;
            final resolved = _resolvePath(href, info, forImage: false);
            if (resolved == null) return;
            launchUrl(Uri.parse(resolved),
                mode: LaunchMode.externalApplication);
          },
          imageBuilder: (uri, title, alt) {
            final resolved = _resolvePath(uri.toString(), info, forImage: true);
            if (resolved == null) return const SizedBox.shrink();
            if (resolved.startsWith('file://')) {
              return Padding(
                padding: const EdgeInsets.symmetric(vertical: 8),
                child: Image.file(
                  File(Uri.parse(resolved).toFilePath()),
                  fit: BoxFit.contain,
                  errorBuilder: (_, __, ___) => const SizedBox.shrink(),
                ),
              );
            }
            return Padding(
              padding: const EdgeInsets.symmetric(vertical: 8),
              child: Image.network(
                resolved,
                fit: BoxFit.contain,
                errorBuilder: (_, __, ___) => const SizedBox.shrink(),
              ),
            );
          },
        );
      },
    );
  }

  Future<_ReadmeInfo> _loadReadme(Extension? installed) async {
    if (installed == null) {
      return _ReadmeInfo(
        content: extension.readme ?? '',
        mode: _ReadmeMode.remote,
        localReadmePath: null,
      );
    }

    final rootDir = installed.devMode
        ? installed.devPath
        : path.join(Util.getStorageDir(), 'extensions', installed.identity);
    final candidates = [
      path.join(rootDir, 'README.md'),
      path.join(rootDir, 'readme.md'),
      path.join(rootDir, 'README.MD'),
    ];
    for (final filePath in candidates) {
      final file = File(filePath);
      if (await file.exists()) {
        return _ReadmeInfo(
          content: await file.readAsString(),
          mode: _ReadmeMode.local,
          localReadmePath: filePath,
        );
      }
    }

    return _ReadmeInfo(
      content: extension.readme ?? '',
      mode: _ReadmeMode.remote,
      localReadmePath: null,
    );
  }

  String? _resolvePath(String raw, _ReadmeInfo? info,
      {required bool forImage}) {
    final value = raw.trim();
    if (value.isEmpty) return null;
    final uri = Uri.tryParse(value);
    if (uri != null && uri.hasScheme) {
      return uri.toString();
    }

    if (info?.mode == _ReadmeMode.local &&
        info?.localReadmePath != null &&
        forImage) {
      final readmeDir = path.dirname(info!.localReadmePath!);
      final clean = value.split('#').first;
      final absolute = path.normalize(path.join(readmeDir, clean));
      return Uri.file(absolute).toString();
    }

    final ref =
        extension.commitSha?.isNotEmpty == true ? extension.commitSha! : 'HEAD';
    final dir = (extension.directory ?? '').trim();
    final baseSegments = [
      if (dir.isNotEmpty) ...dir.split('/').where((e) => e.isNotEmpty),
      '',
    ];

    final base = forImage
        ? Uri.https('raw.githubusercontent.com',
            '/${extension.repoFullName}/$ref/${baseSegments.join('/')}')
        : Uri.https('github.com',
            '/${extension.repoFullName}/blob/$ref/${baseSegments.join('/')}');
    return base.resolve(value).toString();
  }

  Widget _buildIcon() {
    if ((extension.icon ?? '').isEmpty) {
      return Image.asset('assets/extension/default_icon.png',
          width: 56, height: 56);
    }
    return ClipRRect(
      borderRadius: BorderRadius.circular(12),
      child: Image.network(
        extension.icon!,
        width: 56,
        height: 56,
        fit: BoxFit.cover,
        errorBuilder: (_, __, ___) {
          return Image.asset('assets/extension/default_icon.png',
              width: 56, height: 56);
        },
      ),
    );
  }
}

enum _ReadmeMode {
  remote,
  local,
}

class _ReadmeInfo {
  final String content;
  final _ReadmeMode mode;
  final String? localReadmePath;

  _ReadmeInfo({
    required this.content,
    required this.mode,
    required this.localReadmePath,
  });
}
