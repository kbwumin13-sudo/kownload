import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../../api/api.dart' as api;
import '../../../../api/model/downloader_config.dart';
import '../../../../api/model/media_tools_status.dart';
import '../../../../macos/mac_panel.dart';

class MacMediaToolsEditor extends StatefulWidget {
  final MediaConfig draft;
  final Future<MediaToolsStatus> Function()? probe;
  final Future<ManagedYtDlpState> Function(ManagedYtDlpAction)? manage;

  const MacMediaToolsEditor(
      {super.key, required this.draft, this.probe, this.manage});

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
        if (_canEdit &&
            (widget.manage != null || api.activeMediaToolsSnapshot != null))
          _ManagedYtDlpControls(
            key: const ValueKey('managed-ytdlp-controls'),
            request: widget.manage,
            onChange: _refresh,
            useEntry: (path) {
              if (!_canEdit) return;
              _ytDlp.text = path;
              widget.draft.ytDlpPath = path;
            },
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

class _ManagedYtDlpControls extends StatefulWidget {
  final Future<ManagedYtDlpState> Function(ManagedYtDlpAction)? request;
  final VoidCallback onChange;
  final ValueChanged<String> useEntry;

  const _ManagedYtDlpControls(
      {super.key,
      this.request,
      required this.onChange,
      required this.useEntry});

  @override
  State<_ManagedYtDlpControls> createState() => _ManagedYtDlpControlsState();
}

class _ManagedYtDlpControlsState extends State<_ManagedYtDlpControls> {
  ManagedYtDlpState? _state;
  String? _message;
  bool _busy = false;
  int _generation = 0;

  @override
  void initState() {
    super.initState();
    api.activeApiSnapshotChanges.addListener(_read);
    _read();
  }

  @override
  void dispose() {
    api.activeApiSnapshotChanges.removeListener(_read);
    super.dispose();
  }

  void _read() {
    _state = null;
    _run(ManagedYtDlpAction.read);
  }

  Future<void> _run(ManagedYtDlpAction action) async {
    final generation = ++_generation;
    final snapshot = api.activeMediaToolsSnapshot;
    setState(() {
      _busy = true;
      _message = null;
    });
    try {
      final result = await (widget.request?.call(action) ??
          (snapshot == null
              ? Future<ManagedYtDlpState>.error(
                  const ManagedYtDlpException('managed_ytdlp_unavailable'))
              : api.managedYtDlpRequest(snapshot, action)));
      if (mounted && generation == _generation) {
        final sameVersion = _state?.activeVersion == result.activeVersion;
        setState(() {
          _state = result;
          _message = switch (action) {
            ManagedYtDlpAction.read => null,
            ManagedYtDlpAction.update =>
              sameVersion ? '已确认当前自管版本。' : '自管版本已就绪。',
            ManagedYtDlpAction.rollback => '已切换到上一自管版本。',
          };
        });
        if (action != ManagedYtDlpAction.read) widget.onChange();
      }
    } catch (error) {
      if (mounted && generation == _generation) {
        setState(() => _message = _errorMessage(error));
      }
    } finally {
      if (mounted && generation == _generation) {
        setState(() => _busy = false);
      }
    }
  }

  String _errorMessage(Object error) {
    final code = error is ManagedYtDlpException ? error.code : '';
    return switch (code) {
      'managed_ytdlp_busy' => '另一个自管操作正在进行。',
      'managed_ytdlp_location' => '自管目录需要无空格的主目录路径和私有权限。',
      'managed_ytdlp_python' =>
        '已保存的 yt-dlp 入口需使用可加载 curl_cffi 和媒体桥的 Python 3.11 或更高版本。',
      'managed_ytdlp_download' => '无法下载官方 yt-dlp 发行文件。',
      'managed_ytdlp_signature' => '官方发布清单的签名校验失败。',
      'managed_ytdlp_asset' => '发行文件的摘要、版本或归档校验失败。',
      'managed_ytdlp_no_previous' => '暂无可切换的上一版本。',
      'managed_ytdlp_older' => '官方发行版本早于当前自管版本。',
      _ => '无法取得自管 yt-dlp 状态，请检查下载服务后重试。',
    };
  }

  @override
  Widget build(BuildContext context) {
    final state = _state;
    final installed = state?.activeVersion.isNotEmpty == true;
    return Padding(
      padding: const EdgeInsets.only(bottom: 14),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('自管 yt-dlp', style: Theme.of(context).textTheme.titleSmall),
          const Text('手动安装和更新使用当前已保存的 Python 环境。选用自管入口后，还需保存路径草稿。'),
          if (_message != null) Text(_message!),
          if (installed)
            Text([
              state!.activeVersion,
              if (state.inUse) '当前设置正在使用',
              if (state.previousVersion.isNotEmpty)
                '上一版本 ${state.previousVersion}',
            ].join(' · ')),
          Wrap(
            spacing: 6,
            children: [
              if (state != null)
                TextButton(
                  onPressed:
                      _busy ? null : () => _run(ManagedYtDlpAction.update),
                  child: Text(installed ? '检查并更新自管版本' : '安装自管版本'),
                )
              else if (!_busy)
                TextButton(onPressed: _read, child: const Text('重查自管状态')),
              if (state?.previousVersion.isNotEmpty == true)
                TextButton(
                  onPressed:
                      _busy ? null : () => _run(ManagedYtDlpAction.rollback),
                  child: const Text('回滚自管版本'),
                ),
              if (installed)
                TextButton(
                    onPressed:
                        _busy ? null : () => widget.useEntry(state!.entry),
                    child: const Text('选用自管入口')),
            ],
          ),
          if (_busy) const Text('正在检查自管版本…'),
        ],
      ),
    );
  }
}
