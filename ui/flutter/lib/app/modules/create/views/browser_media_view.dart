import 'dart:async';
import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import '../../../../macos/mac_panel.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../services/browser_media.dart';

List<Map<String, dynamic>> _records(dynamic value) => value is List
    ? value.whereType<Map>().map((e) => Map<String, dynamic>.from(e)).toList()
    : [];

String _mediaTime(dynamic value) {
  if (value is! num || value <= 0) return '';
  final seconds = value.round();
  return '${seconds ~/ 60}:${(seconds % 60).toString().padLeft(2, '0')}';
}

/// Native presentation only. Opaque IDs go back to the browser's existing
/// handlers, which own candidate qualification, auth, media intent and creation.
class BrowserMediaView extends StatefulWidget {
  final BrowserMedia media;
  final BrowserMediaSession session;
  final bool showSmallImages;
  final String defaultDirectory;
  final Future<void> Function()? disableSite;
  const BrowserMediaView(
      {super.key,
      required this.media,
      required this.session,
      this.showSmallImages = false,
      this.defaultDirectory = '',
      this.disableSite});
  @override
  State<BrowserMediaView> createState() => _BrowserMediaViewState();
}

class _BrowserMediaViewState extends State<BrowserMediaView> {
  final _selected = <String>{};
  final _inspected = <String, Map<String, dynamic>>{};
  final _inspecting = <String, Future<void>>{};
  final _formats = <String, String>{},
      _audio = <String, String>{},
      _subtitles = <String, String>{},
      _variants = <String, String>{};
  final _name = TextEditingController(),
      _path = TextEditingController(),
      _filter = TextEditingController();
  String _kind = 'all', _notice = '';
  bool _busy = false, _advanced = false, _recoveryOpen = false;
  final _footerKey = GlobalKey(),
      _kindKey = GlobalKey(),
      _filterKey = GlobalKey();
  final _itemKeys = <String, GlobalKey>{};
  List<Map<String, dynamic>> _tasks = [];
  String? _taskId, _recoverFor;
  BrowserMediaSession get session => widget.session;
  List<Map<String, dynamic>> get items => session.items;
  bool get scoped => session.view != 'all';
  bool _requiresInspect(Map item) =>
      item['mediaRequired'] == true ||
      (item['variantCount'] is num && item['variantCount'] > 1);

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted) _resize();
    });
    if (session.view == 'single' && items.length == 1) {
      _selected.add(items.single['id']);
      if (_requiresInspect(items.single))
        unawaited(_inspect(items.single).catchError(_failed));
    }
  }

  void _resize({bool measured = false}) {
    if (!mounted) return;
    double height(GlobalKey key, double fallback) {
      final box = key.currentContext?.findRenderObject();
      return measured && box is RenderBox && box.hasSize
          ? box.size.height
          : fallback;
    }

    var content = 20.0; // Existing list padding.
    final rows = _visible;
    for (final item in rows) {
      final id = item['id'] as String;
      final info = _inspected[id];
      final choice = _records(info?['options'])
          .where((v) => v['id'] == _formats[id])
          .firstOrNull;
      var controls = 0;
      if (_selected.contains(id) && info != null) {
        if (_records(info['options']).isNotEmpty) controls++;
        if (_advanced && choice?['audioOnly'] != true) {
          if (_records(info['audioTracks']).isNotEmpty) controls++;
          if (_records(info['subtitles']).isNotEmpty) controls++;
        }
        if (_advanced && _records(info['variants']).isNotEmpty) controls++;
      }
      content += height(_itemKeys.putIfAbsent(id, () => GlobalKey()),
              (session.view == 'single' ? 48 : 64) + controls * 40.0) +
          8;
      if (content > 740)
        break; // Preserve lazy scrolling for large candidate lists.
    }
    final filters = scoped ? 0 : height(_kindKey, 44) + height(_filterKey, 40);
    final footer = height(
        _footerKey,
        96 +
            (_advanced ? 88 : 0) +
            (_recoveryOpen ? 88 : 0) +
            (_notice.isEmpty ? 0 : 32));
    MacPanel.resize((44 + filters + content + footer)
        .clamp(session.view == 'single' ? 240.0 : 320.0, 740.0));
  }

  @override
  void dispose() {
    if (!session.closed) scheduleMicrotask(() => widget.media.close(session));
    _name.dispose();
    _path.dispose();
    _filter.dispose();
    super.dispose();
  }

  void _failed(Object error) {
    if (mounted)
      setState(() => _notice =
          error is StateError ? error.message.toString() : '操作未完成，请返回来源页面重试。');
  }

  Future<void> _inspect(Map<String, dynamic> item) async {
    final id = item['id'] as String;
    if (_inspected.containsKey(id)) return;
    if (_inspecting.containsKey(id)) return _inspecting[id]!;
    final work = () async {
      final data = await widget.media.send(session, 'INSPECT', itemId: id);
      if (!mounted || session.closed) return;
      if (data is! Map) throw StateError('助手返回的格式无效。');
      _inspected[id] = Map<String, dynamic>.from(data);
      _formats[id] = data['bestId'] is String ? data['bestId'] : '';
      setState(() {});
    }();
    _inspecting[id] = work;
    try {
      await work;
    } finally {
      _inspecting.remove(id);
    }
  }

  void _choose(Map<String, dynamic> item, bool checked) {
    setState(() {
      if (checked) {
        if (session.view == 'identify') _selected.clear();
        _selected.add(item['id']);
      } else {
        _selected.remove(item['id']);
      }
      _tasks = [];
      _taskId = null;
      _recoverFor = null;
    });
    if (checked && _requiresInspect(item))
      unawaited(_inspect(item).catchError(_failed));
  }

  Map<String, dynamic> _options(Map<String, dynamic> item) {
    final id = item['id'] as String;
    final info = _inspected[id];
    final format = _formats[id] ?? '';
    if (item['mediaRequired'] == true &&
        (format.isEmpty ||
            !_records(info?['options']).any((f) => f['id'] == format))) {
      throw StateError('请选择明确的下载格式；仅音频和轨道未确认的资源需要手动选择。');
    }
    final isAudio = _records(info?['options'])
            .where((f) => f['id'] == format)
            .firstOrNull?['audioOnly'] ==
        true;
    final sub = (isAudio ? '' : _subtitles[id] ?? '').split('|');
    return {
      if (format.isNotEmpty) 'format': format,
      if ((_audio[id] ?? '').isNotEmpty) 'audioId': _audio[id],
      'subtitleLanguages': sub.first.isEmpty ? <String>[] : [sub.first],
      'writeAutomaticSubtitles': sub.length > 1 && sub[1] == 'auto',
      'embedSubtitles': sub.first.isNotEmpty,
      if ((_variants[id] ?? '').isNotEmpty) 'imageVariant': _variants[id],
      'name': _name.text.trim(),
      'path': _path.text.trim(),
      if (_taskId != null) 'taskId': _taskId,
    };
  }

  Future<void> _perform(String type, List<Map<String, dynamic>> chosen) async {
    if (_busy || chosen.isEmpty || scoped && chosen.length != 1) return;
    setState(() {
      _busy = true;
      _notice = '';
    });
    final messages = <String>[];
    try {
      for (final item in chosen) {
        if (_requiresInspect(item)) await _inspect(item);
        final options = _options(item);
        final result = await widget.media
            .send(session, type, itemId: item['id'], options: options);
        if (!mounted || session.closed) return;
        if (type == 'CREATE' &&
            (result is! Map ||
                !const ['accepted', 'created'].contains(result['state']) ||
                result['state'] == 'created' &&
                    (result['taskIds'] is! List ||
                        (result['taskIds'] as List).isEmpty))) {
          throw StateError('尚未确认任务创建结果，请先检查下载任务，避免重复添加。');
        }
        messages.add(type == 'UPDATE'
            ? '已更新并继续'
            : result is Map && result['state'] == 'accepted'
                ? '等待确认，请在 kownload 中完成操作'
                : result is Map && result['duplicate'] == true
                    ? '此资源已在 kownload 中'
                    : '已交给 kownload 下载');
      }
      if (mounted) setState(() => _notice = messages.join('；'));
    } catch (error) {
      _failed(error);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _refresh() async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      await widget.media.send(session, 'LIST');
      if (mounted)
        setState(() {
          _selected.removeWhere((id) => !items.any((i) => i['id'] == id));
        });
    } catch (e) {
      _failed(e);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _deepSearch() async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      await widget.media.send(session, 'DEEP_SEARCH');
      if (mounted) setState(() => _notice = '增强识别已开启，请回到网页重新播放，再刷新这里并确认新候选。');
    } catch (e) {
      _failed(e);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _findTasks(Map<String, dynamic> item) async {
    setState(() => _busy = true);
    try {
      final value =
          await widget.media.send(session, 'RECOVERABLE', itemId: item['id']);
      if (mounted)
        setState(() {
          _tasks = _records(value);
          _taskId = null;
          _recoverFor = item['id'];
          if (_tasks.isEmpty) _notice = '本页没有同协议的暂停或失败任务。';
        });
    } catch (e) {
      _failed(e);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  List<Map<String, dynamic>> get _visible => scoped
      ? items
      : items.where((item) {
          final small = item['kind'] == 'image' &&
              item['width'] is num &&
              item['height'] is num &&
              item['width'] > 0 &&
              item['height'] > 0 &&
              item['width'] < 128 &&
              item['height'] < 128;
          return (_kind == 'all' || item['kind'] == _kind) &&
              (!small || widget.showSmallImages) &&
              '${item['title'] ?? ''} ${item['displayUrl'] ?? item['url'] ?? ''}'
                  .toString()
                  .toLowerCase()
                  .contains(_filter.text.trim().toLowerCase());
        }).toList();

  Widget _preview(Map item, {double width = 48, double height = 42}) {
    final url = item['previewUrl'];
    final headers = item['previewHeaders'];
    final icon = Icon(
        item['kind'] == 'image'
            ? CupertinoIcons.photo
            : item['kind'] == 'audio'
                ? CupertinoIcons.music_note
                : CupertinoIcons.play_rectangle,
        size: 22,
        color: MacColors.of(context).secondary);
    if (url is! String || !RegExp(r'^https?://').hasMatch(url))
      return SizedBox(width: width, height: height, child: icon);
    return ClipRRect(
        borderRadius: BorderRadius.circular(5),
        child: Image.network(url,
            headers: headers is Map ? Map<String, String>.from(headers) : null,
            width: width,
            height: height,
            fit: BoxFit.contain,
            errorBuilder: (_, __, ___) =>
                SizedBox(width: width, height: height, child: icon)));
  }

  Widget _dropdown(String label, String value, Map<String, String> choices,
          ValueChanged<String> change) =>
      Padding(
          padding: const EdgeInsets.only(top: 8),
          child: Row(children: [
            SizedBox(
                width: 64,
                child: Text(label, style: const TextStyle(fontSize: 12))),
            Expanded(
                child: DropdownButtonFormField<String>(
              initialValue: choices.containsKey(value) ? value : null,
              key: ValueKey('$label-$value-${choices.keys.join(',')}'),
              isExpanded: true,
              isDense: true,
              iconSize: 16,
              style: TextStyle(
                  fontFamily: '.AppleSystemUIFont',
                  fontSize: 13,
                  height: 1.15,
                  fontWeight: FontWeight.w400,
                  color: MacColors.of(context).text),
              hint: const Text('请选择'),
              decoration: const InputDecoration(
                  constraints: BoxConstraints(minHeight: 30, maxHeight: 32),
                  isDense: true,
                  contentPadding:
                      EdgeInsets.symmetric(horizontal: 10, vertical: 6)),
              items: [
                for (final entry in choices.entries)
                  DropdownMenuItem(
                      value: entry.key,
                      child: Text(entry.value, overflow: TextOverflow.ellipsis))
              ],
              onChanged: _busy
                  ? null
                  : (value) {
                      if (value != null) change(value);
                    },
            )),
          ]));
  List<Widget> _controls(Map<String, dynamic> item) {
    final id = item['id'] as String, info = _inspected[id];
    if (info == null) return [];
    final options = _records(info['options']);
    final selected = options.where((o) => o['id'] == _formats[id]).firstOrNull;
    final tracks = _records(info['audioTracks']);
    final subs = _records(info['subtitles']);
    final variants = _records(info['variants']);
    return [
      if (options.isNotEmpty)
        _dropdown(
            '下载格式',
            _formats[id] ?? '',
            {for (final f in options) f['id']: f['label'] ?? f['id']},
            (value) => setState(() => _formats[id] = value)),
      if (_advanced && tracks.isNotEmpty && selected?['audioOnly'] != true)
        _dropdown(
            '音轨',
            _audio[id] ?? '',
            {
              '': '自动选择音轨',
              for (final t in tracks)
                t['id']: t['label'] ?? t['language'] ?? t['id']
            },
            (value) => setState(() => _audio[id] = value)),
      if (_advanced && subs.isNotEmpty && selected?['audioOnly'] != true)
        _dropdown(
            '字幕',
            _subtitles[id] ?? '',
            {
              '': '不下载字幕',
              for (final s in subs)
                '${s['language']}|${s['automatic'] == true ? 'auto' : 'manual'}':
                    '${s['name'] ?? s['language']}${s['automatic'] == true ? ' · 自动字幕' : ''}'
            },
            (value) => setState(() => _subtitles[id] = value)),
      if (_advanced && variants.isNotEmpty)
        _dropdown(
            '图片来源',
            _variants[id] ?? variants.first['id'],
            {for (final v in variants) v['id']: v['label'] ?? v['id']},
            (value) => setState(() => _variants[id] = value)),
    ];
  }

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context), rows = _visible;
    final chosen = items.where((i) => _selected.contains(i['id'])).toList();
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted) _resize(measured: true);
    });
    return Material(
        color: c.background,
        child: Column(children: [
          MacToolbar(
              title: scoped ? '网页视频' : '网页资源',
              leading: IconButton(
                  tooltip: '返回轻面板',
                  icon: const Icon(CupertinoIcons.chevron_back, size: 16),
                  onPressed: () {
                    widget.media.close(session);
                    MacPanel.resize(260);
                  }),
              actions: [
                Text(session.site,
                    style: TextStyle(fontSize: 11, color: c.secondary)),
                IconButton(
                    tooltip: '关闭本站嗅探',
                    onPressed: _busy || widget.disableSite == null
                        ? null
                        : () async {
                            try {
                              await widget.disableSite!();
                              widget.media.close(session);
                            } catch (error) {
                              _failed(error);
                            }
                          },
                    icon: const Icon(CupertinoIcons.eye_slash, size: 17)),
                IconButton(
                    tooltip: '浏览器设置',
                    icon: const Icon(CupertinoIcons.gear, size: 17),
                    onPressed: () {
                      widget.media.close(session);
                      MacPanel.showSettings(section: 'browser');
                    }),
                IconButton(
                    tooltip: '刷新资源',
                    onPressed: _busy ? null : _refresh,
                    icon: const Icon(CupertinoIcons.arrow_clockwise, size: 17)),
              ]),
          if (!scoped)
            Padding(
                key: _kindKey,
                padding: const EdgeInsets.fromLTRB(20, 10, 20, 0),
                child: Row(children: [
                  for (final entry in const {
                    'all': '全部',
                    'video': '视频',
                    'image': '图片',
                    'audio': '音频'
                  }.entries)
                    Padding(
                        padding: const EdgeInsets.only(right: 6),
                        child: ChoiceChip(
                            label: Text(entry.value,
                                style: TextStyle(
                                    fontSize: 12,
                                    fontWeight: FontWeight.w400,
                                    color: c.text)),
                            shape: const StadiumBorder(),
                            side: BorderSide.none,
                            backgroundColor: c.field,
                            selectedColor: c.text.withValues(alpha: .09),
                            selected: _kind == entry.key,
                            onSelected: _busy
                                ? null
                                : (_) => setState(() => _kind = entry.key))),
                  const Spacer(),
                  TextButton(
                      onPressed: _busy
                          ? null
                          : () {
                              setState(() {
                                final all = rows
                                    .every((i) => _selected.contains(i['id']));
                                for (final item in rows) {
                                  if (all)
                                    _selected.remove(item['id']);
                                  else
                                    _selected.add(item['id']);
                                }
                              });
                              for (final item in rows.where(_requiresInspect)) {
                                unawaited(_inspect(item).catchError(_failed));
                              }
                            },
                      child: const Text('全选当前列表')),
                ])),
          if (!scoped)
            Padding(
                key: _filterKey,
                padding: const EdgeInsets.fromLTRB(20, 8, 20, 0),
                child: TextField(
                    controller: _filter,
                    decoration: const InputDecoration(
                        isDense: true,
                        hintText: '筛选名称或地址',
                        prefixIcon: Icon(CupertinoIcons.search, size: 16)),
                    onChanged: (_) => setState(() {}))),
          Flexible(
              fit: session.view == 'single' ? FlexFit.loose : FlexFit.tight,
              child: rows.isEmpty
                  ? Center(
                      child: Text(
                          session.view == 'identify'
                              ? '还未识别此播放器。可开启增强识别后重新播放。'
                              : '当前分类尚未发现资源。',
                          style: TextStyle(color: c.secondary)))
                  : ListView.separated(
                      shrinkWrap: session.view == 'single',
                      padding: const EdgeInsets.fromLTRB(20, 12, 20, 8),
                      itemCount: rows.length,
                      separatorBuilder: (_, __) =>
                          Divider(height: 8, thickness: .5, color: c.border),
                      itemBuilder: (_, index) {
                        final item = rows[index], id = item['id'] as String;
                        final selected = _selected.contains(id),
                            info = _inspected[id];
                        final variant = _records(info?['variants'])
                            .where((v) => v['id'] == _variants[id])
                            .firstOrNull;
                        final preview = {
                          ...item,
                          if (info != null) ...info,
                          if (variant != null) ...variant
                        };
                        return DecoratedBox(
                            key: _itemKeys.putIfAbsent(id, () => GlobalKey()),
                            decoration: BoxDecoration(
                                color: session.view == 'single'
                                    ? Colors.transparent
                                    : selected
                                        ? c.accent.withValues(
                                            alpha: c.dark ? .10 : .055)
                                        : Colors.transparent,
                                borderRadius: BorderRadius.circular(6),
                                border: Border.all(
                                    color: Colors.transparent, width: .5)),
                            child: Padding(
                                padding: session.view == 'single'
                                    ? EdgeInsets.zero
                                    : const EdgeInsets.symmetric(
                                        horizontal: 12, vertical: 8),
                                child: Column(children: [
                                  Row(children: [
                                    if (session.view != 'single')
                                      Checkbox(
                                          value: selected,
                                          onChanged: _busy
                                              ? null
                                              : (value) =>
                                                  _choose(item, value == true)),
                                    _preview(preview),
                                    const SizedBox(width: 10),
                                    Expanded(
                                        child: InkWell(
                                            onTap: _busy ||
                                                    session.view == 'single'
                                                ? null
                                                : () =>
                                                    _choose(item, !selected),
                                            child: Column(
                                                crossAxisAlignment:
                                                    CrossAxisAlignment.start,
                                                children: [
                                                  Text(
                                                      info?['title'] ??
                                                          item['title'] ??
                                                          '网页资源',
                                                      maxLines: 1,
                                                      overflow:
                                                          TextOverflow.ellipsis,
                                                      style: const TextStyle(
                                                          fontSize: 13,
                                                          fontWeight:
                                                              FontWeight.w500)),
                                                  const SizedBox(height: 3),
                                                  Text(
                                                      [
                                                        const {
                                                              'video': '视频',
                                                              'image': '图片',
                                                              'audio': '音频'
                                                            }[item['kind']] ??
                                                            '资源',
                                                        if (item['width'] !=
                                                                null &&
                                                            item['height'] !=
                                                                null)
                                                          '${item['width']}×${item['height']}',
                                                        if (_mediaTime(info?[
                                                                    'duration'] ??
                                                                item[
                                                                    'duration'])
                                                            .isNotEmpty)
                                                          _mediaTime(info?[
                                                                  'duration'] ??
                                                              item['duration']),
                                                        if (item[
                                                                'association'] ==
                                                            'search')
                                                          '增强发现，待确认'
                                                      ].join(' · '),
                                                      style: TextStyle(
                                                          fontSize: 11,
                                                          color: c.secondary)),
                                                ]))),
                                    if (!scoped && !_requiresInspect(item))
                                      MacButton(
                                          primary: true,
                                          onPressed: _busy
                                              ? null
                                              : () =>
                                                  _perform('CREATE', [item]),
                                          child: const Text('下载此项')),
                                  ]),
                                  if (selected) ..._controls(item)
                                ])));
                      })),
          if (session.view == 'identify')
            Padding(
                padding: const EdgeInsets.symmetric(horizontal: 20),
                child: Align(
                    alignment: Alignment.centerLeft,
                    child: TextButton(
                        onPressed: _busy ? null : _deepSearch,
                        child: const Text('增强识别这个视频')))),
          Padding(
              key: _footerKey,
              padding: const EdgeInsets.fromLTRB(20, 8, 20, 14),
              child: Column(children: [
                Align(
                    alignment: Alignment.centerLeft,
                    child: TextButton.icon(
                        onPressed: () {
                          setState(() => _advanced = !_advanced);
                          _resize();
                        },
                        icon: Icon(
                            _advanced
                                ? CupertinoIcons.chevron_up
                                : CupertinoIcons.chevron_down,
                            size: 12),
                        label: Text(_advanced ? '收起选项' : '更多选项'))),
                if (_advanced)
                  Row(children: [
                    Expanded(
                        child: TextField(
                            controller: _name,
                            decoration: const InputDecoration(
                                isDense: true,
                                labelText: '名称',
                                hintText: '留空使用原名'))),
                    const SizedBox(width: 12),
                    Expanded(
                        child: TextField(
                            controller: _path,
                            decoration: InputDecoration(
                                isDense: true,
                                labelText: '保存位置',
                                hintText: widget.defaultDirectory.isEmpty
                                    ? 'kownload 默认目录'
                                    : widget.defaultDirectory)))
                  ]),
                if (_advanced && chosen.length == 1)
                  ExpansionTile(
                      onExpansionChanged: (open) {
                        _recoveryOpen = open;
                        _resize();
                      },
                      tilePadding: EdgeInsets.zero,
                      title: const Text('更新暂停或失败的任务',
                          style: TextStyle(fontSize: 12)),
                      children: [
                        Row(children: [
                          MacButton(
                              onPressed: _busy
                                  ? null
                                  : () => _findTasks(chosen.single),
                              child: const Text('查找本页任务')),
                          const SizedBox(width: 8),
                          Expanded(
                              child: DropdownButton<String>(
                                  isExpanded: true,
                                  value: _tasks.any((t) => t['id'] == _taskId)
                                      ? _taskId
                                      : null,
                                  hint: const Text('选择任务'),
                                  items: [
                                    for (final task in _tasks)
                                      DropdownMenuItem(
                                          value: task['id'],
                                          child: Text(task['name'] ?? '未命名任务',
                                              overflow: TextOverflow.ellipsis))
                                  ],
                                  onChanged: _busy
                                      ? null
                                      : (id) => setState(() => _taskId = id))),
                          MacButton(
                              onPressed: _busy ||
                                      _taskId == null ||
                                      _recoverFor != chosen.single['id']
                                  ? null
                                  : () => _perform('UPDATE', chosen),
                              child: const Text('更新并继续'))
                        ]),
                      ]),
                const SizedBox(height: 8),
                Row(children: [
                  Expanded(
                      child: Text(
                          _notice.isNotEmpty
                              ? _notice
                              : scoped
                                  ? '仅下载明确选择的这个资源'
                                  : '${chosen.length} 项已选',
                          maxLines: 3,
                          style: TextStyle(fontSize: 12, color: c.secondary))),
                  const SizedBox(width: 12),
                  MacButton(
                      primary: true,
                      onPressed: _busy || chosen.isEmpty
                          ? null
                          : () => _perform('CREATE', chosen),
                      child: Text(_busy
                          ? '正在处理…'
                          : scoped
                              ? '下载这个资源'
                              : '下载已选资源'))
                ]),
              ])),
        ]));
  }
}
