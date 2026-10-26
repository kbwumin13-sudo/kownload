import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:get/get.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../../i18n/message.dart';
import '../../../../macos/mac_onboarding.dart';
import '../../../../macos/mac_panel.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../../util/locale_manager.dart';
import '../../../../util/package_info.dart';
import '../../../../util/updater.dart';
import '../../../views/directory_selector.dart';
import '../../app/controllers/app_controller.dart';
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

  @override
  void initState() {
    super.initState();
    _directory =
        TextEditingController(text: _app.downloaderConfig.value.downloadDir);
  }

  @override
  void dispose() {
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
      Obx(() {
        final latest = _settings.latestVersion.value;
        return _group(context, '关于', [
          _row(context, 'kownload',
              detail: '版本 ${packageInfo.version}',
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
    void toggle() => _change(() => change(!value));
    return _row(context, title,
        control: MergeSemantics(
          child: Semantics(
            label: title,
            child: SizedBox(
              width: 44,
              height: 32,
              child: GestureDetector(
                behavior: HitTestBehavior.opaque,
                onTap: toggle,
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
                      onChanged: (value) => _change(() => change(value)),
                    ),
                  ),
                ),
              ),
            ),
          ),
        ));
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
