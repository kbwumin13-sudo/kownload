import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:get/get.dart';
import 'package:gopeed/macos/mac_widgets.dart';
import 'package:path/path.dart' as path;
import 'package:url_launcher/url_launcher.dart';

import '../../../../api/api.dart';
import '../../../../database/database.dart';
import '../../../../util/util.dart';
import '../controllers/extension_controller.dart';

class ExtensionCard extends StatelessWidget {
  const ExtensionCard({
    super.key,
    required this.item,
    required this.busy,
    required this.canUpdate,
    this.onTap,
    this.onToggle,
    this.onOpenSetting,
    this.onUpdate,
    this.onDelete,
    this.onInstall,
  });

  final ExtensionListItem item;
  final bool busy;
  final bool canUpdate;
  final VoidCallback? onTap;
  final Future<void> Function(bool value)? onToggle;
  final VoidCallback? onOpenSetting;
  final VoidCallback? onUpdate;
  final VoidCallback? onDelete;
  final VoidCallback? onInstall;

  @override
  Widget build(BuildContext context) {
    if (Util.isMacos()) return _buildMacRow(context);
    final installed = item.installed;
    final store = item.store;
    final installedFlag = item.isInstalled;

    final content = Stack(
      children: [
        Container(
          padding: const EdgeInsets.all(12),
          decoration: BoxDecoration(
            color: Theme.of(context).colorScheme.surface,
            borderRadius: BorderRadius.circular(12),
            border: Border.all(
              color: Theme.of(context).dividerColor.withValues(alpha: 0.2),
            ),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  _buildCardIcon(item),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          item.title,
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: Theme.of(context).textTheme.titleSmall,
                        ),
                        const SizedBox(height: 2),
                        Text(
                          '${item.author} · v${item.version}',
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: Theme.of(context).textTheme.bodySmall,
                        ),
                      ],
                    ),
                  ),
                  if (installed != null) ...[
                    Transform.scale(
                      scale: 0.82,
                      child: Switch(
                        value: !installed.disabled,
                        onChanged: busy || onToggle == null
                            ? null
                            : (value) async {
                                await onToggle!(value);
                              },
                      ),
                    ),
                  ],
                ],
              ),
              const SizedBox(height: 8),
              Text(
                item.description,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
              ),
              const Spacer(),
              Row(
                crossAxisAlignment: CrossAxisAlignment.center,
                children: [
                  Expanded(
                    child: Row(
                      children: [
                        if (store != null) ...[
                          _metricItem(context, Icons.star_rounded,
                              item.stars.toString()),
                          const SizedBox(width: 10),
                          _metricItem(context, Icons.download_outlined,
                              item.installCount.toString()),
                        ],
                      ],
                    ),
                  ),
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      if (installed != null && canUpdate)
                        IconButton.filledTonal(
                          tooltip: 'newVersionUpdate'.tr,
                          onPressed: busy ? null : onUpdate,
                          icon: busy
                              ? const SizedBox(
                                  width: 14,
                                  height: 14,
                                  child:
                                      CircularProgressIndicator(strokeWidth: 2),
                                )
                              : const Icon(Icons.refresh_rounded),
                        ),
                      if (!installedFlag && store != null)
                        IconButton.filledTonal(
                          tooltip: 'extensionInstall'.tr,
                          onPressed: busy ? null : onInstall,
                          icon: busy
                              ? const SizedBox(
                                  width: 14,
                                  height: 14,
                                  child:
                                      CircularProgressIndicator(strokeWidth: 2),
                                )
                              : const Icon(Icons.download),
                        ),
                      if ((item.homepage ?? '').isNotEmpty)
                        IconButton(
                          tooltip: 'homepage'.tr,
                          onPressed: () => launchUrl(Uri.parse(item.homepage!)),
                          icon: const Icon(Icons.home_outlined),
                        ),
                      if ((item.repoUrl ?? '').isNotEmpty)
                        IconButton(
                          tooltip: 'GitHub',
                          onPressed: () => launchUrl(Uri.parse(item.repoUrl!)),
                          icon: const Icon(Icons.code),
                        ),
                      if (installed != null &&
                          installed.settings?.isNotEmpty == true)
                        IconButton(
                          tooltip: 'setting'.tr,
                          onPressed: busy ? null : onOpenSetting,
                          icon: const Icon(Icons.settings),
                        ),
                      if (installed != null)
                        IconButton(
                          tooltip: 'delete'.tr,
                          onPressed: busy ? null : onDelete,
                          icon: const Icon(Icons.delete_outline),
                        ),
                    ],
                  ),
                ],
              ),
            ],
          ),
        ),
        if (installedFlag && canUpdate)
          Positioned(
            top: 10,
            right: 10,
            child: _updateDot(),
          ),
      ],
    );

    if (onTap == null) return content;
    return InkWell(
      borderRadius: BorderRadius.circular(12),
      onTap: onTap,
      child: content,
    );
  }

  Widget _buildMacRow(BuildContext context) {
    final colors = MacColors.of(context);
    final installed = item.installed;
    Widget action(IconData icon, String tooltip, VoidCallback? callback) =>
        IconButton(
          tooltip: tooltip,
          icon: Icon(icon, size: 16),
          onPressed: busy ? null : callback,
          visualDensity: VisualDensity.compact,
          constraints: const BoxConstraints(minWidth: 28, minHeight: 28),
          padding: EdgeInsets.zero,
        );

    return InkWell(
      onTap: onTap,
      child: Container(
        constraints: const BoxConstraints(minHeight: 68),
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 8),
        decoration: BoxDecoration(
          color: colors.surface,
          border: Border(bottom: BorderSide(color: colors.border)),
        ),
        child: Row(children: [
          SizedBox(
              width: 36,
              height: 36,
              child: FittedBox(child: _buildCardIcon(item))),
          const SizedBox(width: 12),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              mainAxisSize: MainAxisSize.min,
              children: [
                Row(children: [
                  Flexible(
                      child: Text(item.title,
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: TextStyle(
                              color: colors.text,
                              fontWeight: FontWeight.w600))),
                  if (canUpdate)
                    Padding(
                      padding: const EdgeInsets.only(left: 7),
                      child: Icon(CupertinoIcons.arrow_up_circle_fill,
                          size: 13, color: colors.accent),
                    ),
                ]),
                const SizedBox(height: 2),
                Text('${item.author} · v${item.version} · ${item.description}',
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: TextStyle(color: colors.secondary, fontSize: 12)),
              ],
            ),
          ),
          const SizedBox(width: 8),
          if (busy)
            const SizedBox(
                width: 16,
                height: 16,
                child: CircularProgressIndicator(strokeWidth: 2)),
          if (installed != null)
            Transform.scale(
              scale: 0.72,
              child: Switch.adaptive(
                activeColor: colors.accent,
                value: !installed.disabled,
                onChanged: busy || onToggle == null ? null : onToggle,
              ),
            ),
          if (installed == null && item.store != null)
            MacButton(
              onPressed: busy ? null : onInstall,
              icon: CupertinoIcons.arrow_down_to_line,
              child: Text('extensionInstall'.tr),
            ),
          if (installed != null && canUpdate)
            action(CupertinoIcons.arrow_clockwise, 'newVersionUpdate'.tr,
                onUpdate),
          if (installed != null && installed.settings?.isNotEmpty == true)
            action(CupertinoIcons.gear, 'setting'.tr, onOpenSetting),
          if (installed != null)
            action(CupertinoIcons.trash, 'delete'.tr, onDelete),
          if ((item.homepage ?? '').isNotEmpty)
            action(CupertinoIcons.house, 'homepage'.tr,
                () => launchUrl(Uri.parse(item.homepage!))),
          if ((item.repoUrl ?? '').isNotEmpty)
            action(CupertinoIcons.chevron_left, 'GitHub',
                () => launchUrl(Uri.parse(item.repoUrl!))),
          if (onTap != null)
            Icon(CupertinoIcons.chevron_right,
                size: 12, color: colors.secondary),
        ]),
      ),
    );
  }

  Widget _buildCardIcon(ExtensionListItem item) {
    final storeIcon = item.icon;
    if (storeIcon != null && storeIcon.isNotEmpty) {
      return ClipRRect(
        borderRadius: BorderRadius.circular(8),
        child: Image.network(
          storeIcon,
          width: 42,
          height: 42,
          fit: BoxFit.cover,
          errorBuilder: (_, __, ___) => Image.asset(
              'assets/extension/default_icon.png',
              width: 42,
              height: 42),
        ),
      );
    }

    final extension = item.installed;
    if (extension == null) {
      return Image.asset('assets/extension/default_icon.png',
          width: 42, height: 42);
    }

    final image = extension.icon.isEmpty
        ? Image.asset('assets/extension/default_icon.png',
            width: 42, height: 42)
        : Util.isWeb()
            ? Image.network(
                join('/fs/extensions/${extension.identity}/${extension.icon}'),
                width: 42,
                height: 42,
                headers: {
                  'Authorization': 'Bearer ${Database.instance.getWebToken()}'
                },
                errorBuilder: (_, __, ___) => Image.asset(
                    'assets/extension/default_icon.png',
                    width: 42,
                    height: 42),
              )
            : Image.file(
                extension.devMode
                    ? File(path.join(extension.devPath, extension.icon))
                    : File(path.join(Util.getStorageDir(), 'extensions',
                        extension.identity, extension.icon)),
                width: 42,
                height: 42,
                errorBuilder: (_, __, ___) => Image.asset(
                    'assets/extension/default_icon.png',
                    width: 42,
                    height: 42),
              );

    return ClipRRect(borderRadius: BorderRadius.circular(8), child: image);
  }

  Widget _metricItem(BuildContext context, IconData icon, String text) {
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Icon(icon, size: 18, color: Theme.of(context).hintColor),
        const SizedBox(width: 4),
        Text(text, style: Theme.of(context).textTheme.bodyMedium),
      ],
    );
  }

  Widget _updateDot() {
    return Container(
      width: 9,
      height: 9,
      decoration: const BoxDecoration(
        color: Colors.redAccent,
        shape: BoxShape.circle,
      ),
    );
  }
}
