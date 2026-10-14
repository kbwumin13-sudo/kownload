import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/services.dart';
import 'package:flutter_form_builder/flutter_form_builder.dart';
import 'package:form_builder_validators/form_builder_validators.dart';
import 'package:get/get.dart';
import 'package:rounded_loading_button_plus/rounded_loading_button.dart';
import 'package:url_launcher/url_launcher.dart';
import 'package:gopeed/macos/mac_widgets.dart';

import '../../../../api/api.dart';
import '../../../../api/model/extension.dart';
import '../../../../api/model/install_extension.dart';
import '../../../../api/model/store_extension.dart';
import '../../../../api/model/update_extension_settings.dart';
import '../../../../util/message.dart';
import '../../../../util/util.dart';
import '../../../views/icon_button_loading.dart';
import '../../../views/responsive_builder.dart';
import '../controllers/extension_controller.dart';
import 'extension_card.dart';
import 'extension_detail_view.dart';

enum ExtensionViewMode { installed, maintenance }

class ExtensionView extends StatefulWidget {
  final ExtensionViewMode mode;
  final bool embedded;
  const ExtensionView({
    super.key,
    this.mode = ExtensionViewMode.installed,
    this.embedded = false,
  });

  @override
  State<ExtensionView> createState() => _ExtensionViewState();
}

class _ExtensionViewState extends State<ExtensionView> {
  ExtensionController get controller => Get.find<ExtensionController>();
  bool _showStore = false;
  bool get _maintenance => widget.mode == ExtensionViewMode.maintenance;
  ExtensionListFilter get _filter => Util.isMacos()
      ? (_showStore
          ? ExtensionListFilter.market
          : ExtensionListFilter.installed)
      : controller.listFilter.value;

  @override
  void initState() {
    super.initState();
    if (!Get.isRegistered<ExtensionController>()) {
      Get.put(ExtensionController());
    }
  }

  @override
  void dispose() {
    _installUrlController.dispose();
    _searchController.dispose();
    super.dispose();
  }

  final _installUrlController = TextEditingController();
  final _searchController = TextEditingController();
  final _installBtnController = IconButtonLoadingController();

  Future<void> _doInstall() async {
    final url = _installUrlController.text.trim();
    if (url.isEmpty) {
      controller.tryOpenDevMode();
      return;
    }
    if (controller.busyExtensionIds
        .contains(ExtensionController.manualInstallBusyKey)) {
      return;
    }
    if (!Util.isMacos()) _installBtnController.start();
    try {
      await controller.installFromUrl(url);
      showMessage('tip'.tr, 'extensionInstallSuccess'.tr);
    } catch (e) {
      showErrorMessage(e);
    } finally {
      if (!Util.isMacos()) _installBtnController.stop();
    }
  }

  Future<void> _installFromFolder() async {
    if (controller.busyExtensionIds
        .contains(ExtensionController.manualInstallBusyKey)) {
      return;
    }
    final dir = await FilePicker.platform.getDirectoryPath();
    if (dir == null) return;
    try {
      await controller.installFromUrl(dir, devInstall: true);
      showMessage('tip'.tr, 'extensionInstallSuccess'.tr);
    } catch (e) {
      showErrorMessage(e);
    }
  }

  @override
  Widget build(BuildContext context) {
    final args = Get.rootDelegate.arguments();
    if (args is InstallExtension && !controller.pendingInstallHandled) {
      controller.pendingInstallHandled = true;
      _installUrlController.text = args.url;
      WidgetsBinding.instance.addPostFrameCallback((_) => _doInstall());
    }

    if (Util.isMacos()) return _buildMacView(context);
    return Scaffold(
      body: SafeArea(
        child: Obx(
          () => RefreshIndicator(
            onRefresh: controller.loadInitialData,
            child: ListView(
              padding: EdgeInsets.symmetric(
                horizontal: ResponsiveBuilder.isNarrow(context) ? 16 : 24,
                vertical: 20,
              ),
              children: [
                _buildMarketToolbar(context),
                const SizedBox(height: 12),
                _buildFilterBar(context),
                if (controller.showInstallTools.value) ...[
                  const SizedBox(height: 10),
                  _buildInstallPanel(context),
                ],
                const SizedBox(height: 12),
                _buildUnifiedGrid(context),
                if (_filter == ExtensionListFilter.market &&
                    controller.storePagination.value?.hasNext == true) ...[
                  const SizedBox(height: 12),
                  Align(
                    alignment: Alignment.center,
                    child: OutlinedButton.icon(
                      onPressed: controller.loadingMoreStore.value
                          ? null
                          : controller.loadMoreStore,
                      icon: controller.loadingMoreStore.value
                          ? const SizedBox(
                              width: 14,
                              height: 14,
                              child: CircularProgressIndicator(strokeWidth: 2),
                            )
                          : const Icon(Icons.expand_more),
                      label: Text('extensionLoadMore'.tr),
                    ),
                  ),
                ],
                if (_filter == ExtensionListFilter.market &&
                    controller.storePagination.value != null &&
                    controller.storeExtensions.isNotEmpty &&
                    controller.storePagination.value!.hasNext == false) ...[
                  const SizedBox(height: 12),
                  Center(
                    child: Text(
                      'extensionNoMore'.tr,
                      style: Get.textTheme.bodySmall
                          ?.copyWith(color: Get.theme.hintColor),
                    ),
                  ),
                ],
              ],
            ),
          ),
        ),
      ),
    );
  }

  Future<void> _openStore() async {
    setState(() => _showStore = true);
    if (controller.storePagination.value == null) {
      await controller.refreshStore();
    }
  }

  Widget _buildMacView(BuildContext context) {
    final colors = MacColors.of(context);
    final content = Obx(() => Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Wrap(
              spacing: 8,
              runSpacing: 6,
              crossAxisAlignment: WrapCrossAlignment.center,
              children: [
                Text(_showStore ? '扩展商店' : '已安装的扩展',
                    style: TextStyle(color: colors.secondary, fontSize: 12)),
                IconButton(
                  tooltip: '刷新已安装扩展',
                  onPressed: () => controller.loadInstalled(),
                  icon: const Icon(CupertinoIcons.arrow_clockwise, size: 16),
                ),
                if (_maintenance) ...[
                  MacButton(
                    onPressed: controller.loadingInstalled.value
                        ? null
                        : () => controller.loadInstalled(refreshUpdates: true),
                    child: const Text('检查扩展更新'),
                  ),
                  MacButton(
                    onPressed: _showStore
                        ? () => setState(() => _showStore = false)
                        : _openStore,
                    child: Text(_showStore ? '返回已安装' : '打开扩展商店'),
                  ),
                  MacButton(
                    onPressed: controller.toggleInstallTools,
                    icon: CupertinoIcons.link,
                    child: Text('extensionManualInstall'.tr),
                  ),
                  IconButton(
                    tooltip: 'extensionDevelop'.tr,
                    onPressed: () => launchUrl(
                        Uri.parse('https://gopeed.com/docs/dev-extension')),
                    icon: const Icon(CupertinoIcons.book, size: 16),
                  ),
                ],
              ],
            ),
            if (_maintenance && _showStore) ...[
              const SizedBox(height: 10),
              Row(children: [
                Expanded(
                  child: Focus(
                    onKeyEvent: (node, event) {
                      if (event is KeyDownEvent &&
                          event.logicalKey == LogicalKeyboardKey.enter) {
                        controller.searchStore(_searchController.text);
                        return KeyEventResult.handled;
                      }
                      return KeyEventResult.ignored;
                    },
                    child: MacSearchField(
                        controller: _searchController,
                        hint: 'extensionSearchHint'.tr),
                  ),
                ),
                IconButton(
                  tooltip: 'extensionSearchHint'.tr,
                  onPressed: () =>
                      controller.searchStore(_searchController.text),
                  icon: const Icon(CupertinoIcons.search, size: 17),
                ),
              ]),
              _buildSortTabs(context),
            ],
            if (_maintenance && controller.showInstallTools.value) ...[
              const SizedBox(height: 10),
              _buildInstallPanel(context),
            ],
            const SizedBox(height: 10),
            Container(
              decoration: BoxDecoration(
                color: colors.surface,
                border: Border.all(color: colors.border),
                borderRadius: BorderRadius.circular(8),
              ),
              clipBehavior: Clip.antiAlias,
              child: _buildUnifiedGrid(context),
            ),
            if (_maintenance &&
                _showStore &&
                controller.storePagination.value?.hasNext == true)
              Padding(
                padding: const EdgeInsets.only(top: 12),
                child: Center(
                    child: MacButton(
                  onPressed: controller.loadingMoreStore.value
                      ? null
                      : controller.loadMoreStore,
                  child: Text('extensionLoadMore'.tr),
                )),
              ),
            if (_maintenance &&
                _showStore &&
                controller.storePagination.value?.hasNext == false)
              Padding(
                padding: const EdgeInsets.only(top: 12),
                child: Center(
                    child: Text('extensionNoMore'.tr,
                        style:
                            TextStyle(color: colors.secondary, fontSize: 12))),
              ),
          ],
        ));
    if (widget.embedded) return content;
    return Scaffold(
        body: MacPage(
      title: '连接与能力',
      child: ListView(
        padding: const EdgeInsets.fromLTRB(22, 16, 22, 24),
        children: [content],
      ),
    ));
  }

  Widget _buildInstallPanel(BuildContext context) {
    if (Util.isMacos()) {
      final colors = MacColors.of(context);
      return Container(
        padding: const EdgeInsets.all(10),
        decoration: BoxDecoration(
          color: colors.surface,
          border: Border.all(color: colors.border),
          borderRadius: BorderRadius.circular(8),
        ),
        child: Row(children: [
          Expanded(
              child: TextField(
            controller: _installUrlController,
            onSubmitted: (_) => _doInstall(),
            decoration: InputDecoration(
              isDense: true,
              hintText: 'extensionInstallUrl'.tr,
              border: const OutlineInputBorder(),
            ),
          )),
          const SizedBox(width: 8),
          MacButton(
            onPressed: controller.busyExtensionIds
                    .contains(ExtensionController.manualInstallBusyKey)
                ? null
                : _doInstall,
            icon: CupertinoIcons.arrow_down_to_line,
            child: Text('extensionInstall'.tr),
          ),
          if (controller.devMode.value) ...[
            const SizedBox(width: 6),
            IconButton(
              tooltip: 'extensionLoadLocal'.tr,
              onPressed: controller.busyExtensionIds
                      .contains(ExtensionController.manualInstallBusyKey)
                  ? null
                  : _installFromFolder,
              icon: const Icon(CupertinoIcons.folder_open, size: 17),
            ),
          ],
        ]),
      );
    }
    final isNarrow = ResponsiveBuilder.isNarrow(context);

    return isNarrow
        ? Column(
            children: [
              TextField(
                controller: _installUrlController,
                decoration: InputDecoration(
                  isDense: true,
                  labelText: 'extensionInstallUrl'.tr,
                  hintText: 'https://github.com/author/repo',
                  border: const OutlineInputBorder(),
                ),
              ),
              const SizedBox(height: 8),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  IconButtonLoading(
                    controller: _installBtnController,
                    onPressed: _doInstall,
                    icon: const Icon(Icons.download),
                  ),
                  if (controller.devMode.value && Util.isDesktop()) ...[
                    const SizedBox(width: 8),
                    IconButton(
                      tooltip: 'extensionLoadLocal'.tr,
                      onPressed: _installFromFolder,
                      icon: const Icon(Icons.folder_open),
                    ),
                  ]
                ],
              ),
            ],
          )
        : Row(
            children: [
              Expanded(
                child: TextField(
                  controller: _installUrlController,
                  decoration: InputDecoration(
                    isDense: true,
                    labelText: 'extensionInstallUrl'.tr,
                    hintText: 'https://github.com/author/repo',
                    border: const OutlineInputBorder(),
                  ),
                ),
              ),
              const SizedBox(width: 10),
              IconButtonLoading(
                controller: _installBtnController,
                onPressed: _doInstall,
                icon: const Icon(Icons.download),
              ),
              if (controller.devMode.value && Util.isDesktop()) ...[
                const SizedBox(width: 8),
                IconButton(
                  tooltip: 'extensionLoadLocal'.tr,
                  onPressed: _installFromFolder,
                  icon: const Icon(Icons.folder_open),
                ),
              ],
            ],
          );
  }

  Widget _buildMarketToolbar(BuildContext context) {
    final narrow = ResponsiveBuilder.isNarrow(context);
    InputDecoration decoration() {
      return const InputDecoration(
        hintText: '搜索扩展...',
        hintStyle: TextStyle(fontSize: 13),
      );
    }

    return narrow
        ? Column(
            children: [
              TextField(
                controller: _searchController,
                textInputAction: TextInputAction.search,
                onSubmitted: controller.searchStore,
                decoration: decoration().copyWith(
                  suffixIcon: IconButton(
                    onPressed: () =>
                        controller.searchStore(_searchController.text),
                    icon: const Icon(Icons.search_rounded),
                  ),
                ),
              ),
              const SizedBox(height: 8),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  Flexible(child: _buildSortTabs(context)),
                  const SizedBox(width: 6),
                  IconButton(
                    tooltip: 'update'.tr,
                    onPressed: controller.refreshStore,
                    icon: const Icon(Icons.refresh),
                  ),
                ],
              ),
            ],
          )
        : Column(
            children: [
              Row(
                children: [
                  Expanded(
                    child: TextField(
                      controller: _searchController,
                      textInputAction: TextInputAction.search,
                      onSubmitted: controller.searchStore,
                      decoration: decoration().copyWith(
                        suffixIcon: IconButton(
                          onPressed: () =>
                              controller.searchStore(_searchController.text),
                          icon: const Icon(Icons.search_rounded),
                        ),
                      ),
                    ),
                  ),
                  const SizedBox(width: 8),
                  _buildSortTabs(context),
                  IconButton(
                    tooltip: 'update'.tr,
                    onPressed: controller.refreshStore,
                    icon: const Icon(Icons.refresh),
                  ),
                ],
              ),
            ],
          );
  }

  Widget _buildSortTabs(BuildContext context) {
    Widget tab(StoreExtensionSort sort, String label) {
      final selected = controller.storeSort.value == sort;
      return InkWell(
        borderRadius: BorderRadius.circular(7),
        onTap: () => controller.changeSort(sort),
        child: AnimatedContainer(
          duration: const Duration(milliseconds: 150),
          padding: const EdgeInsets.symmetric(horizontal: 9, vertical: 6),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(7),
            color: selected
                ? Theme.of(context).colorScheme.primary.withValues(alpha: 0.12)
                : Colors.transparent,
          ),
          child: Text(
            label,
            style: TextStyle(
              fontWeight: selected ? FontWeight.w600 : FontWeight.w500,
              fontSize: 12,
              height: 1.0,
              color: selected
                  ? Theme.of(context).colorScheme.primary
                  : Theme.of(context)
                      .textTheme
                      .bodyMedium
                      ?.color
                      ?.withValues(alpha: 0.82),
            ),
          ),
        ),
      );
    }

    return Container(
      constraints: const BoxConstraints(maxWidth: 310),
      padding: const EdgeInsets.all(2),
      decoration: BoxDecoration(
        color: Theme.of(context).colorScheme.surface.withValues(alpha: 0.35),
        border: Border.all(
          color: Theme.of(context).dividerColor.withValues(alpha: 0.45),
        ),
        borderRadius: BorderRadius.circular(8),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          tab(StoreExtensionSort.stars, 'extensionSortStars'.tr),
          tab(StoreExtensionSort.installs, 'extensionSortInstalls'.tr),
          tab(StoreExtensionSort.updated, 'extensionSortUpdated'.tr),
        ],
      ),
    );
  }

  Widget _buildFilterBar(BuildContext context) {
    final narrow = ResponsiveBuilder.isNarrow(context);
    Widget option(ExtensionListFilter filter, String text) {
      final selected = _filter == filter;
      return InkWell(
        onTap: () => controller.changeFilter(filter),
        borderRadius: BorderRadius.circular(8),
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(8),
            color: selected
                ? Get.theme.colorScheme.primary.withValues(alpha: 0.14)
                : Colors.transparent,
          ),
          child: Text(
            text,
            style: TextStyle(
              color: selected ? Get.theme.colorScheme.primary : null,
              fontWeight: selected ? FontWeight.w600 : FontWeight.w500,
            ),
          ),
        ),
      );
    }

    final left = Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        option(ExtensionListFilter.market, 'extensionFilterMarket'.tr),
        const SizedBox(width: 8),
        option(ExtensionListFilter.installed, 'extensionFilterInstalled'.tr),
      ],
    );

    final right = Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        TextButton.icon(
          onPressed: controller.toggleInstallTools,
          icon: const Icon(Icons.link),
          label: Text('extensionManualInstall'.tr),
        ),
        const SizedBox(width: 8),
        TextButton.icon(
          onPressed: () =>
              launchUrl(Uri.parse('https://gopeed.com/docs/dev-extension')),
          icon: const Icon(Icons.menu_book_outlined),
          label: Text('extensionDevelop'.tr),
        ),
      ],
    );

    if (narrow) {
      return Wrap(
        spacing: 8,
        runSpacing: 6,
        alignment: WrapAlignment.spaceBetween,
        children: [left, right],
      );
    }

    return Row(
      children: [
        left,
        const Spacer(),
        right,
      ],
    );
  }

  Widget _buildUnifiedGrid(BuildContext context) {
    final items = !Util.isMacos()
        ? controller.displayItems
        : _showStore
            ? controller.storeExtensions
                .map((ext) => ExtensionListItem(
                    installed: controller.installedMap[ext.id], store: ext))
                .toList()
            : controller.installedExtensions
                .map((ext) => ExtensionListItem(installed: ext))
                .toList();
    final loading = controller.loadingInstalled.value ||
        (_filter == ExtensionListFilter.market &&
            controller.loadingStore.value);
    if (loading && items.isEmpty) {
      return const Center(child: CircularProgressIndicator());
    }
    if (Util.isMacos() &&
        _filter == ExtensionListFilter.market &&
        controller.storeError.value != null) {
      return Padding(
          padding: const EdgeInsets.symmetric(vertical: 36),
          child: MacEmptyState(
            icon: CupertinoIcons.exclamationmark_circle,
            title: '无法载入扩展商店',
            message: controller.storeError.value,
            action: MacButton(
                onPressed: controller.refreshStore, child: const Text('重试')),
          ));
    }
    if (items.isEmpty) {
      if (Util.isMacos()) {
        return Padding(
            padding: const EdgeInsets.symmetric(vertical: 36),
            child: MacEmptyState(
                icon: controller.storeQuery.value.isEmpty
                    ? CupertinoIcons.square_stack
                    : CupertinoIcons.search,
                title: _filter == ExtensionListFilter.installed
                    ? '还没有安装扩展'
                    : 'extensionStoreEmpty'.tr));
      }
      return Padding(
        padding: const EdgeInsets.symmetric(vertical: 24),
        child: Text('extensionStoreEmpty'.tr),
      );
    }

    if (Util.isMacos()) {
      return Column(
        mainAxisSize: MainAxisSize.min,
        children: items.map(_buildUnifiedCard).toList(),
      );
    }

    return LayoutBuilder(
      builder: (context, constraints) {
        final width = constraints.maxWidth;
        final crossAxisCount = width >= 1320
            ? 4
            : width >= 980
                ? 3
                : width >= 700
                    ? 2
                    : 1;
        const cardHeight = 180.0;
        final childAspectRatio = (width / crossAxisCount) / cardHeight;

        return GridView.builder(
          itemCount: items.length,
          shrinkWrap: true,
          physics: const NeverScrollableScrollPhysics(),
          gridDelegate: SliverGridDelegateWithFixedCrossAxisCount(
            crossAxisCount: crossAxisCount,
            childAspectRatio: childAspectRatio,
            crossAxisSpacing: 10,
            mainAxisSpacing: 10,
          ),
          itemBuilder: (context, index) => _buildUnifiedCard(items[index]),
        );
      },
    );
  }

  Widget _buildUnifiedCard(ExtensionListItem item) {
    final installed = item.installed;
    final store = item.store;
    final allowMaintenance = !Util.isMacos() || _maintenance;
    final canUpdate = allowMaintenance &&
        (_filter == ExtensionListFilter.market && store != null
            ? controller.canUpdateFromStore(store)
            : controller.canUpdateItem(item));
    final busy = controller.busyExtensionIds.contains(item.id);

    final card = ExtensionCard(
      item: item,
      busy: busy,
      canUpdate: canUpdate,
      onTap: store != null ? () => _showExtensionDrawer(item) : null,
      onToggle: installed == null
          ? null
          : (value) async {
              try {
                await controller.toggleExtension(installed, value);
              } catch (e) {
                showErrorMessage(e);
              }
            },
      onOpenSetting: installed != null && installed.settings?.isNotEmpty == true
          ? () => _showSettingDialog(installed)
          : null,
      onUpdate: installed != null && canUpdate
          ? () async {
              try {
                await controller.upgradeExtension(installed);
                showMessage('tip'.tr, 'extensionUpdateSuccess'.tr);
              } catch (e) {
                showErrorMessage(e);
              }
            }
          : null,
      onDelete: allowMaintenance && installed != null
          ? () => _showDeleteDialog(installed)
          : null,
      onInstall: !item.isInstalled && store != null
          ? () async {
              try {
                await controller.installFromStore(store);
                showMessage('tip'.tr, 'extensionInstallSuccess'.tr);
              } catch (e) {
                showErrorMessage(e);
              }
            }
          : null,
    );
    if (Util.isMacos() && !_maintenance && installed != null) {
      return ListTile(
        title:
            Text(installed.title, maxLines: 1, overflow: TextOverflow.ellipsis),
        subtitle: Text(
            'v${installed.version} · ${installed.disabled ? '已停用' : '已启用'}',
            style: const TextStyle(fontSize: 12)),
        trailing: Row(mainAxisSize: MainAxisSize.min, children: [
          if (busy)
            const SizedBox(
                width: 16,
                height: 16,
                child: CircularProgressIndicator(strokeWidth: 2)),
          Switch.adaptive(
              value: !installed.disabled,
              onChanged: busy ? null : card.onToggle),
          if (installed.settings?.isNotEmpty == true)
            IconButton(
              tooltip: '扩展配置',
              onPressed: busy ? null : card.onOpenSetting,
              icon: const Icon(CupertinoIcons.gear, size: 16),
            ),
        ]),
      );
    }
    return card;
  }

  Future<void> _showExtensionDrawer(ExtensionListItem item) async {
    final store = item.store;
    if (store == null) return;
    if (Util.isMacos()) {
      await showDialog<void>(
        context: Get.context!,
        builder: (dialogContext) => Dialog(
          clipBehavior: Clip.antiAlias,
          child: SizedBox(
            width: (MediaQuery.of(dialogContext).size.width - 48)
                .clamp(320.0, 660.0),
            height: MediaQuery.of(dialogContext).size.height * 0.78,
            child: ExtensionDetailDrawer(
              extension: store,
              installed: item.installed,
              onClose: () => Navigator.of(dialogContext).pop(),
            ),
          ),
        ),
      );
      return;
    }
    await showGeneralDialog(
      context: Get.context!,
      barrierDismissible: true,
      barrierLabel: 'close',
      barrierColor: Colors.black54,
      transitionDuration: const Duration(milliseconds: 180),
      pageBuilder: (context, animation, secondaryAnimation) {
        return Align(
          alignment: Alignment.centerRight,
          child: Material(
            child: SizedBox(
              width: (MediaQuery.of(context).size.width * 0.5)
                  .clamp(320.0, 1200.0),
              child: ExtensionDetailDrawer(
                extension: store,
                installed: item.installed,
                onClose: () => Navigator.of(context).pop(),
              ),
            ),
          ),
        );
      },
      transitionBuilder: (context, animation, secondaryAnimation, child) {
        final offset = Tween<Offset>(
          begin: const Offset(1, 0),
          end: Offset.zero,
        ).animate(animation);
        return SlideTransition(position: offset, child: child);
      },
    );
  }

  Future<void> _showSettingDialog(Extension extension) async {
    final formKey = GlobalKey<FormBuilderState>();
    final confrimController = RoundedLoadingButtonController();
    var saving = false;

    Future<void> saveSettings(BuildContext dialogContext) async {
      if (saving) return;
      saving = true;
      try {
        if (!Util.isMacos()) confrimController.start();
        if (formKey.currentState?.saveAndValidate() == true) {
          await updateExtensionSettings(
            extension.identity,
            UpdateExtensionSettings(settings: formKey.currentState!.value),
          );
          await controller.loadInstalled(refreshUpdates: false);
          if (dialogContext.mounted) Navigator.of(dialogContext).pop();
        }
      } catch (e) {
        showErrorMessage(e);
      } finally {
        if (!Util.isMacos()) confrimController.reset();
        saving = false;
      }
    }

    return showDialog<void>(
      context: Get.context!,
      barrierDismissible: false,
      builder: (dialogContext) => AlertDialog(
        title: Util.isMacos() ? Text('setting'.tr) : null,
        content: Builder(builder: (context) {
          final height = MediaQuery.of(context).size.height;
          final width = MediaQuery.of(context).size.width;

          return SizedBox(
            height: Util.isMacos() ? height * 0.58 : height * 0.75,
            width: Util.isMacos() ? (width - 80).clamp(300.0, 500.0) : width,
            child: FormBuilder(
              key: formKey,
              child: Column(children: [
                if (!Util.isMacos()) Text('setting'.tr),
                Expanded(
                  child: SingleChildScrollView(
                    child: Column(
                      children: extension.settings!.map((e) {
                        final settingItem = _buildSettingItem(e);

                        return Row(
                          crossAxisAlignment: CrossAxisAlignment.end,
                          children: [
                            SizedBox(
                              width: 20,
                              child: e.description.isEmpty
                                  ? null
                                  : Tooltip(
                                      message: e.description,
                                      child: const CircleAvatar(
                                        radius: 10,
                                        backgroundColor: Colors.grey,
                                        child:
                                            Icon(Icons.question_mark, size: 10),
                                      ),
                                    ),
                            ).paddingOnly(right: 10),
                            Expanded(child: settingItem),
                          ],
                        );
                      }).toList(),
                    ),
                  ),
                ),
              ]),
            ),
          );
        }),
        actions: Util.isMacos()
            ? [
                TextButton(
                  onPressed: () => Navigator.of(dialogContext).pop(),
                  child: Text('cancel'.tr),
                ),
                MacButton(
                  primary: true,
                  onPressed: () => saveSettings(dialogContext),
                  child: Text('confirm'.tr),
                ),
              ]
            : [
                ConstrainedBox(
                  constraints: BoxConstraints.tightFor(
                    width: Get.theme.buttonTheme.minWidth,
                    height: Get.theme.buttonTheme.height,
                  ),
                  child: ElevatedButton(
                    style:
                        ElevatedButton.styleFrom(shape: const StadiumBorder())
                            .copyWith(
                      backgroundColor: WidgetStateProperty.all(
                          Get.theme.colorScheme.surface),
                    ),
                    onPressed: () => Navigator.of(dialogContext).pop(),
                    child: Text('cancel'.tr),
                  ),
                ),
                ConstrainedBox(
                  constraints: BoxConstraints.tightFor(
                    width: Get.theme.buttonTheme.minWidth,
                    height: Get.theme.buttonTheme.height,
                  ),
                  child: RoundedLoadingButton(
                    color: Get.theme.colorScheme.secondary,
                    onPressed: () => saveSettings(dialogContext),
                    controller: confrimController,
                    child: Text('confirm'.tr),
                  ),
                ),
              ],
      ),
    );
  }

  Widget _buildSettingItem(Setting setting) {
    final requiredValidator =
        setting.required ? FormBuilderValidators.required() : null;

    Widget buildTextField(TextInputFormatter? inputFormatter,
        FormFieldValidator<String>? validator, TextInputType? keyBoardType) {
      return FormBuilderTextField(
        name: setting.name,
        decoration: InputDecoration(labelText: setting.title),
        initialValue: setting.value?.toString(),
        inputFormatters: inputFormatter != null ? [inputFormatter] : null,
        keyboardType: keyBoardType,
        validator: FormBuilderValidators.compose([
          requiredValidator,
          validator,
        ].where((e) => e != null).map((e) => e!).toList()),
      );
    }

    Widget buildDropdown() {
      return FormBuilderDropdown<String>(
        name: setting.name,
        decoration: InputDecoration(labelText: setting.title),
        initialValue: setting.value?.toString(),
        validator: FormBuilderValidators.compose([
          requiredValidator,
        ].where((e) => e != null).map((e) => e!).toList()),
        items: setting.options!
            .map((e) => DropdownMenuItem(
                  value: e.value.toString(),
                  child: Text(e.label),
                ))
            .toList(),
      );
    }

    switch (setting.type) {
      case SettingType.string:
        return setting.options?.isNotEmpty == true
            ? buildDropdown()
            : buildTextField(null, null, null);
      case SettingType.number:
        return setting.options?.isNotEmpty == true
            ? buildDropdown()
            : buildTextField(
                FilteringTextInputFormatter.allow(RegExp(r'^\d+\.?\d*')),
                FormBuilderValidators.numeric(),
                const TextInputType.numberWithOptions(decimal: true),
              );
      case SettingType.boolean:
        return FormBuilderSwitch(
          name: setting.name,
          initialValue: (setting.value as bool?) ?? false,
          title: Text(setting.title),
          validator: requiredValidator,
        );
    }
  }

  void _showDeleteDialog(Extension extension) {
    showDialog(
      context: Get.context!,
      barrierDismissible: false,
      builder: (dialogContext) => AlertDialog(
        title: Text('extensionDelete'.tr),
        actions: [
          TextButton(
            child: Text('cancel'.tr),
            onPressed: () => Navigator.of(dialogContext).pop(),
          ),
          TextButton(
            child: Text(
              'confirm'.tr,
              style: const TextStyle(color: Colors.redAccent),
            ),
            onPressed: () async {
              try {
                await controller.removeExtension(extension);
                if (dialogContext.mounted) {
                  Navigator.of(dialogContext).pop();
                }
              } catch (e) {
                showErrorMessage(e);
              }
            },
          ),
        ],
      ),
    );
  }
}
