import 'dart:io';

import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:path/path.dart' as path;
import 'package:url_launcher/url_launcher.dart';

import '../app/modules/app/controllers/app_controller.dart';
import '../database/database.dart';
import 'mac_panel.dart';
import 'mac_widgets.dart';

/// A one-time tutorial before the first download, also available from Settings.
class MacOnboarding {
  static const completedKey = 'macDownloadOnboarding.v1';
  static Future<bool>? _pending;
  static bool get completed => Database.instance.box.get(completedKey) == true;

  @visibleForTesting
  static String? helperDirectoryOverride;

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
              builder: (_) => const _DownloadTutorial(),
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
  const _DownloadTutorial();

  @override
  State<_DownloadTutorial> createState() => _DownloadTutorialState();
}

class _DownloadTutorialState extends State<_DownloadTutorial> {
  late final AppController _app = Get.find<AppController>();
  late final _directory =
      TextEditingController(text: _app.downloaderConfig.value.downloadDir);
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
    if (!_saving && !_choosing && !_explaining) {
      Navigator.of(context).pop(false);
    }
  }

  Future<void> _showBrowserGuide() async {
    if (_saving || _choosing || _explaining) return;
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
      final selected = await MacPanel.transient(() => FilePicker.platform
          .getDirectoryPath(
              dialogTitle: '选择下载保存位置',
              initialDirectory: _directory.text.trim().isEmpty
                  ? null
                  : _directory.text.trim()));
      if (mounted && selected != null) _directory.text = selected;
    } catch (_) {
      if (mounted) setState(() => _error = '无法选择文件夹，请重试或输入保存位置。');
    } finally {
      if (mounted) setState(() => _choosing = false);
    }
  }

  Future<void> _complete() async {
    if (_saving || _choosing || _explaining) return;
    final directory = _directory.text.trim();
    if (directory.isEmpty) {
      setState(() => _error = '请选择下载保存位置。');
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
      config.downloadDir = directory;
      await _app.saveConfig();
      directorySaved = true;
      // Await Hive itself: Database.save does not expose write failures.
      await Database.instance.box.put(MacOnboarding.completedKey, true);
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
    final busy = _saving || _choosing || _explaining;
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
                          Text('确认保存位置，即可开始下载。',
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
                      Text('从 Chrome 或 Edge 发送网页中的视频与网盘文件。',
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
                                '回到视频或网盘网页并刷新。播放视频后点击下载按钮；网盘勾选文件后点击“kownload 下载”。',
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
