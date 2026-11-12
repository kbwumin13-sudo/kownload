import 'package:flutter/material.dart';
import 'package:get/get.dart';
import '../../../../api/model/path_policy_config.dart';

/// Edits one ordered Config draft. Evaluation belongs to the active Core.
class PathPolicyEditor extends StatefulWidget {
  final PathPolicyConfig config;
  final ValueChanged<PathPolicyConfig> onChanged;
  const PathPolicyEditor(
      {super.key, required this.config, required this.onChanged});
  @override
  State<PathPolicyEditor> createState() => _PathPolicyEditorState();
}

class _PathPolicyEditorState extends State<PathPolicyEditor> {
  late PathPolicyConfig _config = widget.config;
  void _change(PathPolicyConfig next) {
    setState(() => _config = next);
    widget.onChanged(next);
  }

  void _rule(int index, PathPolicyRule next) {
    final rules = _config.rules.toList()..[index] = next;
    _change(_config.changed(rules: rules));
  }

  void _move(int from, int to) {
    final rules = _config.rules.toList();
    final rule = rules.removeAt(from);
    rules.insert(to, rule);
    _change(_config.changed(rules: rules));
  }

  Widget _field(String label, String value, ValueChanged<String> change,
          {String? hint}) =>
      TextFormField(
        key: ValueKey(label),
        initialValue: value,
        decoration: InputDecoration(labelText: label, hintText: hint),
        onChanged: change,
      );
  @override
  Widget build(BuildContext context) {
    if (!_config.editable) return Text('pathPolicyUnsupportedConfig'.tr);
    final rules = _config.rules;
    return Column(mainAxisSize: MainAxisSize.min, children: [
      SwitchListTile(
        key: const ValueKey('path-policy-enabled'),
        title: Text('pathPolicyEnabled'.tr),
        subtitle: Text('pathPolicyScope'.tr),
        value: _config.enabled,
        onChanged: (value) => _change(_config.changed(enabled: value)),
      ),
      for (var i = 0; i < rules.length; ++i)
        ExpansionTile(
          key: ValueKey(rules[i].id),
          title: Text('${i + 1}. ${rules[i].root}'),
          childrenPadding: const EdgeInsets.all(12),
          children: [
            DropdownButtonFormField<String>(
              initialValue: rules[i].kind,
              decoration: InputDecoration(labelText: 'pathPolicyRuleKind'.tr),
              items: [
                for (final kind in const [
                  'user',
                  'domain',
                  'mime',
                  'extension'
                ])
                  DropdownMenuItem(
                      value: kind, child: Text('pathPolicyKind_$kind'.tr))
              ],
              onChanged: (kind) {
                if (kind == null) return;
                _rule(
                    i,
                    rules[i].changed(
                        kind: kind,
                        domain: kind == 'user' || kind == 'domain'
                            ? rules[i].hint('domain')
                            : '',
                        mime: kind == 'user' || kind == 'mime'
                            ? rules[i].hint('mime')
                            : '',
                        extension: kind == 'user' || kind == 'extension'
                            ? rules[i].hint('extension')
                            : ''));
              },
            ),
            if (rules[i].kind == 'user' || rules[i].kind == 'domain')
              _field('pathPolicyDomain'.tr, rules[i].hint('domain'),
                  (value) => _rule(i, rules[i].changed(domain: value))),
            if (rules[i].kind == 'user' || rules[i].kind == 'mime')
              _field('pathPolicyMIME'.tr, rules[i].hint('mime'),
                  (value) => _rule(i, rules[i].changed(mime: value)),
                  hint: 'video/*'),
            if (rules[i].kind == 'user' || rules[i].kind == 'extension')
              _field('pathPolicyExtension'.tr, rules[i].hint('extension'),
                  (value) => _rule(i, rules[i].changed(extension: value)),
                  hint: '.mp4'),
            _field('downloadDir'.tr, rules[i].root,
                (value) => _rule(i, rules[i].changed(root: value))),
            Row(mainAxisAlignment: MainAxisAlignment.end, children: [
              IconButton(
                  tooltip: 'pathPolicyMoveUp'.tr,
                  onPressed: i == 0 ? null : () => _move(i, i - 1),
                  icon: const Icon(Icons.arrow_upward)),
              IconButton(
                  tooltip: 'pathPolicyMoveDown'.tr,
                  onPressed:
                      i + 1 == rules.length ? null : () => _move(i, i + 1),
                  icon: const Icon(Icons.arrow_downward)),
              IconButton(
                  tooltip: 'delete'.tr,
                  onPressed: () {
                    final next = rules.toList()..removeAt(i);
                    _change(_config.changed(rules: next));
                  },
                  icon: const Icon(Icons.delete_outline)),
            ]),
          ],
        ),
      TextButton.icon(
          onPressed: () => _change(_config.changed(rules: [
                ...rules,
                PathPolicyRule.empty(
                    'rule-${DateTime.now().microsecondsSinceEpoch}')
              ])),
          icon: const Icon(Icons.add),
          label: Text('pathPolicyAddRule'.tr)),
    ]);
  }
}
