import 'package:flutter/material.dart';

import 'bootstrap.dart';

/// Shown instead of the app when the identity bootstrap stops. Nothing has
/// been opened or resumed at this point; the text may carry commands to copy.
class BootstrapErrorPage extends StatefulWidget {
  final IdentityBootstrapException error;
  final VoidCallback onQuit;

  const BootstrapErrorPage(
      {super.key, required this.error, required this.onQuit});

  @override
  State<BootstrapErrorPage> createState() => _BootstrapErrorPageState();
}

class _BootstrapErrorPageState extends State<BootstrapErrorPage> {
  bool _busy = false;
  bool _saved = false;
  String? _note;

  Future<void> _ignore() async {
    setState(() {
      _busy = true;
      _note = null;
    });
    final saved = await widget.error.ignoreLegacyChanges!();
    if (!mounted) return;
    setState(() {
      _busy = false;
      _saved = saved;
      _note = saved
          ? '已记住：本应用今后不再检查旧目录。请退出后重新打开 kownload。'
          : '没有保存这个选择，请稍后重试，或按上面的命令处理。';
    });
  }

  @override
  Widget build(BuildContext context) {
    final ignore = widget.error.ignoreLegacyChanges;
    return Scaffold(
      body: SingleChildScrollView(
        padding: const EdgeInsets.all(24),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          SelectableText(widget.error.message),
          if (ignore != null) ...[
            const SizedBox(height: 16),
            OutlinedButton(
              onPressed: _busy || _saved ? null : _ignore,
              child: const Text('保留新数据，今后忽略旧目录的变化'),
            ),
          ],
          if (_note != null) ...[
            const SizedBox(height: 12),
            SelectableText(_note!),
          ],
          if (_saved) ...[
            const SizedBox(height: 12),
            FilledButton(onPressed: widget.onQuit, child: const Text('退出')),
          ],
        ]),
      ),
    );
  }
}
