import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import '../app/routes/app_pages.dart';
import 'mac_appearance.dart';
import 'mac_widgets.dart';
import 'mac_panel.dart';

class MacShell extends StatefulWidget {
  final int selectedIndex;
  final Widget child;
  const MacShell({super.key, required this.selectedIndex, required this.child});
  @override
  State<MacShell> createState() => _MacShellState();
}

class _MacShellState extends State<MacShell> {
  late final ValueChanged<String> _commandHandler;
  @override
  void initState() {
    super.initState();
    MacAppearance.state.addListener(_appearanceChanged);
    _commandHandler = (command) {
      if (command == 'newTask') _newTask();
      if (command == 'settings') MacPanel.showSettings();
      if (command == 'search') _search();
      if (command == 'inspector') MacAppearance.inspectorRequests.value++;
      if (command == 'selectAll') {
        final editable = FocusManager.instance.primaryFocus?.context
            ?.findAncestorStateOfType<EditableTextState>();
        if (editable != null) {
          editable.selectAll(SelectionChangedCause.keyboard);
        } else {
          MacAppearance.selectAllRequests.value++;
        }
      }
    };
    MacAppearance.onCommand = _commandHandler;
  }

  @override
  void dispose() {
    MacAppearance.state.removeListener(_appearanceChanged);
    if (identical(MacAppearance.onCommand, _commandHandler)) {
      MacAppearance.onCommand = null;
    }
    super.dispose();
  }

  void _appearanceChanged() {
    if (mounted) setState(() {});
  }

  Future<void> _newTask() async {
    await MacPanel.showPanel();
  }

  void _search() {
    // Get navigation futures complete when the destination is later popped.
    // Dispatch focus after the task view has mounted without waiting for that.
    MacPanel.showSettings(section: 'tasks');
    WidgetsBinding.instance.addPostFrameCallback((_) {
      MacAppearance.searchRequests.value++;
    });
    WidgetsBinding.instance.scheduleFrame();
  }

  static const _sections = <String, String>{
    'tasks': '任务',
    'general': '通用',
    'download': '下载',
    'connections': '连接与能力',
    'advanced': '高级',
  };

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return ValueListenableBuilder<String>(
      valueListenable: MacPanel.settingsSection,
      builder: (context, section, _) {
        final selected = widget.selectedIndex == 0
            ? 'tasks'
            : widget.selectedIndex == 1
                ? 'connections'
                : section;
        WidgetsBinding.instance.addPostFrameCallback((_) {
          if (!mounted) return;
          MacAppearance.setWindowTint(
              c.windowTop.toARGB32(), c.windowBottom.toARGB32());
          MacAppearance.setSidebarWidth(0);
          MacAppearance.setToolbarState(
              section: _sections[selected] ?? '通用',
              sidebarVisible: false,
              dark: c.dark,
              modal: Get.rootDelegate.currentConfiguration?.uri.path ==
                  Routes.CREATE);
        });
        return CallbackShortcuts(
          bindings: {
            const SingleActivator(LogicalKeyboardKey.keyN, meta: true):
                _newTask,
            const SingleActivator(LogicalKeyboardKey.comma, meta: true): () =>
                MacPanel.showSettings(),
            const SingleActivator(LogicalKeyboardKey.keyF, meta: true): _search,
          },
          child: Focus(
            autofocus: true,
            child: Material(
              color: Colors.transparent,
              child: Column(children: [
                Container(
                  height: 44,
                  decoration: BoxDecoration(
                    color: c.background,
                    border: Border(bottom: BorderSide(color: c.border)),
                  ),
                  child: Row(children: [
                    const SizedBox(width: 8),
                    const IconButton(
                      tooltip: '返回轻面板',
                      onPressed: MacPanel.showPanel,
                      icon: Icon(CupertinoIcons.chevron_left, size: 16),
                    ),
                    Expanded(
                      child: SingleChildScrollView(
                        scrollDirection: Axis.horizontal,
                        child: Row(children: [
                          for (final entry in _sections.entries)
                            Padding(
                              padding:
                                  const EdgeInsets.symmetric(horizontal: 2),
                              child: Semantics(
                                selected: selected == entry.key,
                                button: true,
                                child: TextButton(
                                  style: TextButton.styleFrom(
                                    minimumSize: const Size(0, 30),
                                    padding: const EdgeInsets.symmetric(
                                        horizontal: 12),
                                    foregroundColor: selected == entry.key
                                        ? c.accent
                                        : c.text,
                                    backgroundColor: selected == entry.key
                                        ? c.selection
                                        : Colors.transparent,
                                    textStyle: const TextStyle(fontSize: 13),
                                  ),
                                  onPressed: () =>
                                      MacPanel.showSettings(section: entry.key),
                                  child: Text(entry.value),
                                ),
                              ),
                            ),
                        ]),
                      ),
                    ),
                    const SizedBox(width: 8),
                  ]),
                ),
                Expanded(child: widget.child),
              ]),
            ),
          ),
        );
      },
    );
  }
}
