import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../../api/api.dart' as api;
import '../../../../api/model/downloader_config.dart';
import '../../../../api/model/media_tools_status.dart';
import '../../../../macos/mac_panel.dart';

class MacMediaToolsEditor extends StatefulWidget {
  final MediaConfig draft;
  final Future<MediaToolsStatus> Function()? probe;

  const MacMediaToolsEditor({super.key, required this.draft, this.probe});

  @override
  State<MacMediaToolsEditor> createState() => _MacMediaToolsEditorState();
}

class _MacMediaToolsEditorState extends State<MacMediaToolsEditor> {
  late final _ytDlp = TextEditingController(text: widget.draft.ytDlpPath);
  late final _ffmpeg = TextEditingController(text: widget.draft.ffmpegPath);
  late final _node = TextEditingController(text: widget.draft.jsRuntimePath);
  MediaToolsStatus? _status;
  bool _loading = false;
  bool _failed = false;
  int _revision = 0;
  String? _guideError;

  bool get _canEdit =>
      widget.probe != null || api.activeMediaToolsSnapshot != null;

  @override
  void initState() {
    super.initState();
    api.activeApiSnapshotChanges.addListener(_refresh);
    _refresh();
  }

  @override
  void dispose() {
    api.activeApiSnapshotChanges.removeListener(_refresh);
    _ytDlp.dispose();
    _ffmpeg.dispose();
    _node.dispose();
    super.dispose();
  }

  Future<void> _refresh() async {
    final revision = ++_revision;
    final snapshot = api.activeMediaToolsSnapshot;
    final fixtureProbe = widget.probe;
    setState(() {
      _status = null;
      _failed = false;
      _loading = fixtureProbe != null || snapshot != null;
    });
    if (!_loading) return;
    try {
      final status =
          await (fixtureProbe?.call() ?? api.getMediaToolsStatus(snapshot!));
      if (mounted && revision == _revision) {
        setState(() => _status = status);
      }
    } catch (_) {
      if (mounted && revision == _revision) {
        setState(() => _failed = true);
      }
    } finally {
      if (mounted && revision == _revision) {
        setState(() => _loading = false);
      }
    }
  }

  Future<void> _openGuide(String url) async {
    var opened = false;
    try {
      opened =
          await launchUrl(Uri.parse(url), mode: LaunchMode.externalApplication);
    } catch (_) {
      // The URL remains available for copying if the system browser fails.
    }
    if (mounted) setState(() => _guideError = opened ? null : url);
  }

  Widget _tool(
      {required String name,
      required TextEditingController controller,
      required void Function(String) write,
      required MediaToolStatus? status,
      required String guide}) {
    void setPath(String path) {
      if (!_canEdit) return;
      controller.text = path;
      write(path);
    }

    return Padding(
      padding: const EdgeInsets.only(bottom: 14),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(name, style: Theme.of(context).textTheme.titleSmall),
          if (status != null) ...[
            Text([
              _stateLabel(status.state),
              if (status.version.isNotEmpty) status.version,
              if (name == 'Node' &&
                  !status.enabled &&
                  status.state != MediaToolState.inactive &&
                  status.state != MediaToolState.notConfigured)
                '当前未启用',
              if (status.fallback)
                '已保存的路径未生效，使用默认路径'
              else if (status.custom)
                '自定义配置',
              if (status.setting.isEmpty) '未配置路径',
            ].join(' · ')),
            if (status.path.isNotEmpty)
              SelectableText(status.path,
                  key: ValueKey('effective-${name.toLowerCase()}')),
          ],
          if (_canEdit)
            TextField(
              key: ValueKey('media-path-${name.toLowerCase()}'),
              controller: controller,
              decoration: const InputDecoration(labelText: '配置路径（保存后生效）'),
              onChanged: (value) {
                if (_canEdit) write(value.trim());
              },
            ),
          Wrap(
            spacing: 6,
            children: [
              if (_canEdit)
                TextButton(
                    onPressed: () async {
                      final paths =
                          await MacPanel.chooseFiles(title: '选择 $name');
                      if (mounted && paths?.length == 1) setPath(paths!.single);
                    },
                    child: Text('选择 $name…')),
              if (_canEdit)
                TextButton(
                    onPressed: status == null && name == 'yt-dlp'
                        ? null
                        : () => setPath(status?.defaultPath ?? ''),
                    child: Text('恢复 $name 默认')),
              TextButton(
                  onPressed: () => _openGuide(guide),
                  child: Text('$name 官方安装指引')),
            ],
          ),
        ],
      ),
    );
  }

  String _stateLabel(MediaToolState state) => switch (state) {
        MediaToolState.ready => '可运行',
        MediaToolState.missing => '未找到',
        MediaToolState.notRunnable => '无法运行',
        MediaToolState.timedOut => '检测超时',
        MediaToolState.notConfigured => '未配置',
        MediaToolState.inactive => '当前未启用（需要绝对路径）',
        MediaToolState.unrecognized => '无法识别版本',
        MediaToolState.unavailable => '无法确认',
        MediaToolState.unsupported => '需要使用绝对 Python 解释器的 yt-dlp 入口',
        MediaToolState.incompatible => '当前 Python 环境无法加载媒体桥',
      };

  @override
  Widget build(BuildContext context) {
    final status = _status;
    final bridge = status?.bridge;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (!_canEdit)
          const Text('当前连接无法安全检查或修改本机工具。请切换到应用默认的本机 API，重启后再试。')
        else ...[
          const Text('下载服务当前使用的工具'),
          if (_loading) const Text('正在检测…'),
          if (_failed) const Text('无法取得下载服务的工具状态。'),
          TextButton(
              onPressed: _loading ? null : _refresh, child: const Text('重新检测')),
        ],
        _tool(
            name: 'yt-dlp',
            controller: _ytDlp,
            write: (path) => widget.draft.ytDlpPath = path,
            status: status?.ytDlp,
            guide: 'https://github.com/yt-dlp/yt-dlp/wiki/Installation'),
        if (bridge != null) ...[
          Text(
              '限定域请求头与共享恢复桥 · ${bridge.state == MediaToolState.ready ? '离线加载通过' : _stateLabel(bridge.state)}'
              '${bridge.version.isEmpty ? '' : ' · yt-dlp ${bridge.version}'}'),
          if (bridge.interpreter.isNotEmpty) SelectableText(bridge.interpreter),
        ],
        const Padding(
          padding: EdgeInsets.only(bottom: 14),
          child: Text(
              '需要限定域请求头或共享恢复时，请用 pip 或 pipx 安装 yt-dlp，并选择使用该 Python 环境的入口。'),
        ),
        _tool(
            name: 'ffmpeg',
            controller: _ffmpeg,
            write: (path) => widget.draft.ffmpegPath = path,
            status: status?.ffmpeg,
            guide: 'https://ffmpeg.org/download.html'),
        _tool(
            name: 'Node',
            controller: _node,
            write: (path) =>
                widget.draft.jsRuntimePath = path.isEmpty ? null : path,
            status: status?.node,
            guide: 'https://nodejs.org/en/download'),
        if (_guideError != null) SelectableText('无法打开安装指引：$_guideError'),
      ],
    );
  }
}
