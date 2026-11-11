import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:path/path.dart' as path;
import 'package:url_launcher/url_launcher.dart';

import '../app/modules/app/controllers/app_controller.dart';
import '../database/database.dart';
import 'mac_panel.dart';
import 'mac_widgets.dart';
import 'onboarding_checks.dart';

/// A one-time tutorial before the first download, also available from Settings.
class MacOnboarding {
  static const completedKey = 'macDownloadOnboarding.v1';
  static Future<bool>? _pending;
  static bool get completed => Database.instance.box.get(completedKey) == true;

  @visibleForTesting
  static String? helperDirectoryOverride;

  @visibleForTesting
  static OnboardingChecks? checksOverride;

  static String get _helperDirectory =>
      helperDirectoryOverride ??
      path.normalize(path.join(path.dirname(Platform.resolvedExecutable), '..',
          'Resources', 'browser-media-extension'));

  /// Returns false when cancelled, so the caller can stop this submission.
  static Future<bool> show(BuildContext context, {bool force = false}) {
    final pending = _pending;
    if (pending != null) return pending;
    if (!force && completed) {
      return Future.value(true);
    }
    MacPanel.cancelAutoHide();
    final operation = MacPanel.transient(() async {
      if (!context.mounted) return false;
      final previousHeight = MacPanel.height;
      final inPanel = MacPanel.presentation.value == 'panel';
      try {
        if (inPanel) await MacPanel.resize(520);
        if (!context.mounted) return false;
        return await showDialog<bool>(
              context: context,
              barrierDismissible: false,
              builder: (_) => _DownloadTutorial(previouslyCompleted: completed),
            ) ??
            false;
      } finally {
        if (inPanel && MacPanel.presentation.value == 'panel') {
          await MacPanel.resize(previousHeight);
        }
      }
    });
    _pending = operation;
    return operation.whenComplete(() => _pending = null);
  }
}

class _DownloadTutorial extends StatefulWidget {
  final bool previouslyCompleted;
  const _DownloadTutorial({required this.previouslyCompleted});

  @override
  State<_DownloadTutorial> createState() => _DownloadTutorialState();
}

class _DownloadTutorialState extends State<_DownloadTutorial> {
  late final AppController _app = Get.find<AppController>();
  late final _directory =
      TextEditingController(text: _app.downloaderConfig.value.downloadDir);
  late final _checks = MacOnboarding.checksOverride ?? OnboardingChecks();
  DirectoryCheck? _directoryCheck;
  String? _checkedDirectory;
  bool _checking = false;
  int _directoryRevision = 0;
  MediaTool? _runningTool;
  final _toolResults = <MediaTool, ToolCheck>{};
  bool get _busy =>
      _saving || _choosing || _explaining || _checking || _runningTool != null;

  @override
  void initState() {
    super.initState();
    _directory.addListener(() {
      if (mounted) {
        setState(() {
          _directoryRevision++;
          _checkedDirectory = null;
          _directoryCheck = null;
          _error = null;
        });
      }
    });
  }

  Future<void> _checkDirectory() async {
    if (_busy) return;
    final directory = _directory.text.trim();
    final revision = _directoryRevision;
    setState(() {
      _checking = true;
      _error = null;
    });
    try {
      final result = await _checks.checkDirectory(directory);
      if (mounted &&
          revision == _directoryRevision &&
          result.directory == directory) {
        setState(() {
          _directoryCheck = result;
          _checkedDirectory = result.writable ? directory : null;
        });
      }
    } catch (_) {
      if (mounted) setState(() => _error = '无法完成文件夹检查，请重试或选择其他位置。');
    } finally {
      if (mounted) setState(() => _checking = false);
    }
  }

  Future<void> _checkTool(MediaTool tool, String executable) async {
    if (_busy) return;
    setState(() => _runningTool = tool);
    try {
      final result = await _checks.checkTool(tool, executable);
      if (mounted) setState(() => _toolResults[tool] = result);
    } catch (_) {
      if (mounted) {
        setState(() => _toolResults[tool] =
            const ToolCheck(ToolCheckState.failed, '版本检测失败，请检查路径后重试。'));
      }
    } finally {
      if (mounted) setState(() => _runningTool = null);
    }
  }

  Widget _tools() {
    final media = _app.downloaderConfig.value.protocolConfig.media;
    final tools = {
      MediaTool.ytDlp: media.ytDlpPath,
      MediaTool.ffmpeg: media.ffmpegPath,
      MediaTool.node: media.jsRuntimePath ?? ''
    };
    return ExpansionTile(
      key: const ValueKey('onboarding-tools'),
      tilePadding: EdgeInsets.zero,
      title: const Text('媒体工具（可选）', style: TextStyle(fontSize: 12)),
      children: [
        const Text(
            '配置由内核按既有优先级解析；此处未取得内核有效路径。仅对你明确配置的绝对路径检测版本，不安装工具。配置可在“下载 → 连接与工具”中编辑。',
            style: TextStyle(fontSize: 11)),
        for (final entry in tools.entries)
          Padding(
              padding: const EdgeInsets.symmetric(vertical: 8),
              child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                        '${entry.key.label}：${entry.value.isEmpty ? '未配置自定义路径' : entry.value}',
                        style: const TextStyle(fontSize: 12)),
                    Text(_toolResults[entry.key]?.message ?? '有效路径及版本：未检测。',
                        key: ValueKey(
                            'onboarding-tool-result-${entry.key.name}'),
                        style: const TextStyle(fontSize: 11)),
                    Text(
                        '仅运行此文件 ${entry.key.versionArguments.join(' ')}，最多3秒、输出最多4 KiB；不验证媒体下载。',
                        style: const TextStyle(fontSize: 11)),
                    MacButton(
                        key: ValueKey('onboarding-tool-${entry.key.name}'),
                        onPressed: _busy || !path.isAbsolute(entry.value)
                            ? null
                            : () => _checkTool(entry.key, entry.value),
                        child: Text(
                            _runningTool == entry.key ? '正在检测…' : '检测此文件版本')),
                  ])),
      ],
    );
  }

  bool _saving = false;
  bool _choosing = false;
  bool _explaining = false;
  String? _error;

  @override
  void dispose() {
    _directory.dispose();
    super.dispose();
  }

  void _cancel() {
    if (!_busy) {
      Navigator.of(context).pop(false);
    }
  }

  Future<void> _showBrowserGuide() async {
    if (_busy) return;
    setState(() => _explaining = true);
    try {
      await MacPanel.transient(() => showDialog<void>(
          context: context, builder: (_) => const _BrowserDownloadGuide()));
    } finally {
      if (mounted) setState(() => _explaining = false);
    }
  }

  Future<void> _chooseDirectory() async {
    setState(() {
      _choosing = true;
      _error = null;
    });
    try {
      final selected = await MacPanel.chooseDirectory(
          title: '选择下载保存位置', initialDirectory: _directory.text.trim());
      if (mounted && selected != null) _directory.text = selected;
    } catch (_) {
      if (mounted) setState(() => _error = '无法选择文件夹，请重试或输入保存位置。');
    } finally {
      if (mounted) setState(() => _choosing = false);
    }
  }

  Future<void> _complete() async {
    if (_busy) return;
    final directory = _directory.text.trim();
    if (directory.isEmpty) {
      setState(() => _error =
          _directoryCheck?.state == DirectoryCheckState.empty
              ? null
              : '请选择下载保存位置。');
      return;
    }
    if (_checkedDirectory != directory) {
      setState(() => _error = '请先检查此文件夹的写入权限，再完成教程。');
      return;
    }
    setState(() {
      _saving = true;
      _error = null;
    });
    final config = _app.downloaderConfig.value;
    final previousDirectory = config.downloadDir;
    var directorySaved = false;
    try {
      if (!widget.previouslyCompleted || directory != previousDirectory) {
        config.downloadDir = directory;
        await _app.saveConfig();
        directorySaved = true;
      }
      // Preserve the existing v1 marker on an explicit upgrade review.
      if (!widget.previouslyCompleted) {
        await Database.instance.box.put(MacOnboarding.completedKey, true);
      }
      _app.downloaderConfig.refresh();
      if (mounted) Navigator.of(context).pop(true);
    } catch (_) {
      config.downloadDir = previousDirectory;
      var message = '保存失败，请重试。';
      if (directorySaved) {
        try {
          await _app.saveConfig();
        } catch (_) {
          message = '教程记录保存失败，恢复原目录失败。请重试保存。';
        }
      }
      if (mounted) setState(() => _error = message);
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    final busy = _busy;
    return PopScope(
      canPop: !busy,
      child: CallbackShortcuts(
        bindings: {
          const SingleActivator(LogicalKeyboardKey.escape): _cancel,
          const SingleActivator(LogicalKeyboardKey.enter): _complete,
        },
        child: Dialog(
          insetPadding:
              const EdgeInsets.symmetric(horizontal: 20, vertical: 16),
          backgroundColor: c.background,
          shape: RoundedRectangleBorder(
              borderRadius: BorderRadius.circular(12),
              side: BorderSide(color: c.border, width: .5)),
          child: ConstrainedBox(
            key: const ValueKey('onboarding-content'),
            constraints: const BoxConstraints(maxWidth: 440),
            child: Padding(
              padding: const EdgeInsets.all(24),
              child: Column(mainAxisSize: MainAxisSize.min, children: [
                Flexible(
                  child: SingleChildScrollView(
                    child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text('下载新手教程',
                              style: TextStyle(
                                  color: c.text,
                                  fontSize: 17,
                                  fontWeight: FontWeight.w500)),
                          const SizedBox(height: 6),
                          Text(
                              widget.previouslyCompleted
                                  ? '已完成原有教程。本次为手动复查，保留已有设置。'
                                  : '检查保存位置后继续；浏览器和媒体工具可稍后设置。',
                              style:
                                  TextStyle(color: c.secondary, fontSize: 12)),
                          const SizedBox(height: 20),
                          Container(
                            padding: const EdgeInsets.symmetric(
                                horizontal: 12, vertical: 8),
                            decoration: BoxDecoration(
                              color: c.surface,
                              borderRadius: BorderRadius.circular(8),
                              border: Border.all(color: c.border, width: .5),
                            ),
                            child: Row(children: [
                              Text('保存位置',
                                  style: TextStyle(
                                      fontSize: 12, color: c.secondary)),
                              const SizedBox(width: 8),
                              Expanded(
                                child: TextField(
                                  key: const ValueKey('onboarding-directory'),
                                  controller: _directory,
                                  enabled: !busy,
                                  autofocus: true,
                                  style: TextStyle(fontSize: 12, color: c.text),
                                  decoration: InputDecoration(
                                    isDense: true,
                                    filled: false,
                                    contentPadding: const EdgeInsets.symmetric(
                                        horizontal: 7, vertical: 7),
                                    hintText: '选择文件夹或输入路径',
                                    hintStyle: TextStyle(
                                        fontSize: 11, color: c.secondary),
                                    border: const OutlineInputBorder(
                                        borderSide: BorderSide.none),
                                    enabledBorder: const OutlineInputBorder(
                                        borderSide: BorderSide.none),
                                    focusedBorder: OutlineInputBorder(
                                      borderRadius: BorderRadius.circular(4),
                                      borderSide:
                                          BorderSide(color: c.accent, width: 1),
                                    ),
                                  ),
                                  onSubmitted: (_) => _complete(),
                                ),
                              ),
                              const SizedBox(width: 8),
                              MacButton(
                                  onPressed: busy ? null : _chooseDirectory,
                                  child: const Text('选择…')),
                            ]),
                          ),
                          const SizedBox(height: 8),
                          const Text(
                              '点击“检查写入权限”会在当前文件夹创建并删除一个检查专用临时文件。只在你确认后执行。',
                              style: TextStyle(fontSize: 11)),
                          MacButton(
                              key: const ValueKey('onboarding-check-directory'),
                              onPressed: busy ? null : _checkDirectory,
                              child: Text(_checking ? '正在检查…' : '检查写入权限')),
                          if (_directoryCheck != null)
                            Text(_directoryCheck!.message,
                                key: const ValueKey(
                                    'onboarding-directory-result'),
                                style: const TextStyle(fontSize: 11)),
                          _tools(),
                          const ExpansionTile(
                            key: ValueKey('onboarding-gatekeeper'),
                            tilePadding: EdgeInsets.zero,
                            title: Text('macOS 首次打开帮助',
                                style: TextStyle(fontSize: 12)),
                            children: [
                              Text(
                                  '此版本尚未经过 Apple 公证。如果系统提示无法验证开发者或无法检查恶意软件，先确认应用来源可信且未被篡改，再尝试打开应用，并在“系统设置 → 隐私与安全性”中按系统提示点“仍要打开”，再次确认“打开”。受管理设备可能不提供此选项。若提示会损坏电脑或应用已损坏，请停止并核实来源，不按此流程继续。',
                                  style: TextStyle(fontSize: 11)),
                              SelectableText(
                                  'Apple 官方说明：https://support.apple.com/zh-cn/102445',
                                  style: TextStyle(fontSize: 11)),
                            ],
                          ),
                          const SizedBox(height: 14),
                          Text('直接粘贴视频或网盘链接，会按需提示下一步。',
                              style: TextStyle(
                                  color: c.secondary,
                                  fontSize: 12,
                                  height: 1.4)),
                          const SizedBox(height: 8),
                          TextButton(
                              style: TextButton.styleFrom(
                                padding: EdgeInsets.zero,
                                alignment: Alignment.centerLeft,
                                minimumSize: const Size(0, 28),
                                tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                                textStyle: const TextStyle(fontSize: 12),
                              ),
                              onPressed: busy ? null : _showBrowserGuide,
                              child: const Text('设置浏览器下载…')),
                          const SizedBox(height: 4),
                          Text('可稍后设置。完成教程后不再自动显示。',
                              style:
                                  TextStyle(color: c.secondary, fontSize: 11)),
                          if (_error != null) ...[
                            const SizedBox(height: 12),
                            Semantics(
                              liveRegion: true,
                              child: Text(_error!,
                                  style: TextStyle(
                                      color:
                                          Theme.of(context).colorScheme.error,
                                      fontSize: 12)),
                            ),
                          ],
                        ]),
                  ),
                ),
                const SizedBox(height: 24),
                Row(mainAxisAlignment: MainAxisAlignment.end, children: [
                  MacButton(
                      onPressed: busy ? null : _cancel,
                      child: const Text('取消')),
                  const SizedBox(width: 8),
                  MacButton(
                      primary: true,
                      onPressed: busy ? null : _complete,
                      child: Text(_saving ? '正在保存…' : '完成并继续')),
                ]),
              ]),
            ),
          ),
        ),
      ),
    );
  }
}

class _BrowserDownloadGuide extends StatefulWidget {
  const _BrowserDownloadGuide();

  @override
  State<_BrowserDownloadGuide> createState() => _BrowserDownloadGuideState();
}

class _BrowserDownloadGuideState extends State<_BrowserDownloadGuide> {
  static final _documentation = Uri.parse(
      'https://github.com/kbwumin13-sudo/kownload/blob/main/browser-media-extension/README.md');
  bool _opening = false;
  String? _error;

  Future<void> _open({bool documentation = false}) async {
    if (_opening) return;
    setState(() {
      _opening = true;
      _error = null;
    });
    try {
      final directory = MacOnboarding._helperDirectory;
      if (!documentation && !Directory(directory).existsSync()) {
        if (mounted) {
          setState(
              () => _error = '此应用中没有附带下载助手。请打开使用文档获取助手，再按上方步骤加载。普通链接仍可直接下载。');
        }
        return;
      }
      final opened = await MacPanel.transient(() => launchUrl(
          documentation ? _documentation : Uri.directory(directory),
          mode: LaunchMode.externalApplication));
      if (!opened && mounted) {
        setState(() => _error =
            documentation ? '无法打开使用文档，请检查默认浏览器后重试。' : '无法打开助手文件夹，请重试或查看使用文档。');
      }
    } catch (_) {
      if (mounted) {
        setState(() => _error =
            documentation ? '无法打开使用文档，请检查默认浏览器后重试。' : '无法打开助手文件夹，请重试或查看使用文档。');
      }
    } finally {
      if (mounted) setState(() => _opening = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return Dialog(
      insetPadding: const EdgeInsets.symmetric(horizontal: 20, vertical: 16),
      backgroundColor: c.background,
      shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(12),
          side: BorderSide(color: c.border, width: .5)),
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 440),
        child: Padding(
          padding: const EdgeInsets.all(24),
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Flexible(
              child: SingleChildScrollView(
                child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text('设置浏览器下载',
                          style: TextStyle(
                              color: c.text,
                              fontSize: 17,
                              fontWeight: FontWeight.w500)),
                      const SizedBox(height: 6),
                      Text('支持 Chrome 和 Edge。浏览器运行时连接尚未验证；打开助手文件夹或完成教程不代表已连接。',
                          style: TextStyle(
                              color: c.secondary, fontSize: 12, height: 1.4)),
                      const SizedBox(height: 18),
                      Container(
                        decoration: BoxDecoration(
                          color: c.surface,
                          borderRadius: BorderRadius.circular(8),
                          border: Border.all(color: c.border, width: .5),
                        ),
                        child: Column(children: [
                          _step(context, '1', '打开浏览器管理页', [
                            SelectableText(
                                'Chrome：chrome://extensions\nEdge：edge://extensions',
                                style: TextStyle(
                                    color: c.secondary,
                                    fontSize: 12,
                                    height: 1.55)),
                          ]),
                          Divider(
                              height: .5,
                              thickness: .5,
                              indent: 42,
                              endIndent: 12,
                              color: c.border),
                          _step(context, '2', '载入下载助手', [
                            Text('开启“开发者模式”，点击“加载已解压的扩展程序”，选择助手文件夹。',
                                style: TextStyle(
                                    color: c.secondary,
                                    fontSize: 12,
                                    height: 1.45)),
                            const SizedBox(height: 10),
                            Wrap(spacing: 8, runSpacing: 8, children: [
                              MacButton(
                                  onPressed: _opening ? null : () => _open(),
                                  child: const Text('打开助手文件夹')),
                              TextButton(
                                  style: TextButton.styleFrom(
                                      minimumSize: const Size(0, 28),
                                      padding: const EdgeInsets.symmetric(
                                          horizontal: 4),
                                      tapTargetSize:
                                          MaterialTapTargetSize.shrinkWrap),
                                  onPressed: _opening
                                      ? null
                                      : () => _open(documentation: true),
                                  child: const Text('使用文档')),
                            ]),
                          ]),
                          Divider(
                              height: .5,
                              thickness: .5,
                              indent: 42,
                              endIndent: 12,
                              color: c.border),
                          _step(context, '3', '发送网页中的下载', [
                            Text(
                                '安装或更新后，在对应浏览器资料中确认扩展已启用并按需重新加载，回到视频或网盘网页并刷新。下载接管需要开启助手内开关并按提示允许 管理下载内容的权限；未授权时可继续使用手动下载。播放视频后点击下载按钮；网盘勾选文件后点击“kownload 下载”。连接及账号同步仍须在实际使用时确认，此教程未进行验证。',
                                style: TextStyle(
                                    color: c.secondary,
                                    fontSize: 12,
                                    height: 1.45)),
                          ]),
                        ]),
                      ),
                      if (_error != null) ...[
                        const SizedBox(height: 12),
                        Semantics(
                          liveRegion: true,
                          child: Text(_error!,
                              style: TextStyle(
                                  color: Theme.of(context).colorScheme.error,
                                  fontSize: 12)),
                        ),
                      ],
                    ]),
              ),
            ),
            const SizedBox(height: 24),
            Align(
              alignment: Alignment.centerRight,
              child: MacButton(
                  primary: true,
                  onPressed: () => Navigator.of(context).pop(),
                  child: const Text('返回教程')),
            ),
          ]),
        ),
      ),
    );
  }

  Widget _step(
      BuildContext context, String number, String title, List<Widget> body) {
    final c = MacColors.of(context);
    return Padding(
      padding: const EdgeInsets.all(12),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        SizedBox(
            width: 18,
            child: Text(number,
                style: TextStyle(fontSize: 12, color: c.secondary))),
        const SizedBox(width: 8),
        Expanded(
          child:
              Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(title, style: TextStyle(fontSize: 13, color: c.text)),
            const SizedBox(height: 5),
            ...body,
          ]),
        ),
      ]),
    );
  }
}
