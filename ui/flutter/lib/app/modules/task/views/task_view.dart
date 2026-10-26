import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:open_filex/open_filex.dart';
import 'package:path/path.dart' as path;

import '../../../../api/api.dart';
import '../../../../api/model/task.dart';
import '../../../../api/model/task_stats.dart';
import '../../../../util/file_explorer.dart';
import '../../../../util/util.dart';
import 'package:gopeed/macos/mac_widgets.dart';
import 'package:gopeed/macos/mac_appearance.dart';
import 'package:gopeed/macos/mac_panel.dart';
import '../../../routes/app_pages.dart';
import '../../../views/copy_button.dart';
import '../../../views/buid_task_list_view.dart';
import '../controllers/task_controller.dart';
import '../controllers/task_downloaded_controller.dart';
import '../controllers/task_downloading_controller.dart';
import 'task_downloaded_view.dart';
import 'task_downloading_view.dart';

class TaskView extends GetView<TaskController> {
  const TaskView({Key? key}) : super(key: key);

  @override
  Widget build(BuildContext context) {
    if (Util.isMacos()) return const _MacTaskView();
    final selectTask = controller.selectTask;

    return DefaultTabController(
      length: 2,
      child: Scaffold(
        key: controller.scaffoldKey,
        appBar: PreferredSize(
          preferredSize: const Size.fromHeight(56),
          child: AppBar(
            bottom: TabBar(
              tabs: const [
                Tab(icon: Icon(Icons.file_download)),
                Tab(icon: Icon(Icons.done)),
              ],
              onTap: (index) {
                if (controller.tabIndex.value != index) {
                  controller.tabIndex.value = index;
                  final downloadingController =
                      Get.find<TaskDownloadingController>();
                  final downloadedController =
                      Get.find<TaskDownloadedController>();
                  switch (index) {
                    case 0:
                      downloadingController.start();
                      downloadedController.stop();
                      break;
                    case 1:
                      downloadingController.stop();
                      downloadedController.start();
                      break;
                  }
                }
              },
            ),
          ),
        ),
        body: const TabBarView(
          children: [TaskDownloadingView(), TaskDownloadedView()],
        ),
        endDrawer: Drawer(
          // Add a ListView to the drawer. This ensures the user can scroll
          // through the options in the drawer if there isn't enough vertical
          // space to fit everything.
          child: Obx(
            () => ListView(
              // Important: Remove any padding from the ListView.
              padding: EdgeInsets.zero,
              children: [
                SizedBox(
                  height: MediaQuery.of(context).padding.top + 65,
                  child: DrawerHeader(
                    child: Text(
                      'taskDetail'.tr,
                      style: Theme.of(context).textTheme.titleLarge,
                    ),
                  ),
                ),
                ListTile(
                  title: Text('taskName'.tr),
                  subtitle: buildTooltipSubtitle(selectTask.value?.name),
                ),
                ListTile(
                  title: Text('taskUrl'.tr),
                  subtitle: buildTooltipSubtitle(
                    selectTask.value?.meta.req.url,
                  ),
                  trailing: CopyButton(selectTask.value?.meta.req.url),
                ),
                ListTile(
                  title: Text('downloadPath'.tr),
                  subtitle: buildTooltipSubtitle(selectTask.value?.explorerUrl),
                  trailing: IconButton(
                    icon: const Icon(Icons.folder_open),
                    onPressed: () {
                      selectTask.value?.explorer();
                    },
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }

  Widget buildTooltipSubtitle(String? text) {
    final showText = text ?? "";
    return Tooltip(
      message: showText,
      child: Text(showText, overflow: TextOverflow.ellipsis),
    );
  }
}

class _MacTaskView extends StatefulWidget {
  const _MacTaskView();

  @override
  State<_MacTaskView> createState() => _MacTaskViewState();
}

class _MacTaskViewState extends State<_MacTaskView> {
  late final MacTaskListController taskList;
  final search = TextEditingController();
  final searchFocus = FocusNode();
  final listFocus = FocusNode();
  List<Task> visibleTasks = const [];
  int filter = 0;
  bool inspectorOpen = false;
  double inspectorWidth = 278;
  bool? _sentInspectorVisible;
  bool? _sentInspectorEnabled;

  @override
  void initState() {
    super.initState();
    taskList = Get.put(MacTaskListController());
    MacAppearance.searchRequests.addListener(_focusSearch);
    MacAppearance.selectAllRequests.addListener(_selectVisible);
    MacAppearance.inspectorRequests.addListener(_toggleInspector);
  }

  void _focusSearch() {
    if (mounted) {
      searchFocus.requestFocus();
    }
  }

  void _selectVisible() {
    if (mounted && listFocus.hasFocus) {
      taskList.selectVisible(visibleTasks.map((task) => task.id).toList());
    }
  }

  void _toggleInspector() {
    if (mounted && Get.find<TaskController>().selectTask.value != null) {
      setState(() => inspectorOpen = !inspectorOpen);
    }
  }

  void _syncInspectorState(bool visible, bool enabled) {
    if (_sentInspectorVisible == visible && _sentInspectorEnabled == enabled) {
      return;
    }
    _sentInspectorVisible = visible;
    _sentInspectorEnabled = enabled;
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted) {
        MacAppearance.setInspectorState(visible: visible, enabled: enabled);
      }
    });
  }

  KeyEventResult _handleListKey(FocusNode node, KeyEvent event) {
    if (event is! KeyDownEvent || !node.hasFocus) return KeyEventResult.ignored;
    final keyboard = HardwareKeyboard.instance;
    final ids = visibleTasks.map((task) => task.id).toList();
    if (keyboard.isMetaPressed && event.logicalKey == LogicalKeyboardKey.keyA) {
      taskList.selectVisible(ids);
      return KeyEventResult.handled;
    }
    if (keyboard.isMetaPressed ||
        keyboard.isControlPressed ||
        keyboard.isAltPressed) {
      return KeyEventResult.ignored;
    }
    final key = event.logicalKey;
    final direction = key == LogicalKeyboardKey.arrowUp
        ? -1
        : key == LogicalKeyboardKey.arrowDown
            ? 1
            : key == LogicalKeyboardKey.home
                ? -2
                : key == LogicalKeyboardKey.end
                    ? 2
                    : null;
    if (direction == null) return KeyEventResult.ignored;
    final id = taskList.moveSelection(ids,
        direction: direction, shift: keyboard.isShiftPressed);
    if (id != null) {
      for (final task in visibleTasks) {
        if (task.id == id) {
          Get.find<TaskController>().selectTask.value = task;
          break;
        }
      }
    }
    return KeyEventResult.handled;
  }

  @override
  void dispose() {
    MacAppearance.searchRequests.removeListener(_focusSearch);
    MacAppearance.selectAllRequests.removeListener(_selectVisible);
    MacAppearance.inspectorRequests.removeListener(_toggleInspector);
    search.dispose();
    searchFocus.dispose();
    listFocus.dispose();
    MacAppearance.setInspectorState(visible: false, enabled: false);
    // Application owns the shared collection used by the lightweight panel.
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final colors = MacColors.of(context);
    final selection = Get.find<TaskController>().selectTask;
    return Focus(
      autofocus: true,
      onKeyEvent: (_, event) {
        if (event is KeyDownEvent &&
            event.logicalKey == LogicalKeyboardKey.escape &&
            inspectorOpen &&
            !searchFocus.hasFocus) {
          setState(() => inspectorOpen = false);
          return KeyEventResult.handled;
        }
        if (event is! KeyDownEvent ||
            !HardwareKeyboard.instance.isMetaPressed) {
          return KeyEventResult.ignored;
        }
        if (event.logicalKey == LogicalKeyboardKey.keyF) {
          searchFocus.requestFocus();
          return KeyEventResult.handled;
        }
        return KeyEventResult.ignored;
      },
      child: LayoutBuilder(builder: (context, pageConstraints) {
        final compact = pageConstraints.maxWidth < 750;
        return MacPage(
          title: '',
          headerHeight: 60,
          leading: SizedBox(
            width: pageConstraints.maxWidth - 148,
            child: MacSearchField(
              controller: search,
              focusNode: searchFocus,
              prominent: true,
              hint: '搜索下载',
              onChanged: (_) => setState(() {}),
            ),
          ),
          bottom: Align(
            alignment: Alignment.centerLeft,
            child: _segments(context, compact),
          ),
          actions: [
            Tooltip(
              message: 'create'.tr,
              child: MacButton(
                icon: CupertinoIcons.add,
                onPressed: () => MacPanel.showPanel(),
                child: const Text('新建'),
              ),
            ),
          ],
          child: Obx(() {
            final all = taskList.tasks.toList();
            final query = search.text.trim();
            final visible = filterMacTasks(all, filter, query);
            visibleTasks = visible;
            final visibleIds = visible.map((task) => task.id).toList();
            final visibleSet = visibleIds.toSet();
            if (taskList.selectedTaskIds
                    .any((id) => !visibleSet.contains(id)) ||
                (taskList.selectionAnchor != null &&
                    !visibleSet.contains(taskList.selectionAnchor)) ||
                (taskList.selectionCursor != null &&
                    !visibleSet.contains(taskList.selectionCursor))) {
              WidgetsBinding.instance.addPostFrameCallback((_) {
                if (mounted) taskList.reconcileSelection(visibleIds);
              });
            }
            final current = selection.value;
            Task? selected;
            for (final task in visible) {
              if (task.id == current?.id) {
                selected = task;
                break;
              }
            }
            if (current != null && selected == null) {
              WidgetsBinding.instance.addPostFrameCallback((_) {
                if (mounted && selection.value?.id == current.id) {
                  selection.value = null;
                  if (inspectorOpen) setState(() => inspectorOpen = false);
                }
              });
            }
            final canShowInspector =
                selected != null && pageConstraints.maxWidth >= 541;
            final maxInspectorWidth =
                (pageConstraints.maxWidth - 301).clamp(240.0, 360.0);
            final shownInspectorWidth =
                inspectorWidth.clamp(240.0, maxInspectorWidth);
            _syncInspectorState(
                inspectorOpen && canShowInspector, selected != null);
            return Column(children: [
              Expanded(
                  child: Row(
                children: [
                  Expanded(
                    child: Container(
                      color: Colors.transparent,
                      child: taskList.isLoading.value
                          ? const Center(child: CupertinoActivityIndicator())
                          : taskList.error.value != null && visible.isEmpty
                              ? MacEmptyState(
                                  icon: CupertinoIcons.exclamationmark_circle,
                                  title: '加载失败',
                                  message: taskList.error.value,
                                  action: MacButton(
                                    onPressed: taskList.getTasksState,
                                    child: const Text('重试'),
                                  ),
                                )
                              : visible.isEmpty
                                  ? MacEmptyState(
                                      icon: query.isEmpty
                                          ? CupertinoIcons.arrow_down_to_line
                                          : CupertinoIcons.search,
                                      title: query.isEmpty ? '暂无任务' : '没有匹配的任务',
                                      message: query.isEmpty
                                          ? '点击新建任务开始下载'
                                          : '试试其他关键词',
                                      action: query.isEmpty
                                          ? MacButton(
                                              primary: true,
                                              icon: CupertinoIcons.add,
                                              onPressed: () => Get.rootDelegate
                                                  .toNamed(Routes.CREATE),
                                              child: Text('create'.tr))
                                          : null,
                                    )
                                  : Column(
                                      children: [
                                        if (taskList.error.value != null)
                                          MaterialBanner(
                                            content: const Text('刷新失败，正在重试'),
                                            actions: [
                                              TextButton(
                                                onPressed:
                                                    taskList.getTasksState,
                                                child: const Text('重试'),
                                              ),
                                            ],
                                          ),
                                        Expanded(
                                          child: Focus(
                                            focusNode: listFocus,
                                            onKeyEvent: _handleListKey,
                                            child: Listener(
                                              behavior: HitTestBehavior.opaque,
                                              onPointerDown: (_) =>
                                                  listFocus.requestFocus(),
                                              child: BuildTaskListView(
                                                tasks: visible,
                                                selectedTaskIds:
                                                    taskList.selectedTaskIds,
                                                macos: true,
                                              ),
                                            ),
                                          ),
                                        ),
                                      ],
                                    ),
                    ),
                  ),
                  if (inspectorOpen && canShowInspector) ...[
                    Container(width: 1, color: colors.border),
                    SizedBox(
                      width: shownInspectorWidth,
                      child: Row(
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: [
                            MouseRegion(
                              cursor: SystemMouseCursors.resizeLeftRight,
                              child: GestureDetector(
                                behavior: HitTestBehavior.opaque,
                                onHorizontalDragUpdate: (details) =>
                                    setState(() {
                                  inspectorWidth =
                                      (inspectorWidth - details.delta.dx)
                                          .clamp(240.0, maxInspectorWidth);
                                }),
                                child: const SizedBox(width: 6),
                              ),
                            ),
                            Expanded(
                                child: _MacTaskInspector(
                              task: selected,
                              onClose: _toggleInspector,
                            )),
                          ]),
                    ),
                  ],
                ],
              )),
              Container(
                height: 28,
                padding: const EdgeInsets.symmetric(horizontal: 20),
                decoration: BoxDecoration(
                    border: Border(
                        top: BorderSide(color: colors.border, width: .5))),
                child: Row(children: [
                  Text('${visible.length} 项',
                      style: TextStyle(fontSize: 11, color: colors.secondary)),
                  const Spacer(),
                  Text('双击打开  ·  右键查看更多操作',
                      style: TextStyle(fontSize: 11, color: colors.secondary)),
                ]),
              ),
            ]);
          }),
        );
      }),
    );
  }

  Widget _segments(BuildContext context, bool compact) {
    final c = MacColors.of(context);
    return Row(mainAxisSize: MainAxisSize.min, children: [
      for (final entry in <int, String>{2: '全部', 0: '进行中', 1: '已完成'}.entries)
        Padding(
          padding: const EdgeInsets.only(right: 4),
          child: Semantics(
            selected: filter == entry.key,
            button: true,
            child: TextButton(
              onPressed: () => setState(() => filter = entry.key),
              style: TextButton.styleFrom(
                minimumSize: const Size(0, 26),
                tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                padding: const EdgeInsets.symmetric(horizontal: 11),
                shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(7)),
                backgroundColor:
                    filter == entry.key ? c.selection : Colors.transparent,
                foregroundColor: filter == entry.key ? c.accent : c.secondary,
                textStyle: TextStyle(
                    fontSize: 12,
                    fontWeight: filter == entry.key
                        ? FontWeight.w500
                        : FontWeight.w400),
              ),
              child: Text(entry.value),
            ),
          ),
        ),
    ]);
  }
}

class _MacTaskInspector extends StatelessWidget {
  final Task task;
  final VoidCallback onClose;
  const _MacTaskInspector({required this.task, required this.onClose});

  @override
  Widget build(BuildContext context) {
    final colors = MacColors.of(context);
    Widget field(String label, String value, {Widget? action}) => Padding(
          padding: const EdgeInsets.only(bottom: 18),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(label,
                  style: TextStyle(color: colors.secondary, fontSize: 12)),
              const SizedBox(height: 5),
              Row(
                children: [
                  Expanded(
                    child: SelectableText(
                      value,
                      style: TextStyle(color: colors.text, fontSize: 13),
                    ),
                  ),
                  if (action != null) action,
                ],
              ),
            ],
          ),
        );
    return Container(
      color: colors.surface,
      padding: const EdgeInsets.all(18),
      child: ListView(
        children: [
          Row(
            children: [
              Expanded(
                child: Text(
                  'taskDetail'.tr,
                  style: TextStyle(
                    color: colors.text,
                    fontSize: 15,
                    fontWeight: FontWeight.w600,
                  ),
                ),
              ),
              IconButton(
                icon: const Icon(CupertinoIcons.xmark, size: 17),
                onPressed: onClose,
              ),
            ],
          ),
          const SizedBox(height: 20),
          field('taskName'.tr, task.name),
          field(
            'taskUrl'.tr,
            task.meta.req.url,
            action: CopyButton(task.meta.req.url),
          ),
          field(
            'downloadPath'.tr,
            task.explorerUrl,
            action: IconButton(
              icon: const Icon(CupertinoIcons.folder, size: 18),
              onPressed: task.explorer,
            ),
          ),
          if (task.protocol == null ||
              task.protocol == Protocol.http ||
              task.protocol == Protocol.netdisk)
            _MacTaskDiagnostics(key: ValueKey(task.id), task: task),
        ],
      ),
    );
  }
}

/// Explains how a download is running: phase, streams in use against the
/// limit, whether the server allows segments and why, and the latest failure.
/// Network details stay folded until asked for.
class _MacTaskDiagnostics extends StatefulWidget {
  final Task task;
  const _MacTaskDiagnostics({super.key, required this.task});

  @override
  State<_MacTaskDiagnostics> createState() => _MacTaskDiagnosticsState();
}

class _MacTaskDiagnosticsState extends State<_MacTaskDiagnostics> {
  TaskStats? stats;
  bool details = false;
  Timer? timer;

  @override
  void initState() {
    super.initState();
    _load();
    timer = Timer.periodic(const Duration(seconds: 1), (_) {
      if (widget.task.status == Status.running) _load();
    });
  }

  Future<void> _load() async {
    try {
      final next = await getTaskStats(widget.task.id);
      if (mounted) setState(() => stats = next);
    } catch (_) {
      // Older cores or removed tasks have no diagnostics to show.
    }
  }

  @override
  void dispose() {
    timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final s = stats;
    if (s == null) return const SizedBox.shrink();
    final colors = MacColors.of(context);
    Widget row(String label, String? value) => value == null || value.isEmpty
        ? const SizedBox.shrink()
        : Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                SizedBox(
                  width: 72,
                  child: Text(label,
                      style: TextStyle(color: colors.secondary, fontSize: 12)),
                ),
                Expanded(
                  child: SelectableText(value,
                      style: TextStyle(color: colors.text, fontSize: 12)),
                ),
              ],
            ),
          );
    String? ms(String key) {
      final v = s.timings[key];
      if (v == null || v <= 0) return null;
      return v >= 1000 ? '${(v / 1000).toStringAsFixed(1)} 秒' : '$v 毫秒';
    }

    final running = s.runningStreams;
    final limit = s.streamLimit;
    final streams = running == null
        ? null
        : limit == null
            ? '$running'
            : '$running / $limit';
    final reused = s.reusedConnections;
    final created = s.newConnections;
    final timing = [
      if (ms('queueMs') != null) '排队 ${ms('queueMs')}',
      if (s.linkMs != null && s.linkMs! > 0)
        '取链 ${s.linkMs! >= 1000 ? '${(s.linkMs! / 1000).toStringAsFixed(1)} 秒' : '${s.linkMs} 毫秒'}',
      if (ms('resolveMs') != null) '首个响应 ${ms('resolveMs')}',
      if (ms('connectMs') != null) '建连 ${ms('connectMs')}',
      if (ms('tlsMs') != null) 'TLS ${ms('tlsMs')}',
      if (ms('firstByteMs') != null) '首字节 ${ms('firstByteMs')}',
      if (ms('transferMs') != null) '传输 ${ms('transferMs')}',
      if (ms('cooldownMs') != null) '限流等待 ${ms('cooldownMs')}',
    ].join('，');
    return Padding(
      padding: const EdgeInsets.only(bottom: 18),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('下载诊断', style: TextStyle(color: colors.secondary, fontSize: 12)),
          const SizedBox(height: 8),
          row('阶段', s.netdiskPhase ?? s.phaseLabel),
          row('下载流', streams),
          row('分段', s.rangeLabel),
          row('最近错误', s.lastError),
          GestureDetector(
            onTap: () => setState(() => details = !details),
            child: Row(
              children: [
                Icon(
                    details
                        ? CupertinoIcons.chevron_down
                        : CupertinoIcons.chevron_right,
                    size: 12,
                    color: colors.secondary),
                const SizedBox(width: 4),
                Text('网络细节',
                    style: TextStyle(color: colors.secondary, fontSize: 12)),
              ],
            ),
          ),
          if (details) ...[
            const SizedBox(height: 8),
            row('协议', s.protocol),
            row(
                '连接',
                created == null && reused == null
                    ? null
                    : '新建 ${created ?? 0}，复用 ${reused ?? 0}'),
            row('请求', s.requests?.toString()),
            row('限流', s.rateLimits == null ? null : '${s.rateLimits} 次'),
            row('耗时', timing),
            row(
                '自适应',
                s.adaptivePhase == null
                    ? null
                    : '${s.adaptivePhase}${s.adaptiveTarget != null && s.adaptiveTarget! > 0 ? '，目标 ${s.adaptiveTarget}' : ''}'),
          ],
        ],
      ),
    );
  }
}

extension TaskEnhance on Task {
  bool get isFolder {
    return meta.res?.name.isNotEmpty ?? false;
  }

  String get explorerUrl {
    return path.join(Util.safeDir(meta.opts.path), Util.safeDir(name));
  }

  Future<void> explorer() async {
    if (Util.isDesktop()) {
      await FileExplorer.openAndSelectFile(explorerUrl);
    } else {
      Get.rootDelegate.toNamed(Routes.TASK_FILES, parameters: {'id': id});
    }
  }

  Future<void> open() async {
    if (status != Status.done) {
      return;
    }

    if (isFolder) {
      await explorer();
    } else {
      await OpenFilex.open(explorerUrl);
    }
  }
}
