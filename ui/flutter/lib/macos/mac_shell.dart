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
    'tasks': '下载列表',
    'general': '通用',
    'download': '下载',
  };

  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return ValueListenableBuilder<String>(
      valueListenable: MacPanel.settingsSection,
      builder: (context, section, _) {
        final selected = widget.selectedIndex == 0
            ? 'tasks'
            : _sections.containsKey(section)
                ? section
                : 'general';
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
                    border:
                        Border(bottom: BorderSide(color: c.border, width: .5)),
                  ),
                  child: Row(children: [
                    const SizedBox(width: 8),
                    const IconButton(
                      tooltip: '返回轻面板',
                      onPressed: MacPanel.showPanel,
                      icon: Icon(CupertinoIcons.chevron_left, size: 14),
                    ),
                    Expanded(
                      child: SingleChildScrollView(
                        scrollDirection: Axis.horizontal,
                        child: Container(
                          padding: const EdgeInsets.all(2),
                          decoration: BoxDecoration(
                            color: c.text.withValues(alpha: .045),
                            borderRadius: BorderRadius.circular(8),
                          ),
                          child: Row(children: [
                            for (final entry in _sections.entries)
                              Padding(
                                padding:
                                    const EdgeInsets.symmetric(horizontal: 1),
                                child: Semantics(
                                  selected: selected == entry.key,
                                  button: true,
                                  child: TextButton(
                                    style: TextButton.styleFrom(
                                      minimumSize: const Size(0, 25),
                                      tapTargetSize:
                                          MaterialTapTargetSize.shrinkWrap,
                                      padding: const EdgeInsets.symmetric(
                                          horizontal: 14),
                                      foregroundColor: selected == entry.key
                                          ? c.text
                                          : c.secondary,
                                      backgroundColor: selected == entry.key
                                          ? c.surface
                                          : Colors.transparent,
                                      shape: RoundedRectangleBorder(
                                          borderRadius:
                                              BorderRadius.circular(6)),
                                      textStyle: TextStyle(
                                          fontFamily: '.AppleSystemUIFont',
                                          fontSize: 12,
                                          fontWeight: selected == entry.key
                                              ? FontWeight.w500
                                              : FontWeight.w400),
                                    ),
                                    onPressed: () => MacPanel.showSettings(
                                        section: entry.key),
                                    child: Text(entry.value),
                                  ),
                                ),
                              ),
                          ]),
                        ),
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
