import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:get/get.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../../i18n/message.dart';
import '../../../../identity/browser_connection.dart';
import '../../../../identity/native_manifests.dart';
import '../../../../macos/mac_onboarding.dart';
import '../../../../macos/mac_panel.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../../util/locale_manager.dart';
import '../../../../util/package_info.dart';
import '../../../../util/release_updates.dart';
import '../../../../util/util.dart';
import '../../../views/directory_selector.dart';
import '../../app/controllers/app_controller.dart';
import '../../../services/browser_integration.dart';
import '../controllers/setting_controller.dart';

/// The macOS preferences expose everyday choices while preserving the full
/// downloader configuration used by the other platforms and download engine.
class MacSettingsView extends StatefulWidget {
  final Widget hotKeyEditor;

  const MacSettingsView({super.key, required this.hotKeyEditor});

  @override
  State<MacSettingsView> createState() => _MacSettingsViewState();
}

class _MacSettingsViewState extends State<MacSettingsView> {
  final _app = Get.find<AppController>();
  final _settings = Get.find<SettingController>();
  late final TextEditingController _directory;
  String? _directoryError;
  String? _saveError;
  bool _saving = false;
  bool _saveRequested = false;
  bool _saved = false;
  Map<String, ManifestRoute?>? _routes;
  bool _connecting = false;
  String? _browserNotice;
  bool? _showInDock;
  bool _dockChanging = false;
  String? _dockNotice;
  String? _browserClientId;
  bool _browserSaving = false;
  String? _browserSaveError;
  final _browserGroupKey = GlobalKey();
  bool _browserFocused = false;

  @override
  void initState() {
    super.initState();
    _directory =
        TextEditingController(text: _app.downloaderConfig.value.downloadDir);
    _loadRoutes();
    _loadDock();
    _app.browserIntegration.addListener(_browserChanged);
  }

  void _browserChanged() {
    if (mounted) setState(() {});
  }

  Future<void> _changeBrowser(String clientId, Map patch) async {
    if (_browserSaving) return;
    setState(() {
      _browserSaving = true;
      _browserSaveError = null;
    });
    try {
      await _app.browserIntegration.update(clientId, patch);
    } catch (_) {
      if (mounted) setState(() => _browserSaveError = '更改未保存，请重试。');
    } finally {
      if (mounted) setState(() => _browserSaving = false);
    }
  }

  Future<void> _loadRoutes() async {
    try {
      final routes = await BrowserConnection.routes();
      if (mounted) setState(() => _routes = routes);
    } catch (_) {}
  }

  Future<void> _loadDock() async {
    final shown = await MacPanel.showInDock();
    if (mounted) setState(() => _showInDock = shown ?? false);
  }

  /// The host applies the change at once and keeps it for the next launch;
  /// the switch then shows what the host reports.
  Future<void> _changeDock(bool show) async {
    setState(() {
      _showInDock = show;
      _dockChanging = true;
      _dockNotice = null;
    });
    final shown = await MacPanel.setShowInDock(show);
    if (!mounted) return;
    setState(() {
      _showInDock = shown ?? !show;
      _dockChanging = false;
      if (shown != show) _dockNotice = '没有切换成功，请重试。';
    });
  }

  String _routeText() {
    if (Util.isIsolatedPreview) return '预览版只在临时目录生成清单，不连接浏览器。';
    final routes = _routes;
    if (routes == null) return '正在检查连接…';
    const names = {'chrome': 'Chrome', 'edge': 'Edge'};
    return [
      for (final entry in routes.entries)
        '${names[entry.key] ?? entry.key}：${switch (entry.value) {
          null => '未检测到',
          ManifestRoute.thisApp => '已连接本应用',
          ManifestRoute.elsewhere => '指向其他程序（如旧版）',
          ManifestRoute.none => '未连接',
        }}',
    ].join(' · ');
  }

  // Explicit and reversible: originals are backed up before the first change
  // and scripts/rollback-identity.py restores them.
  Future<void> _connectBrowsers() async {
    final confirmed = await MacPanel.transient(() => showDialog<bool>(
          context: context,
          builder: (dialogContext) => AlertDialog(
            title: const Text('连接浏览器助手'),
            content: const SizedBox(
              width: 440,
              child: Text(
                  '将在 Chrome 和 Edge 的 NativeMessagingHosts 目录写入 kownload 的连接清单，'
                  '并把旧名称 com.gopeed.gopeed 改为指向本应用。原文件会先私密备份到数据目录，'
                  '回退脚本可以恢复。已加载的下载助手不需要重新安装。'),
            ),
            actions: [
              TextButton(
                  onPressed: () => Navigator.of(dialogContext).pop(false),
                  child: const Text('取消')),
              TextButton(
                  onPressed: () => Navigator.of(dialogContext).pop(true),
                  child: const Text('连接')),
            ],
          ),
        ));
    if (confirmed != true || !mounted) return;
    setState(() {
      _connecting = true;
      _browserNotice = null;
    });
    String notice;
    try {
      final switched = await BrowserConnection.connect();
      notice = switched.isEmpty ? '没有检测到 Chrome 或 Edge。' : '已连接，下次在网页上点击下载时生效。';
    } on StateError catch (e) {
      notice = '没有连接：${e.message}';
    } catch (_) {
      notice = '没有连接：请检查浏览器目录的权限后重试。';
    }
    if (!mounted) return;
    setState(() {
      _connecting = false;
      _browserNotice = notice;
    });
    await _loadRoutes();
  }

  @override
  void dispose() {
    _app.browserIntegration.removeListener(_browserChanged);
    _directory.dispose();
    super.dispose();
  }

  void _change(VoidCallback change) {
    setState(change);
    _app.downloaderConfig.refresh();
    _save();
  }

  // Serialize writes so a slow earlier request cannot overwrite a newer choice.
  Future<void> _save() async {
    _saveRequested = true;
    if (_saving) return;
    setState(() {
      _saving = true;
      _saved = false;
      _saveError = null;
    });
    try {
      while (_saveRequested) {
        _saveRequested = false;
        await _app.saveConfig();
      }
      if (mounted) setState(() => _saved = true);
    } catch (_) {
      if (mounted) {
        setState(() => _saveError = '更改尚未保存，请检查下载服务后重试。');
      }
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  void _saveDirectory() {
    final path = _directory.text.trim();
    if (path.isEmpty) {
      setState(() => _directoryError = '请选择保存位置。');
      return;
    }
    setState(() => _directoryError = null);
    if (path != _app.downloaderConfig.value.downloadDir) {
      _change(() => _app.downloaderConfig.value.downloadDir = path);
    }
  }

  Future<void> _showGuide() async {
    final completed = await MacOnboarding.show(context, force: true);
    if (!mounted) return;
    // The guide can change the directory while this preferences view stays alive.
    _directory.text = _app.downloaderConfig.value.downloadDir;
    if (completed) {
      setState(() {
        _directoryError = null;
        _saveError = null;
        _saved = true;
      });
    }
  }

  Future<void> _open(String url) async {
    try {
      if (!await launchUrl(Uri.parse(url),
          mode: LaunchMode.externalApplication)) {
        throw StateError('无法打开链接');
      }
    } catch (_) {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(const SnackBar(content: Text('无法打开，请稍后重试。')));
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return ValueListenableBuilder<String>(
      valueListenable: MacPanel.settingsSection,
      builder: (context, requestedSection, _) {
        // Old deep links continue to land on ordinary preferences.
        final download = requestedSection == 'download';
        if (requestedSection != 'browser') _browserFocused = false;
        if (requestedSection == 'browser' && !_browserFocused) {
          WidgetsBinding.instance.addPostFrameCallback((_) {
            final target = _browserGroupKey.currentContext;
            if (mounted && target != null) {
              _browserFocused = true;
              Scrollable.ensureVisible(target);
            }
          });
        }
        return Scaffold(
          backgroundColor: c.background,
          body: Align(
            alignment: Alignment.topCenter,
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 600),
              child: ListView(
                key: ValueKey(
                    'mac-settings-${download ? 'download' : 'general'}'),
                padding: const EdgeInsets.fromLTRB(20, 24, 20, 28),
                children: [
                  if (_saveError != null)
                    Padding(
                      padding: const EdgeInsets.only(bottom: 16),
                      child: Row(children: [
                        Expanded(
                          child: Semantics(
                            liveRegion: true,
                            child: Text(_saveError!,
                                style: TextStyle(
                                    fontSize: 12,
                                    color:
                                        Theme.of(context).colorScheme.error)),
                          ),
                        ),
                        const SizedBox(width: 12),
                        MacButton(onPressed: _save, child: const Text('重试')),
                      ]),
                    ),
                  if (download) ..._download(context) else ..._general(context),
                  SizedBox(
                    height: 16,
                    child: Align(
                      alignment: Alignment.centerRight,
                      child: Text(
                        _saving ? '正在保存…' : (_saved ? '已保存' : ''),
                        style: TextStyle(fontSize: 11, color: c.secondary),
                      ),
                    ),
                  ),
                ],
              ),
            ),
          ),
        );
      },
    );
  }

  List<Widget> _general(BuildContext context) {
    final cfg = _app.downloaderConfig.value;
    final mode = ThemeMode.values.any((m) => m.name == cfg.extra.themeMode)
        ? cfg.extra.themeMode
        : 'system';
    final locale = messages.keys.containsKey(cfg.extra.locale)
        ? cfg.extra.locale
        : 'zh_CN';
    return [
      _group(context, '使用习惯', [
        _row(context, '启动与菜单栏',
            detail: '通过菜单栏图标或快捷键打开轻面板。',
            control: MacButton(
                onPressed: () => _open(
                    'x-apple.systempreferences:com.apple.LoginItems-Settings.extension'),
                child: const Text('登录项…'))),
        _row(context, '在程序坞中显示 kownload',
            detail: _dockNotice ?? '关闭时只在菜单栏显示，不出现在程序坞和 ⌘Tab 中。',
            control: _switch(context, '在程序坞中显示 kownload', _showInDock ?? false,
                _showInDock == null || _dockChanging ? null : _changeDock)),
        ListTileTheme(
          data: ListTileThemeData(
            dense: true,
            minVerticalPadding: 8,
            minTileHeight: 52,
            contentPadding: const EdgeInsets.symmetric(horizontal: 14),
            titleTextStyle:
                TextStyle(fontSize: 13, color: MacColors.of(context).text),
            subtitleTextStyle:
                TextStyle(fontSize: 11, color: MacColors.of(context).secondary),
          ),
          child: widget.hotKeyEditor,
        ),
        _toggle(context, '下载完成后通知', cfg.extra.desktopNotification,
            (value) => cfg.extra.desktopNotification = value),
      ]),
      _group(context, '外观与语言', [
        _row(context, '外观',
            control: _menu<String>(
                value: mode,
                width: 112,
                options: const {
                  'system': '跟随系统',
                  'light': '浅色',
                  'dark': '深色',
                },
                onChanged: (value) {
                  _change(() => cfg.extra.themeMode = value);
                  Get.changeThemeMode(ThemeMode.values.byName(value));
                })),
        _row(context, '语言',
            control: _menu<String>(
                value: locale,
                width: 176,
                options: {
                  for (final entry in messages.keys.entries)
                    entry.key: entry.value['label']!,
                },
                onChanged: (value) {
                  _change(() => cfg.extra.locale = value);
                  Get.updateLocale(toLocale(value));
                })),
      ]),
      KeyedSubtree(
          key: _browserGroupKey,
          child: _group(context, '浏览器助手', [
            _row(context, '浏览器连接',
                detail: _browserNotice == null
                    ? _routeText()
                    : '${_routeText()}\n$_browserNotice',
                control: MacButton(
                    onPressed: _connecting ? null : _connectBrowsers,
                    child: const Text('连接…'))),
            ..._browserPreferences(context),
          ])),
      Obx(() {
        final latest = _settings.latestVersion.value;
        return _group(context, '关于', [
          _row(context, 'kownload',
              detail: '版本 ${packageInfo.version}（${packageInfo.buildNumber}）',
              control: MacButton(
                  onPressed: () =>
                      _open('https://github.com/kbwumin13-sudo/kownload'),
                  child: const Text('项目主页'))),
          _toggle(context, '有新版本时提醒', cfg.extra.notifyWhenNewVersion,
              (value) => cfg.extra.notifyWhenNewVersion = value),
          if (latest != null)
            _row(context, '有新版本可用',
                control: MacButton(
                    onPressed: () => showUpdateDialog(context, latest),
                    child: const Text('查看更新'))),
          _row(context, '使用引导',
              detail: '添加下载、浏览器助手与网盘。',
              control: MacButton(
                  onPressed: _saving ? null : _showGuide,
                  child: const Text('重新打开…'))),
        ]);
      }),
    ];
  }

  List<Widget> _browserPreferences(BuildContext context) {
    final integration = _app.browserIntegration;
    final clients = integration.clients;
    if (clients.isEmpty) {
      return [
        _row(context, '下载接管与媒体嗅探',
            detail: '在 Chrome 或 Edge 打开下载助手并同步后，可在这里调整各自的偏好。')
      ];
    }
    final client = clients.where((c) => c.id == _browserClientId).firstOrNull ??
        clients.first;
    final prefs = client.preferences;
    final status = integration.statuses[client.id];
    final mode = !prefs.captureEnabled
        ? 'browser'
        : prefs.confirmDownloads
            ? 'confirm'
            : 'direct';
    final labels = <String, String>{};
    for (final browser in const ['chrome', 'edge']) {
      final matches = clients.where((c) => c.browser == browser).toList();
      for (var i = 0; i < matches.length; i++) {
        labels[matches[i].id] =
            '${matches[i].name}${matches.length > 1 ? ' · 资料 ${i + 1}' : ''}';
      }
    }
    final connection = status == null
        ? '尚未收到助手同步。'
        : '最近同步 ${status.seen.hour.toString().padLeft(2, '0')}:${status.seen.minute.toString().padLeft(2, '0')}'
            '${status.version.isEmpty ? '' : ' · 助手 ${status.version}'}'
            '${status.downloadsPermission ? '' : ' · 浏览器下载权限尚未授权'}';
    return [
      _row(context, '偏好所属浏览器',
          detail: '每个浏览器资料分别保留。',
          control: _menu<String>(
              value: client.id,
              options: labels,
              onChanged: (id) => setState(() => _browserClientId = id))),
      _row(context, '助手状态', detail: connection),
      _row(context, '普通下载',
          detail: '网页临时生成的文件等无法接管的下载仍由浏览器处理。',
          control: _menu<String>(
              value: mode,
              width: 166,
              options: const {
                'browser': '由浏览器下载',
                'confirm': '在 kownload 中确认',
                'direct': '直接交给 kownload'
              },
              onChanged: (value) {
                if (_browserSaving) return;
                _changeBrowser(client.id, {
                  'downloadCapture': {
                    'enabled': value != 'browser',
                    if (value != 'browser') 'confirm': value == 'confirm',
                  }
                });
              })),
      _row(context, '网页媒体嗅探',
          detail: '在网页播放器旁显示下载入口，保留视频、图片和音频候选。',
          control: _switch(
              context,
              '网页媒体嗅探',
              prefs.sniffEnabled,
              _browserSaving
                  ? null
                  : (value) => _changeBrowser(client.id, {
                        'sniffing': {'enabled': value}
                      }))),
      _row(context, '显示小图',
          detail: '在资源列表中保留小尺寸图片。',
          control: _switch(
              context,
              '显示小图',
              prefs.showAll,
              _browserSaving
                  ? null
                  : (value) => _changeBrowser(client.id, {
                        'sniffing': {'showAll': value}
                      }))),
      if (prefs.disabledSites.isNotEmpty)
        _row(context, '已关闭嗅探的网站',
            detail: '${prefs.disabledSites.length} 个网站',
            control: MacButton(
                onPressed:
                    _browserSaving ? null : () => _showDisabledSites(client),
                child: const Text('管理…'))),
      if (_browserSaving || _browserSaveError != null)
        _row(context, _browserSaveError ?? '正在保存浏览器偏好…'),
    ];
  }

  Future<void> _showDisabledSites(BrowserClient client) async {
    final host = await MacPanel.transient(() => showDialog<String>(
          context: context,
          builder: (dialogContext) => AlertDialog(
            title: const Text('已关闭嗅探的网站'),
            content: SizedBox(
                width: 360,
                child: ListView(shrinkWrap: true, children: [
                  for (final host in client.preferences.disabledSites.keys)
                    ListTile(
                        title: Text(host),
                        trailing: TextButton(
                            onPressed: () =>
                                Navigator.of(dialogContext).pop(host),
                            child: const Text('重新启用'))),
                ])),
            actions: [
              TextButton(
                  onPressed: () => Navigator.of(dialogContext).pop(),
                  child: const Text('完成'))
            ],
          ),
        ));
    if (host != null && mounted)
      await _changeBrowser(client.id, {
        'sniffing': {
          'disabledSites': {host: false}
        }
      });
  }

  List<Widget> _download(BuildContext context) {
    final cfg = _app.downloaderConfig.value;
    final counts = <int>{1, 2, 3, 4, 5, 8, 10, 16};
    if (cfg.maxRunning > 0) counts.add(cfg.maxRunning);
    final sortedCounts = counts.toList()..sort();
    return [
      _group(context, '保存与开始', [
        Padding(
          padding: const EdgeInsets.fromLTRB(14, 12, 14, 12),
          child:
              Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            const Text('保存位置', style: TextStyle(fontSize: 13)),
            const SizedBox(height: 8),
            DirectorySelector(
              controller: _directory,
              showLabel: false,
              allowEdit: true,
              inlineActions: true,
              onEditComplete: _saveDirectory,
            ),
            if (_directoryError != null)
              Padding(
                padding: const EdgeInsets.only(top: 6),
                child: Text(_directoryError!,
                    style: TextStyle(
                        fontSize: 12,
                        color: Theme.of(context).colorScheme.error)),
              ),
          ]),
        ),
        _row(context, '同时下载数量',
            control: _menu<int>(
                value: cfg.maxRunning > 0 ? cfg.maxRunning : null,
                width: 76,
                options: {for (final count in sortedCounts) count: '$count 个'},
                onChanged: (value) => _change(() => cfg.maxRunning = value))),
        _toggle(context, '添加后立即开始', cfg.extra.defaultDirectDownload,
            (value) => cfg.extra.defaultDirectDownload = value),
        _toggle(context, '打开应用时继续未完成的下载', cfg.extra.autoStartTasks,
            (value) => cfg.extra.autoStartTasks = value),
      ]),
      _group(context, '下载完成后', [
        _toggle(context, '自动解压压缩包', cfg.archive.autoExtract,
            (value) => cfg.archive.autoExtract = value),
        if (cfg.archive.autoExtract)
          _toggle(context, '解压成功后删除压缩包', cfg.archive.deleteAfterExtract,
              (value) => cfg.archive.deleteAfterExtract = value),
      ]),
      _group(context, '种子文件', [
        _toggle(context, '下载种子文件后自动添加任务', cfg.autoTorrent.enable,
            (value) => cfg.autoTorrent.enable = value),
        if (cfg.autoTorrent.enable)
          _toggle(context, '添加任务后删除种子文件', cfg.autoTorrent.deleteAfterDownload,
              (value) => cfg.autoTorrent.deleteAfterDownload = value),
        _row(context, '默认打开应用',
            detail: '在 Finder 的“显示简介”中，将种子文件的“打开方式”设为 kownload，再选择“全部更改”。'),
      ]),
    ];
  }

  Widget _group(BuildContext context, String title, List<Widget> rows) {
    final c = MacColors.of(context);
    return Padding(
      padding: const EdgeInsets.only(bottom: 22),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Padding(
          padding: const EdgeInsets.only(left: 14, bottom: 7),
          child: Text(title,
              style: TextStyle(
                  fontSize: 12,
                  fontWeight: FontWeight.w500,
                  color: c.secondary)),
        ),
        Container(
          decoration: BoxDecoration(
            color: c.surface,
            borderRadius: BorderRadius.circular(10),
            border: Border.all(color: c.border, width: .5),
          ),
          child: Column(children: [
            for (var i = 0; i < rows.length; i++) ...[
              rows[i],
              if (i < rows.length - 1)
                Divider(
                    height: .5,
                    thickness: .5,
                    indent: 14,
                    endIndent: 14,
                    color: c.border),
            ],
          ]),
        ),
      ]),
    );
  }

  Widget _row(BuildContext context, String title,
      {String? detail, Widget? control}) {
    final c = MacColors.of(context);
    return ConstrainedBox(
      constraints: const BoxConstraints(minHeight: 46),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 9),
        child: Row(children: [
          Expanded(
            child:
                Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text(title, style: TextStyle(fontSize: 13, color: c.text)),
              if (detail != null)
                Padding(
                  padding: const EdgeInsets.only(top: 3),
                  child: Text(detail,
                      style: TextStyle(
                          fontSize: 11, height: 1.35, color: c.secondary)),
                ),
            ]),
          ),
          if (control != null) ...[
            const SizedBox(width: 18),
            control,
          ],
        ]),
      ),
    );
  }

  Widget _toggle(BuildContext context, String title, bool value,
      ValueChanged<bool> change) {
    return _row(context, title,
        control: _switch(
            context, title, value, (value) => _change(() => change(value))));
  }

  /// A null [onChanged] shows the switch disabled.
  Widget _switch(BuildContext context, String title, bool value,
      ValueChanged<bool>? onChanged) {
    return MergeSemantics(
      child: Semantics(
        label: title,
        child: SizedBox(
          width: 44,
          height: 32,
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTap: onChanged == null ? null : () => onChanged(!value),
            child: OverflowBox(
              minWidth: 60,
              maxWidth: 60,
              minHeight: 40,
              maxHeight: 40,
              child: Transform.scale(
                scale: .64,
                child: Switch.adaptive(
                  activeTrackColor: MacColors.of(context).accent,
                  materialTapTargetSize: MaterialTapTargetSize.shrinkWrap,
                  value: value,
                  onChanged: onChanged,
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }

  Widget _menu<T>({
    required T? value,
    required Map<T, String> options,
    required ValueChanged<T> onChanged,
    double width = 160,
  }) {
    final c = MacColors.of(context);
    return Container(
      width: width,
      height: 28,
      padding: const EdgeInsets.symmetric(horizontal: 8),
      decoration: BoxDecoration(
        color: Color.alphaBlend(c.text.withValues(alpha: .025), c.surface),
        borderRadius: BorderRadius.circular(5),
        border: Border.all(color: c.border, width: .5),
      ),
      child: DropdownButton<T>(
        value: value,
        isDense: true,
        isExpanded: true,
        borderRadius: BorderRadius.circular(7),
        icon: Icon(CupertinoIcons.chevron_up_chevron_down,
            size: 11, color: c.secondary),
        underline: const SizedBox.shrink(),
        hint: const Text('请选择'),
        style: TextStyle(fontSize: 12, color: c.text),
        items: [
          for (final entry in options.entries)
            DropdownMenuItem(
                value: entry.key,
                child: Text(entry.value,
                    maxLines: 1, overflow: TextOverflow.ellipsis)),
        ],
        onChanged: (value) {
          if (value != null) onChanged(value);
        },
      ),
    );
  }
}
