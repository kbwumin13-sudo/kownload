import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';

import '../../../../api/model/options.dart';
import '../../../../api/model/request.dart';
import '../../../../api/netdisk.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../../macos/mac_panel.dart';
import '../../../../util/browser_profile_access.dart';
import '../../../../util/netdisk_browser.dart';
import '../../../views/directory_selector.dart';

/// A foreground, cancellable confirmation. Nothing is saved to configuration
/// or ordinary URL history, and preparing a batch requires an explicit click.
class NetdiskView extends StatefulWidget {
  final NetdiskInput input;
  final Options options;
  final RequestProxy? proxy;
  final NetdiskApi? api;
  final NetdiskBrowserTool? browserTool;
  final Duration pollInterval;

  /// After browser capture, continue the original task without preparing a
  /// batch or listing files against a source whose file may be transferred.
  final Future<void> Function()? onResume;
  const NetdiskView(
      {super.key,
      required this.input,
      required this.options,
      this.proxy,
      this.api,
      this.browserTool,
      this.onResume,
      this.pollInterval = const Duration(seconds: 2)});

  @override
  State<NetdiskView> createState() => _NetdiskViewState();
}

class _NetdiskViewState extends State<NetdiskView> {
  late final NetdiskApi _api = widget.api ?? NetdiskApi();
  late final NetdiskBrowserTool _browserTool =
      widget.browserTool ?? NetdiskBrowserTool();
  late final TextEditingController _path =
      TextEditingController(text: widget.options.path);
  late NetdiskSource _source = widget.input.source;
  late String _sessionId = widget.input.sessionId;
  late List<NetdiskFile> _files = List.of(widget.input.files);
  final _selected = <String, NetdiskFile>{};
  final _directories = <(NetdiskSource, String)>[];
  List<NetdiskBrowser> _browsers = [];
  NetdiskBrowser? _browser;
  NetdiskBrowserProfile? _profile;
  String _providerName = '';
  String _nextPage = '';
  String _title = '';
  String? _error;
  String? _notice;
  String? _browserDiscoveryError;
  bool _serviceLoading = false;
  bool _discovering = false;
  String? _requestId;
  Timer? _pollTimer;
  int _generation = 0;
  bool _loading = false;
  bool _submitting = false;
  bool _waiting = false;
  bool _batchAttempted = false;
  bool _closed = false;
  final _pageCursors = <String>{};

  bool get _active => mounted && !_closed;
  bool get _resumeOnly => widget.onResume != null;
  bool get _locked => _loading || _waiting || _submitting || _batchAttempted;

  @override
  void initState() {
    super.initState();
    for (final file in _files) {
      _selected[file.key] = file;
    }
    _providerName = netdiskProviders[_source.provider] ?? '网盘';
    _loadServiceInfo();
    _loadBrowsers();
    if (!_resumeOnly && _files.isEmpty && _sessionId.isNotEmpty) _load();
  }

  Future<void> _loadServiceInfo() async {
    // Independent metadata failures must not hide a captured selection.
    if (!_active || _serviceLoading) return;
    setState(() => _serviceLoading = true);
    try {
      final providers = await _api.providers();
      if (!_active) return;
      setState(() {
        _notice = '当前下载服务不支持$_providerName，请检查服务版本。';
        for (final info in providers) {
          if (info.id == _source.provider) {
            _providerName = info.name;
            _notice = null;
          }
        }
      });
    } catch (e) {
      if (_active) setState(() => _notice = _message(e));
    } finally {
      if (_active) setState(() => _serviceLoading = false);
    }
  }

  Future<void> _loadBrowsers() async {
    if (!_active || _discovering) return;
    setState(() => _discovering = true);
    try {
      final browsers = await _browserTool.browsers();
      if (!_active) return;
      setState(() {
        final previousApplication = _browser?.application;
        final previousDirectory = _profile?.directory;
        _browsers = browsers;
        _browser = null;
        _profile = null;
        _browserDiscoveryError = null;
        for (final browser in browsers) {
          if (browser.application == previousApplication) _browser = browser;
        }
        _browser ??= browsers.where((b) => b.profiles.isNotEmpty).firstOrNull;
        _browser ??= browsers.firstOrNull;
        for (final profile in _browser?.profiles ?? <NetdiskBrowserProfile>[]) {
          if (profile.directory == previousDirectory) _profile = profile;
        }
        _profile ??= _browser?.profiles.firstOrNull;
      });
    } catch (e) {
      if (_active) setState(() => _browserDiscoveryError = _message(e));
    } finally {
      if (_active) setState(() => _discovering = false);
    }
  }

  String _message(Object error) {
    if (error is NetdiskException) {
      if (['login_required', 'expired', 'invalid_context']
          .contains(error.reasonCode)) {
        return '${error.message}。请在原网盘页面登录同一账号，再点“重新同步登录”。';
      }
      if (['helper_missing', 'unsupported_browser']
          .contains(error.reasonCode)) {
        return '请在所选浏览器个人资料中启用 kownload 网页助手，再回到原网盘页面同步。';
      }
      return error.message;
    }
    return '网盘请求失败，请重试或重新同步原页面。';
  }

  Future<void> _showTransfers() async {
    try {
      final records = await _api.transfers();
      if (!_active) return;
      await showDialog<void>(
          context: context,
          builder: (dialogContext) => StatefulBuilder(
              builder: (context, update) => AlertDialog(
                    title: const Text('临时转存记录'),
                    content: SizedBox(
                        width: 520,
                        height: 300,
                        child: records.isEmpty
                            ? const Center(child: Text('暂无临时转存'))
                            : ListView(
                                children: records.map((record) {
                                final state = record['state'] as String? ?? '';
                                final remaining =
                                    (record['remaining'] as List? ?? []).length;
                                final files =
                                    (record['files'] as List? ?? []).length;
                                final message =
                                    record['message'] as String? ?? '';
                                return ListTile(
                                    title: Text(
                                        '${netdiskProviders[record['provider']] ?? '网盘'} · $files 个文件'),
                                    subtitle: Text(state == 'cleaned'
                                        ? '下载完成，副本已清理'
                                        : remaining > 0
                                            ? '还有 $remaining 项未完成，副本继续保留'
                                            : message.isNotEmpty
                                                ? message
                                                : '下载完成，可清理副本'),
                                    trailing: state == 'cleaned'
                                        ? const Icon(CupertinoIcons.check_mark,
                                            color: Colors.green)
                                        : remaining > 0
                                            ? null
                                            : TextButton(
                                                onPressed: () async {
                                                  try {
                                                    await _api.cleanup(
                                                        record['batchId']
                                                            as String);
                                                    if (dialogContext.mounted)
                                                      update(() {
                                                        record['state'] =
                                                            'cleaned';
                                                        record['message'] = '';
                                                      });
                                                  } catch (e) {
                                                    if (dialogContext.mounted)
                                                      update(() {
                                                        record['message'] =
                                                            _message(e);
                                                      });
                                                  }
                                                },
                                                child: const Text('重试清理')));
                              }).toList())),
                    actions: [
                      TextButton(
                          onPressed: () => Navigator.pop(dialogContext),
                          child: const Text('关闭'))
                    ],
                  )));
    } catch (e) {
      if (_active) setState(() => _error = _message(e));
    }
  }

  Future<void> _load(
      {String cursor = '',
      NetdiskSource? directory,
      String? directoryName,
      bool back = false}) async {
    if (_resumeOnly || !_active || _loading || _submitting || _waiting) return;
    final source = directory ?? _source;
    final generation = _generation;
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final listing = await _api.list(source, _sessionId,
          cursor: cursor, proxy: widget.proxy);
      if (!_active || generation != _generation) return;
      setState(() {
        if (directory != null) {
          if (back) {
            _directories.removeLast();
          } else {
            _directories.add((_source, _title));
          }
        }
        _source = listing.source;
        if (listing.sessionId.isNotEmpty) _sessionId = listing.sessionId;
        _title =
            listing.title.isNotEmpty ? listing.title : directoryName ?? _title;
        if (cursor.isEmpty) {
          _files = listing.files;
          _pageCursors.clear();
        } else {
          final keys = _files.map((f) => f.key).toSet();
          _files.addAll(listing.files.where((f) => keys.add(f.key)));
          _pageCursors.add(cursor);
        }
        _nextPage = listing.nextPage;
        if (_pageCursors.contains(_nextPage) && _nextPage.isNotEmpty) {
          _nextPage = '';
          _error = '网盘返回了重复分页，请刷新列表。';
        }
        // A refresh reconciles the current page without losing other folders.
        for (final file in _files) {
          if (_selected.containsKey(file.key)) _selected[file.key] = file;
        }
      });
    } catch (e) {
      if (_active && generation == _generation) {
        setState(() {
          _error = _message(e);
        });
      }
    } finally {
      if (_active && generation == _generation) {
        setState(() {
          _loading = false;
        });
      }
    }
  }

  Future<void> _chooseLocalState() async {
    final browser = _browser;
    if (browser == null) return;
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final path = await MacPanel.transient(() => BrowserProfileAccess.choose(
          title: '允许 kownload 读取 ${browser.name} 的个人资料名称',
          path: browser.localStatePath));
      if (!_active || path == null) return;
      final profiles = await _browserTool.readProfiles(path);
      if (!_active) return;
      if (profiles.isEmpty) {
        throw const NetdiskException('该文件没有可用的浏览器个人资料', 'invalid_profile');
      }
      final updated = NetdiskBrowser(
          name: browser.name,
          application: browser.application,
          localStatePath: path,
          profiles: profiles);
      setState(() {
        _browsers[_browsers.indexOf(browser)] = updated;
        _browser = updated;
        _profile = profiles.first;
      });
    } catch (e) {
      if (_active) {
        setState(() {
          _error = _message(e);
        });
      }
    } finally {
      if (_active) {
        setState(() {
          _loading = false;
        });
      }
    }
  }

  Future<void> _syncBrowser() async {
    final browser = _browser;
    final profile = _profile;
    if (browser == null || profile == null || _locked) return;
    final generation = ++_generation;
    setState(() {
      _waiting = true;
      _error = null;
    });
    try {
      // Do not invent an instance ID from the profile name: the helper claims
      // this source on its original page and binds its own instance/context.
      final request = await _api.requestBrowser(_source);
      if (!_active || generation != _generation) {
        await _api.cancelBrowser(request.id);
        return;
      }
      _requestId = request.id;
      await _browserTool.launch(browser, profile, _source,
          requestId: request.id);
      if (_active && generation == _generation) _schedulePoll(generation);
    } catch (e) {
      if (_active && generation == _generation) {
        setState(() {
          _error = _message(e);
        });
        _stopWaiting();
      }
    }
  }

  void _schedulePoll(int generation) {
    _pollTimer = Timer(widget.pollInterval, () => _poll(generation));
  }

  Future<void> _poll(int generation) async {
    if (!_active ||
        !_waiting ||
        generation != _generation ||
        _requestId == null) {
      return;
    }
    // A covered dialog (e.g. file picker route) does not issue status requests.
    if (ModalRoute.of(context)?.isCurrent == false) {
      _schedulePoll(generation);
      return;
    }
    try {
      final request = await _api.browserStatus(_requestId!);
      if (!_active || generation != _generation) return;
      if (request.state == 'ready' && request.sessionId.isNotEmpty) {
        if (!_resumeOnly) {
          setState(() {
            _sessionId = request.sessionId;
          });
        }
        _stopWaiting();
        if (_resumeOnly) {
          await _resume();
        } else {
          await _load();
        }
        return;
      }
      if (['expired', 'cancelled', 'error', 'failed'].contains(request.state)) {
        setState(() {
          _error = request.message.isNotEmpty
              ? '${request.message}。请确认所选个人资料已启用网页助手，并在原网盘页面登录后重试。'
              : '页面同步已结束，请启用网页助手并在原网盘页面登录后重试。';
        });
        _stopWaiting();
        return;
      }
      _schedulePoll(generation);
    } catch (e) {
      if (_active && generation == _generation) {
        setState(() {
          _error = _message(e);
        });
        _stopWaiting();
      }
    }
  }

  void _cancelRequest() {
    _pollTimer?.cancel();
    _pollTimer = null;
    final id = _requestId;
    _requestId = null;
    if (id != null) unawaited(_api.cancelBrowser(id).catchError((Object _) {}));
  }

  void _stopWaiting() {
    ++_generation;
    _cancelRequest();
    if (_active) {
      setState(() {
        _waiting = false;
      });
    }
  }

  void _close([bool? result]) {
    if (_submitting) return;
    _closed = true;
    ++_generation;
    _cancelRequest();
    Navigator.of(context).pop(result);
  }

  Future<void> _confirm() async {
    if (_resumeOnly || _locked || _selected.isEmpty || _sessionId.isEmpty)
      return;
    if (_path.text.trim().isEmpty) {
      setState(() {
        _error = '请选择保存目录';
      });
      return;
    }
    setState(() {
      _submitting = true;
      _error = null;
    });
    try {
      final opts = Options.fromJson(widget.options.toJson())
        ..path = renderPathPlaceholders(_path.text.trim());
      final prepared = await _api.prepare(
          _source, _sessionId, _selected.values.toList(), opts,
          proxy: widget.proxy);
      if (prepared.reqs.isEmpty) {
        throw const NetdiskException('所选目录没有可下载文件', 'empty_selection');
      }
      _batchAttempted = true;
      await _api.createBatch(prepared);
      if (!_active) return;
      setState(() {
        _submitting = false;
      });
      _close(true);
    } catch (e) {
      if (!_active) return;
      setState(() {
        _submitting = false;
        final detail =
            e is NetdiskException && e.data is Map ? e.data as Map : null;
        final created = detail?['taskIds'] ?? detail?['createdTaskIds'];
        final batchMessage = created is List && created.isNotEmpty
            ? '已创建 ${created.length} 个任务，其余任务未完成创建。'
            : '批次未全部确认，部分任务可能已创建。';
        _error = _batchAttempted
            ? '${_message(e)}\n$batchMessage请先检查任务列表；临时转存副本会保留，避免重复提交。'
            : _message(e);
      });
    }
  }

  Future<void> _resume() async {
    if (!_active || _submitting) return;
    setState(() {
      _submitting = true;
      _error = null;
    });
    try {
      await widget.onResume!();
      if (!_active) return;
      setState(() => _submitting = false);
      _close(true);
    } catch (e) {
      if (_active) {
        setState(() {
          _submitting = false;
          _error = _message(e);
        });
      }
    }
  }

  @override
  void dispose() {
    _closed = true;
    ++_generation;
    _cancelRequest();
    _path.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    final size = MediaQuery.sizeOf(context);
    return PopScope(
      canPop: !_submitting,
      child: Dialog(
        insetPadding: const EdgeInsets.all(24),
        backgroundColor: c.surface,
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
        clipBehavior: Clip.antiAlias,
        child: SizedBox(
          width: math.min(720, size.width - 48),
          height: math.min(640, size.height - 48),
          child:
              Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
            MacToolbar(
                title: '$_providerName · ${_resumeOnly ? '恢复下载' : '确认下载'}',
                actions: [
                  IconButton(
                      tooltip: '临时转存记录',
                      onPressed: _submitting ? null : _showTransfers,
                      icon: const Icon(CupertinoIcons.clock, size: 16)),
                  IconButton(
                      tooltip: '关闭',
                      onPressed: _submitting ? null : _close,
                      icon: const Icon(CupertinoIcons.xmark, size: 16)),
                ]),
            Padding(
                padding: const EdgeInsets.fromLTRB(16, 12, 16, 8),
                child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Wrap(
                          spacing: 10,
                          runSpacing: 6,
                          crossAxisAlignment: WrapCrossAlignment.center,
                          children: [
                            SizedBox(
                                width: 175,
                                child: DropdownButton<NetdiskBrowser>(
                                  isExpanded: true,
                                  value: _browser,
                                  hint: const Text('选择浏览器'),
                                  items: _browsers
                                      .map((b) => DropdownMenuItem(
                                          value: b, child: Text(b.name)))
                                      .toList(),
                                  onChanged: _locked || _discovering
                                      ? null
                                      : (b) => setState(() {
                                            _browser = b;
                                            _profile =
                                                b?.profiles.isNotEmpty == true
                                                    ? b!.profiles.first
                                                    : null;
                                          }),
                                )),
                            SizedBox(
                                width: 155,
                                child: DropdownButton<NetdiskBrowserProfile>(
                                  isExpanded: true,
                                  value: _profile,
                                  hint: const Text('选择个人资料'),
                                  items: (_browser?.profiles ?? [])
                                      .map((p) => DropdownMenuItem(
                                          value: p,
                                          child: Text(p.name,
                                              overflow: TextOverflow.ellipsis)))
                                      .toList(),
                                  onChanged: _locked || _discovering
                                      ? null
                                      : (p) => setState(() {
                                            _profile = p;
                                          }),
                                )),
                            MacButton(
                                onPressed:
                                    _locked || _discovering || _profile == null
                                        ? null
                                        : _syncBrowser,
                                child: Text(_resumeOnly || _sessionId.isNotEmpty
                                    ? '重新同步登录'
                                    : '打开页面并同步')),
                            TextButton(
                                onPressed:
                                    _locked || _discovering || _browser == null
                                        ? null
                                        : _chooseLocalState,
                                child: const Text('选择资料文件')),
                          ]),
                      if (_waiting) ...[
                        const SizedBox(height: 6),
                        Row(children: [
                          const CupertinoActivityIndicator(radius: 7),
                          const SizedBox(width: 8),
                          const Expanded(
                              child: Text('等待原网盘页面的助手同步登录…',
                                  style: TextStyle(fontSize: 12))),
                          TextButton(
                              onPressed: _stopWaiting,
                              child: const Text('取消同步')),
                        ]),
                      ],
                      if (_resumeOnly)
                        const Padding(
                            padding: EdgeInsets.only(top: 6),
                            child: Text('请登录下载时的同一账号；同步成功后继续原任务。',
                                style: TextStyle(fontSize: 12))),
                      if (_sessionId.isEmpty || _waiting)
                        const Padding(
                            padding: EdgeInsets.only(top: 6),
                            child: Text(
                                '请在所选个人资料中启用 kownload 网页助手，登录原网盘页面。同步超时可取消后重试。',
                                style: TextStyle(fontSize: 12))),
                      if (_browsers.isEmpty &&
                          !_discovering &&
                          _browserDiscoveryError == null &&
                          (_resumeOnly || _sessionId.isEmpty))
                        const Text('未找到 Chrome 或 Edge。请安装浏览器并启用网页助手后再打开此窗口。',
                            style: TextStyle(fontSize: 12)),
                      if (_browserDiscoveryError != null ||
                          (_browser != null && _browser!.profiles.isEmpty)) ...[
                        Text(
                            _browserDiscoveryError ??
                                _browser!.profileError?.message ??
                                '该浏览器没有可用的个人资料：${_browser!.localStatePath}。请选择实际使用的 Local State 文件。',
                            key: const ValueKey('netdisk-profile-error'),
                            style: const TextStyle(fontSize: 12)),
                      ],
                      if (!_discovering)
                        TextButton(
                            onPressed: _locked ? null : _loadBrowsers,
                            child: const Text('重新读取资料')),
                      if (_notice != null)
                        Wrap(
                            crossAxisAlignment: WrapCrossAlignment.center,
                            children: [
                              Text(_notice!,
                                  style: const TextStyle(fontSize: 12)),
                              TextButton(
                                  onPressed: _serviceLoading || _locked
                                      ? null
                                      : _loadServiceInfo,
                                  child: const Text('重试服务信息')),
                            ]),
                      if (_error != null)
                        Padding(
                            padding: const EdgeInsets.only(top: 6),
                            child: Text(_error!,
                                key: const ValueKey('netdisk-error'),
                                style: TextStyle(
                                    fontSize: 12,
                                    color:
                                        Theme.of(context).colorScheme.error))),
                    ])),
            const Divider(height: 1),
            if (_resumeOnly)
              const Padding(
                  padding: EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                  child: Text('原任务文件'))
            else
              Padding(
                  padding:
                      const EdgeInsets.symmetric(horizontal: 16, vertical: 6),
                  child: Row(children: [
                    IconButton(
                        tooltip: '上级目录',
                        icon: const Icon(CupertinoIcons.chevron_left, size: 16),
                        onPressed: _locked || _directories.isEmpty
                            ? null
                            : () => _load(
                                directory: _directories.last.$1,
                                directoryName: _directories.last.$2,
                                back: true)),
                    Expanded(
                        child: Text(_title.isEmpty ? '文件列表' : _title,
                            maxLines: 1, overflow: TextOverflow.ellipsis)),
                    TextButton(
                        onPressed: _locked || _files.isEmpty
                            ? null
                            : () => setState(() {
                                  final all = _files.every(
                                      (f) => _selected.containsKey(f.key));
                                  for (final f in _files) {
                                    if (all) {
                                      _selected.remove(f.key);
                                    } else {
                                      _selected[f.key] = f;
                                    }
                                  }
                                }),
                        child: const Text('全选 / 取消全选')),
                    IconButton(
                        tooltip: '刷新列表',
                        onPressed: _locked || _sessionId.isEmpty
                            ? null
                            : () => _load(),
                        icon: const Icon(CupertinoIcons.refresh, size: 16)),
                  ])),
            Expanded(
                child: _loading && _files.isEmpty
                    ? const Center(child: CupertinoActivityIndicator())
                    : _files.isEmpty
                        ? Center(
                            child: Text(
                                _sessionId.isEmpty ? '同步登录后查看文件' : '此目录没有文件'))
                        : ListView.builder(
                            itemCount:
                                _files.length + (_nextPage.isNotEmpty ? 1 : 0),
                            itemBuilder: (context, index) {
                              if (index == _files.length) {
                                return Center(
                                    child: TextButton(
                                        onPressed: _locked
                                            ? null
                                            : () => _load(cursor: _nextPage),
                                        child:
                                            Text(_loading ? '正在加载…' : '加载更多')));
                              }
                              final file = _files[index];
                              return SizedBox(
                                  height: 42,
                                  child: Row(children: [
                                    const SizedBox(width: 12),
                                    if (!_resumeOnly)
                                      CupertinoCheckbox(
                                          value:
                                              _selected.containsKey(file.key),
                                          activeColor: c.accent,
                                          onChanged: _locked
                                              ? null
                                              : (selected) => setState(() {
                                                    if (selected == true) {
                                                      _selected[file.key] =
                                                          file;
                                                    } else {
                                                      _selected
                                                          .remove(file.key);
                                                    }
                                                  })),
                                    Icon(
                                        file.isDir
                                            ? CupertinoIcons.folder
                                            : CupertinoIcons.doc,
                                        size: 18,
                                        color: c.secondary),
                                    const SizedBox(width: 8),
                                    Expanded(
                                        child: GestureDetector(
                                            behavior: HitTestBehavior.opaque,
                                            onTap: _resumeOnly || _locked
                                                ? null
                                                : () => setState(() {
                                                      if (_selected.containsKey(
                                                          file.key)) {
                                                        _selected
                                                            .remove(file.key);
                                                      } else {
                                                        _selected[file.key] =
                                                            file;
                                                      }
                                                    }),
                                            child: Text(file.name,
                                                maxLines: 1,
                                                overflow:
                                                    TextOverflow.ellipsis))),
                                    if (file.isDir && !_resumeOnly)
                                      IconButton(
                                          tooltip: '打开 ${file.name}',
                                          onPressed:
                                              _locked || _sessionId.isEmpty
                                                  ? null
                                                  : () => _load(
                                                      directory: _source
                                                          .inDirectory(file.id),
                                                      directoryName: file.name),
                                          icon: const Icon(
                                              CupertinoIcons.chevron_right,
                                              size: 14))
                                    else
                                      Padding(
                                          padding: const EdgeInsets.symmetric(
                                              horizontal: 16),
                                          child: Text(_fileSize(file.size),
                                              style: TextStyle(
                                                  fontSize: 12,
                                                  color: c.secondary))),
                                  ]));
                            })),
            if (_loading && _files.isNotEmpty)
              const LinearProgressIndicator(minHeight: 2),
            const Divider(height: 1),
            Padding(
                padding: const EdgeInsets.fromLTRB(16, 10, 16, 12),
                child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      const Text('保存目录', style: TextStyle(fontSize: 12)),
                      const SizedBox(height: 5),
                      if (_resumeOnly)
                        Text(_path.text,
                            maxLines: 2, overflow: TextOverflow.ellipsis)
                      else
                        IgnorePointer(
                            ignoring: _submitting || _batchAttempted,
                            child: DirectorySelector(
                                controller: _path,
                                allowEdit: true,
                                showLabel: false,
                                inlineActions: true,
                                showPlaceholderButton: false,
                                showRenderedPlaceholders: false)),
                      const SizedBox(height: 8),
                      Text(
                          _resumeOnly
                              ? '继续原任务的下载进度，保留原保存位置和临时转存副本。'
                              : '已选 ${_selected.length} 项，文件夹将包含其全部内容。必要时自动转存，整批下载成功后清理临时副本。',
                          style: TextStyle(fontSize: 12, color: c.secondary)),
                      const SizedBox(height: 10),
                      Row(mainAxisAlignment: MainAxisAlignment.end, children: [
                        if (_submitting)
                          const Padding(
                              padding: EdgeInsets.only(right: 12),
                              child: CupertinoActivityIndicator(radius: 8)),
                        MacButton(
                            onPressed: _submitting ? null : _close,
                            child: const Text('取消')),
                        if (_resumeOnly && _submitting)
                          const Padding(
                              padding: EdgeInsets.only(left: 10),
                              child: Text('正在继续任务…')),
                        if (!_resumeOnly) ...[
                          const SizedBox(width: 10),
                          MacButton(
                              primary: true,
                              onPressed: _locked ||
                                      _selected.isEmpty ||
                                      _sessionId.isEmpty
                                  ? null
                                  : _confirm,
                              child: Text(_submitting ? '正在创建任务…' : '确认下载')),
                        ],
                      ]),
                    ])),
          ]),
        ),
      ),
    );
  }

  String _fileSize(int size) {
    if (size < 1024) return '$size B';
    if (size < 1024 * 1024) return '${(size / 1024).toStringAsFixed(1)} KB';
    if (size < 1024 * 1024 * 1024) {
      return '${(size / (1024 * 1024)).toStringAsFixed(1)} MB';
    }
    return '${(size / (1024 * 1024 * 1024)).toStringAsFixed(1)} GB';
  }
}
