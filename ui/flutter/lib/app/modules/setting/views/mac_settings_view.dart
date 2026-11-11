import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:get/get.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../../api/model/downloader_config.dart';
import '../../../../core/common/start_config.dart';
import '../../../routes/app_pages.dart';
import '../../../../i18n/message.dart';
import '../../../../identity/browser_connection.dart';
import '../../../../identity/app_identity.dart';
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
import 'api_endpoint_draft.dart';
import 'bt_settings_draft.dart';

/// The macOS preferences expose everyday choices while preserving the full
/// downloader configuration used by the other platforms and download engine.
class MacSettingsView extends StatefulWidget {
  final Widget hotKeyEditor;
  final Widget Function(String section, Rx<DownloaderConfig> draft,
      Rx<StartConfig> startDraft) editorBuilder;

  const MacSettingsView(
      {super.key, required this.hotKeyEditor, required this.editorBuilder});

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
  bool _updatingTrackers = false;
  bool _btEditing = false;
  String? _trackerNotice;
  static const _browserNames = {'chrome': 'Chrome', 'edge': 'Edge'};
  final _reports = <String, BrowserConnectionReport>{};
  bool _diagnosticOpen = false;
  final _inspectionRevision = <String, int>{};
  final _reportErrors = <String, String>{};
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
    for (final browser in _browserNames.keys) {
      final revision = (_inspectionRevision[browser] ?? 0) + 1;
      _inspectionRevision[browser] = revision;
      try {
        final report = await BrowserConnection.inspect(browser);
        if (mounted && _inspectionRevision[browser] == revision) {
          setState(() {
            _reports[browser] = report;
            _reportErrors.remove(browser);
          });
        }
      } catch (_) {
        if (mounted && _inspectionRevision[browser] == revision) {
          setState(() => _reportErrors[browser] = '磁盘检查失败，请重试。');
        }
      }
    }
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

  String _routeText(String browser) {
    if (_reportErrors.containsKey(browser)) {
      return '${_reportErrors[browser]} · Native 未测试';
    }
    final report = _reports[browser];
    if (report == null) return '正在读取磁盘注册状态 · Native 未测试';
    return '${report.configured ? '新名称的磁盘注册可用' : '新名称的磁盘注册未就绪'} · Native 未测试';
  }

  String _evidenceText(ManifestEvidence evidence) {
    final owner = switch (evidence.owner) {
      'this_app' => '本应用',
      'foreign' => '其他程序，保留原文件',
      'missing' => '缺失',
      _ => '归属未知，保留原文件',
    };
    final executable = switch (evidence.executable) {
      'current_asset' => '与当前应用资源匹配',
      'mismatch' => '与当前应用不匹配',
      _ => '不可验证',
    };
    return '配置文件：${evidence.present ? '存在' : '缺失'} · 归属：$owner\n'
        '结构：${!evidence.present ? '未检测' : evidence.schemaValid ? '符合' : '不符合'} · '
        '扩展来源：${!evidence.present ? '未检测' : evidence.allowedOrigin ? '符合允许列表' : '不匹配'}\n'
        '宿主文件：$executable';
  }

  Future<void> _showBrowserDiagnostics(String browser) async {
    final name = _browserNames[browser];
    if (name == null || _diagnosticOpen) return;
    _diagnosticOpen = true;
    _inspectionRevision[browser] = (_inspectionRevision[browser] ?? 0) + 1;
    try {
      final current = await BrowserConnection.inspect(browser);
      if (!mounted) {
        _diagnosticOpen = false;
        return;
      }
      setState(() {
        _reports[browser] = current;
        _reportErrors.remove(browser);
      });
    } catch (_) {
      if (!mounted) {
        _diagnosticOpen = false;
        return;
      }
      setState(() => _reportErrors[browser] = '当前磁盘状态无法读取；上次结果不能代表当前状态。');
    }
    var report = _reports[browser];
    String? notice = _reportErrors[browser];
    var busy = false;
    await MacPanel.transient(() => showDialog<void>(
          context: context,
          builder: (dialogContext) =>
              StatefulBuilder(builder: (context, update) {
            Future<void> perform({bool register = false}) async {
              if (busy) return;
              if (register) {
                final confirmed = await showDialog<bool>(
                    context: dialogContext,
                    builder: (confirmContext) => AlertDialog(
                          title: Text('为 $name 注册新名称？'),
                          content: Text(
                              '仅写入 $name 的 ${AppIdentity.hostName} 清单，并先备份原状态。其他浏览器与旧名称 ${AppIdentity.legacyHostName} 均不修改。归属其他程序或无法确认的清单会保留；完成磁盘注册仍不代表下载助手已连接。'),
                          actions: [
                            TextButton(
                                onPressed: () =>
                                    Navigator.of(confirmContext).pop(false),
                                child: const Text('取消')),
                            TextButton(
                                onPressed: () =>
                                    Navigator.of(confirmContext).pop(true),
                                child: const Text('注册'))
                          ],
                        ));
                if (confirmed != true || !dialogContext.mounted) return;
              }
              update(() {
                busy = true;
                notice = null;
              });
              try {
                final next = register
                    ? await BrowserConnection.connectOne(browser,
                        legacyCompatibility: false)
                    : await BrowserConnection.inspect(browser);
                if (!dialogContext.mounted || !mounted) return;
                setState(() {
                  _reports[browser] = next;
                  _reportErrors.remove(browser);
                });
                update(() {
                  report = next;
                  if (register) {
                    notice = next.configured
                        ? '磁盘注册已准备好。注册不会改变助手现有资料绑定。请在此浏览器中检查助手；Native、App 和 Core 连接仍未测试。'
                        : '注册未就绪。请检查清单归属和宿主文件；无法确认的现有项会保留，不自动覆盖。';
                  }
                });
              } catch (_) {
                if (mounted) {
                  setState(() => _reportErrors[browser] = '当前结果未确认，请重新检查。');
                }
                if (dialogContext.mounted) {
                  update(() => notice = register
                      ? '注册失败。请检查本应用数据目录及浏览器清单目录的权限、宿主文件来源后重试；不强制覆盖其他程序。'
                      : '磁盘检查失败。上次结果不能代表当前状态，请检查目录权限后重试。');
                }
              } finally {
                if (dialogContext.mounted) update(() => busy = false);
              }
            }

            final current = report;
            final conflict = current != null &&
                !const ['this_app', 'missing'].contains(current.newHost.owner);
            return PopScope(
                canPop: !busy,
                child: AlertDialog(
                  key: ValueKey('browser-diagnostics-$browser'),
                  title: Text('$name 浏览器助手诊断'),
                  content: SizedBox(
                      width: 520,
                      child: SingleChildScrollView(
                          child: Column(
                              crossAxisAlignment: CrossAxisAlignment.start,
                              mainAxisSize: MainAxisSize.min,
                              children: [
                            const Text(
                                '本页只检查本机磁盘注册，不创建下载或读取账号。清单可用不能证明浏览器实际会话已连接。',
                                style: TextStyle(fontSize: 12)),
                            if (Util.isIsolatedPreview)
                              const Text('独立预览：只检查或写入指定的临时目录。',
                                  style: TextStyle(fontSize: 12)),
                            if (current != null) ...[
                              const SizedBox(height: 12),
                              const Text('新名称',
                                  style:
                                      TextStyle(fontWeight: FontWeight.w600)),
                              Text(current.newHost.hostName),
                              Text(_evidenceText(current.newHost),
                                  key: ValueKey('browser-new-host-$browser'),
                                  style: const TextStyle(fontSize: 12)),
                              const SizedBox(height: 12),
                              const Text('旧名称（独立保留）',
                                  style:
                                      TextStyle(fontWeight: FontWeight.w600)),
                              Text(current.legacyAlias.hostName),
                              Text(_evidenceText(current.legacyAlias),
                                  key: ValueKey('browser-legacy-host-$browser'),
                                  style: const TextStyle(fontSize: 12)),
                              if (current.issues.contains('route_mismatch'))
                                const Text(
                                    '助手绑定与当前注册不一致。请在对应浏览器中检查助手绑定，并按其提示重新加载；不自动清除助手数据或迁移未决操作。',
                                    style: TextStyle(fontSize: 12)),
                              if (current.issues.isNotEmpty ||
                                  current.newHost.issues.isNotEmpty ||
                                  current.legacyAlias.issues.isNotEmpty)
                                const Text(
                                    '有未就绪项：请核对清单格式、允许的扩展来源和宿主文件。其他程序或归属未知的现有文件会保留。',
                                    style: TextStyle(fontSize: 12)),
                            ] else
                              const Text('磁盘状态尚未取得，请点“重新检查”。'),
                            const SizedBox(height: 12),
                            const Text(
                                'Native 实际会话：未测试\nApp IPC：未测试\nCore 交接能力：未检测\n当前资料身份、绑定冲突及当前权限：未检测',
                                key: ValueKey('browser-live-unknown'),
                                style: TextStyle(fontSize: 12)),
                            if (conflict)
                              const Text(
                                  '新名称归属其他程序或未知，已禁止覆盖。请先在对应程序中处理，或核实来源后再检查；旧名称不受本操作影响。',
                                  style: TextStyle(fontSize: 12)),
                            if (notice != null)
                              Text(notice!,
                                  key: ValueKey('browser-notice-$browser'),
                                  style: const TextStyle(fontSize: 12)),
                          ]))),
                  actions: [
                    TextButton(
                        key: ValueKey('browser-inspect-$browser'),
                        onPressed: busy ? null : () => perform(),
                        child: Text(busy ? '正在处理…' : '重新检查')),
                    TextButton(
                        key: ValueKey('browser-register-$browser'),
                        onPressed: busy || current == null || conflict
                            ? null
                            : () => perform(register: true),
                        child: Text('注册 $name…')),
                    TextButton(
                        onPressed: busy
                            ? null
                            : () => Navigator.of(dialogContext).pop(),
                        child: const Text('完成')),
                  ],
                ));
          }),
        )).whenComplete(() => _diagnosticOpen = false);
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
        // Legacy advanced links keep the existing editors reachable in Download.
        final download = const ['download', 'advanced', 'connections']
            .contains(requestedSection);
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
              constraints: const BoxConstraints(maxWidth: 640),
              child: ListView(
                key: ValueKey(
                    'mac-settings-${download ? 'download' : 'general'}'),
                padding: const EdgeInsets.fromLTRB(20, 20, 20, 28),
                children: [
                  Padding(
                    padding: const EdgeInsets.only(bottom: 20),
                    child: Row(children: [
                      Image.asset('assets/icon/icon_macos_1024.png',
                          width: 40, height: 40),
                      const SizedBox(width: 12),
                      Expanded(
                        child: Column(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Text(download ? '下载设置' : '通用设置',
                                  style: TextStyle(
                                      fontSize: 20,
                                      fontWeight: FontWeight.w600,
                                      color: c.text)),
                              const SizedBox(height: 4),
                              Text(download ? '保存位置与下载行为' : '外观、浏览器助手与使用习惯',
                                  style: TextStyle(
                                      fontSize: 12, color: c.secondary)),
                            ]),
                      ),
                    ]),
                  ),
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
            for (final entry in _browserNames.entries)
              _row(context, '${entry.value} 注册与诊断',
                  detail: _routeText(entry.key),
                  control: MacButton(
                      key: ValueKey('browser-open-${entry.key}'),
                      onPressed: () => _showBrowserDiagnostics(entry.key),
                      child: const Text('检查…'))),
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
        ? '尚未收到助手上报；当前权限和连接未检测。'
        : '最近一次上报（本机时间） ${status.seen.toLocal().toIso8601String().split('.').first.replaceFirst('T', ' ')}'
            ' · 助手版本 ${status.version.isEmpty ? '未知' : status.version}'
            ' · 下载权限：${status.downloadsPermission ? '上报为允许' : '未确认'}；当前权限和连接未检测。';
    return [
      _row(context, '偏好所属浏览器',
          detail: '按助手保存的客户端分别保留；当前浏览器资料身份未验证。',
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

  Future<void> _editSection(String section, String title) async {
    if (_saving || (section == 'bt' && (_updatingTrackers || _btEditing))) {
      return;
    }
    if (section == 'bt') setState(() => _btEditing = true);
    _settings.clearTap();
    final draft =
        DownloaderConfig.fromJson(_app.downloaderConfig.value.toJson()).obs;
    final btEdits = section == 'bt' ? BtSettingsDraft(draft.value) : null;
    var saving = false;
    String? error;
    final start = _app.startConfig.value;
    final originalApi = (start.network, start.address, start.apiToken);
    final startDraft = (StartConfig()
          ..network = start.network
          ..address = start.address
          ..apiToken = start.apiToken)
        .obs;
    final editor = widget.editorBuilder(section, draft, startDraft);
    await MacPanel.transient(() => showDialog<void>(
        context: context,
        builder: (dialogContext) => StatefulBuilder(builder: (context, update) {
              return PopScope(
                canPop: !saving,
                child: AlertDialog(
                  key: ValueKey('settings-editor-$section'),
                  title: Text(title),
                  content: SizedBox(
                    width: 600,
                    child: SingleChildScrollView(
                        child: Column(
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        AbsorbPointer(absorbing: saving, child: editor),
                        if (error != null)
                          Text(error!,
                              style: TextStyle(
                                  color: Theme.of(context).colorScheme.error)),
                      ],
                    )),
                  ),
                  actions: [
                    TextButton(
                        onPressed: saving
                            ? null
                            : () => Navigator.of(dialogContext).pop(),
                        child: const Text('取消')),
                    TextButton(
                        onPressed: saving
                            ? null
                            : () async {
                                final proposed = startDraft.value;
                                if (section == 'api' &&
                                    (
                                          proposed.network,
                                          proposed.address,
                                          proposed.apiToken
                                        ) ==
                                        originalApi) {
                                  Navigator.of(dialogContext).pop();
                                  return;
                                }
                                update(() {
                                  saving = true;
                                  error = null;
                                });
                                if (section == 'api') {
                                  var invalid =
                                      ApiEndpointDraft.validationError(
                                          proposed.network, proposed.address);
                                  if (invalid == null &&
                                      proposed.network == 'unix') {
                                    invalid = ApiEndpointDraft.unixTargetError(
                                        address: proposed.address,
                                        expectedAddress:
                                            await Util.homePathJoin(
                                                AppIdentity.apiSocket),
                                        storageRoot: Util.getStorageDir());
                                  }
                                  if (!dialogContext.mounted) return;
                                  if (invalid != null) {
                                    update(() {
                                      saving = false;
                                      error = invalid;
                                    });
                                    return;
                                  }
                                }
                                final config = _app.downloaderConfig.value;
                                final restore = _applySection(
                                    config,
                                    draft.value,
                                    section,
                                    startDraft.value,
                                    btEdits);
                                try {
                                  if (section == 'api') {
                                    await _app.saveStartConfig();
                                  } else {
                                    await _app.saveConfig();
                                  }
                                  _app.downloaderConfig.refresh();
                                  if (dialogContext.mounted) {
                                    Navigator.of(dialogContext).pop();
                                  }
                                  if (mounted) setState(() => _saved = true);
                                } catch (_) {
                                  restore();
                                  if (dialogContext.mounted) {
                                    update(() {
                                      saving = false;
                                      error = section == 'api'
                                          ? '启动设置保存未完成，请重试。'
                                          : '更改尚未保存，请检查下载服务后重试。';
                                    });
                                  }
                                }
                              },
                        child: Text(saving ? '正在保存…' : '保存')),
                  ],
                ),
              );
            }))).whenComplete(() {
      if (section == 'bt' && mounted) setState(() => _btEditing = false);
    });
    _settings.clearTap();
  }

  // Apply only the selected section. BT patches changed user fields in place;
  // generated/unknown fields remain current, including on a failed save.
  VoidCallback _applySection(DownloaderConfig config, DownloaderConfig draft,
      String section, StartConfig startDraft, BtSettingsDraft? btEdits) {
    switch (section) {
      case 'media':
        final previous = config.protocolConfig.media;
        config.protocolConfig.media = draft.protocolConfig.media;
        return () => config.protocolConfig.media = previous;
      case 'http':
        final previous = config.protocolConfig.http;
        config.protocolConfig.http = draft.protocolConfig.http;
        return () => config.protocolConfig.http = previous;
      case 'bt':
        return btEdits!.apply(config, draft);
      case 'ed2k':
        final previous = config.protocolConfig.ed2k;
        config.protocolConfig.ed2k = draft.protocolConfig.ed2k;
        return () => config.protocolConfig.ed2k = previous;
      case 'proxy':
        final previous = config.proxy;
        config.proxy = draft.proxy;
        return () => config.proxy = previous;
      case 'api':
        final current = _app.startConfig.value;
        final network = current.network,
            address = current.address,
            token = current.apiToken;
        current.network = startDraft.network;
        current.address = startDraft.address;
        current.apiToken = startDraft.apiToken;
        return () {
          current.network = network;
          current.address = address;
          current.apiToken = token;
        };
      case 'webhook':
        final current = config.webhook;
        final enable = current.enable, urls = current.urls;
        current.enable = draft.webhook.enable;
        current.urls = List.of(draft.webhook.urls);
        return () {
          current.enable = enable;
          current.urls = urls;
        };
      case 'script':
        final current = config.script;
        final enable = current.enable, paths = current.paths;
        current.enable = draft.script.enable;
        current.paths = List.of(draft.script.paths);
        return () {
          current.enable = enable;
          current.paths = paths;
        };
      case 'mirrors':
        final current = config.extra.githubMirror;
        final enabled = current.enabled, mirrors = current.mirrors;
        current.enabled = draft.extra.githubMirror.enabled;
        current.mirrors = List.of(draft.extra.githubMirror.mirrors);
        return () {
          current.enabled = enabled;
          current.mirrors = mirrors;
        };
      default:
        throw StateError('Unknown settings section: $section');
    }
  }

  Future<void> _updateTrackers() async {
    if (_updatingTrackers || _btEditing || _saving) return;
    setState(() {
      _updatingTrackers = true;
      _trackerNotice = null;
    });
    try {
      await _app.trackerUpdate();
      if (mounted) {
        setState(() => _trackerNotice = '更新操作已结束，可在 BitTorrent 设置中查看更新时间。');
      }
    } catch (_) {
      if (mounted) setState(() => _trackerNotice = '未能更新 Tracker，请检查网络后重试。');
    } finally {
      if (mounted) setState(() => _updatingTrackers = false);
    }
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
      _group(context, '连接与工具', [
        ExpansionTile(
          key: const ValueKey('settings-advanced'),
          title: const Text('协议、媒体与代理'),
          children: [
            for (final entry in const {
              'media': '媒体下载工具',
              'http': 'HTTP',
              'bt': 'BitTorrent',
              'ed2k': 'ED2K',
              'proxy': '代理',
              'api': 'API',
              'webhook': 'Webhook',
              'script': '完成脚本',
              'mirrors': 'GitHub 镜像',
            }.entries)
              _row(context, entry.value,
                  control: MacButton(
                      key: ValueKey('settings-${entry.key}'),
                      onPressed:
                          _saving || (entry.key == 'bt' && _updatingTrackers)
                              ? null
                              : () => _editSection(entry.key, entry.value),
                      child: const Text('编辑…'))),
            _row(context, 'BT Tracker',
                detail: _trackerNotice ?? '使用已保存的订阅，点击后才连接网络更新。',
                control: MacButton(
                    key: const ValueKey('settings-tracker-update'),
                    onPressed: _updatingTrackers || _btEditing || _saving
                        ? null
                        : _updateTrackers,
                    child: Text(_updatingTrackers ? '正在更新…' : '立即更新'))),
            _row(context, '下载扩展',
                control: MacButton(
                    key: const ValueKey('settings-extensions'),
                    onPressed: () => Get.rootDelegate.toNamed(Routes.EXTENSION),
                    child: const Text('管理…'))),
          ],
        ),
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
      padding: const EdgeInsets.only(bottom: 20),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Padding(
          padding: const EdgeInsets.only(left: 12, bottom: 8),
          child: Text(title,
              style: TextStyle(
                  fontSize: 13, fontWeight: FontWeight.w600, color: c.text)),
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
      constraints: const BoxConstraints(minHeight: 48),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 12),
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
