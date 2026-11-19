import 'package:flutter/material.dart';

import '../../../services/batch_preview.dart';
import '../../../../macos/mac_panel.dart';
import 'create_path_intent.dart';

class BatchPreviewSelection {
  final List<BatchPreviewRow> rows;
  final PathIntent path;
  const BatchPreviewSelection(this.rows, this.path);
}

class BatchPreviewDialog extends StatefulWidget {
  final BatchPreviewDraft draft;
  final PathIntent initialPath;
  final bool singleItemOnly;
  final String? singleItemHint;

  const BatchPreviewDialog({
    super.key,
    required this.draft,
    required this.initialPath,
    this.singleItemOnly = false,
    this.singleItemHint,
  });

  @override
  State<BatchPreviewDialog> createState() => _BatchPreviewDialogState();
}

class _BatchPreviewDialogState extends State<BatchPreviewDialog> {
  late PathIntent _path = widget.initialPath;
  late final _validRows = widget.draft.rows
      .where((row) => row.issue == null)
      .toList();
  late final _selected = _validRows.toSet();
  bool _choosingPath = false;

  Future<void> _choosePath() async {
    setState(() => _choosingPath = true);
    try {
      final directory = await MacPanel.chooseDirectory(
        title: '选择批量下载保存位置',
        initialDirectory: _path.path,
      );
      if (mounted && directory != null && directory.startsWith('/')) {
        setState(
          () => _path = PathIntent(
            path: directory,
            origin: PathOrigin.userSelected,
          ),
        );
      }
    } finally {
      if (mounted) setState(() => _choosingPath = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final invalid = widget.draft.rows.length - _validRows.length;
    final allSelected = _selected.length == _validRows.length;
    final tooMany = widget.singleItemOnly && _selected.length > 1;
    return AlertDialog(
      key: const ValueKey('batch-preview'),
      title: const Text('批量下载预览'),
      content: SizedBox(
        width: 475,
        height: 375,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              widget.draft.tooLarge
                  ? '每次最多导入 200 项，文本总计最多 64 KiB。'
                  : '已选 ${_selected.length} / ${_validRows.length} · 重复 ${widget.draft.duplicates} · 无效 $invalid',
            ),
            if (_validRows.isNotEmpty)
              TextButton(
                key: const ValueKey('batch-preview-select-all'),
                onPressed: () => setState(() {
                  if (allSelected) {
                    _selected.clear();
                  } else {
                    _selected.addAll(_validRows);
                  }
                }),
                child: Text(allSelected ? '取消全选' : '全选'),
              ),
            Expanded(
              child: ListView.builder(
                itemCount: widget.draft.rows.length,
                itemBuilder: (context, i) {
                  final row = widget.draft.rows[i];
                  final issue = row.issue;
                  if (issue != null) {
                    return ListTile(
                      dense: true,
                      leading: Text('${row.number}'),
                      title: Text(switch (issue) {
                        BatchLinkIssue.unsupported => '需要 HTTP(S) 下载地址',
                        BatchLinkIssue.malformed => '地址格式无效或含内嵌凭据',
                        BatchLinkIssue.tooLong => '地址过长',
                      }),
                    );
                  }
                  return CheckboxListTile(
                    key: ValueKey('batch-preview-item-${row.number}'),
                    dense: true,
                    controlAffinity: ListTileControlAffinity.leading,
                    value: _selected.contains(row),
                    title: Text(
                      '${row.number}  ${row.label}',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                    subtitle: row.headers.isEmpty
                        ? null
                        : const Text('含匹配的浏览器请求上下文'),
                    onChanged: (selected) => setState(() {
                      if (selected == true) {
                        _selected.add(row);
                      } else {
                        _selected.remove(row);
                      }
                    }),
                  );
                },
              ),
            ),
            if (tooMany) Text(widget.singleItemHint ?? '当前设置仅支持确认一项。'),
            const Divider(),
            Row(
              children: [
                Expanded(
                  child: Text(
                    '${_path.origin == PathOrigin.defaultPath ? '默认目录' : '保存到'}  ${_path.path}',
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                  ),
                ),
                TextButton(
                  key: const ValueKey('batch-preview-directory'),
                  onPressed: _choosingPath ? null : _choosePath,
                  child: const Text('选择目录…'),
                ),
              ],
            ),
          ],
        ),
      ),
      actions: [
        TextButton(
          key: const ValueKey('batch-preview-cancel'),
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('取消'),
        ),
        FilledButton(
          key: const ValueKey('batch-preview-confirm'),
          onPressed:
              _selected.isEmpty ||
                  tooMany ||
                  _choosingPath ||
                  !_path.path.startsWith('/') ||
                  _path.path.contains('\x00')
              ? null
              : () => Navigator.of(context).pop(
                  BatchPreviewSelection(
                    List.unmodifiable(_validRows.where(_selected.contains)),
                    _path,
                  ),
                ),
          child: const Text('确认下载所选'),
        ),
      ],
    );
  }
}
