import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:gopeed/api/model/task.dart';
import 'task_list_controller.dart';

class TaskController extends GetxController {
  final tabIndex = 0.obs;
  final scaffoldKey = GlobalKey<ScaffoldState>();
  final selectTask = Rx<Task?>(null);

  @override
  void onInit() {
    super.onInit();
    if (kIsWeb) {
      BrowserContextMenu.disableContextMenu();
    }
  }

  @override
  void onClose() {
    super.onClose();
    if (kIsWeb) {
      BrowserContextMenu.enableContextMenu();
    }
  }
}

/// One poll owns the complete macOS task collection; filters are local.
class MacTaskListController extends TaskListController {
  MacTaskListController()
      : super([], (a, b) {
          if (a.status == Status.running && b.status != Status.running) {
            return -1;
          }
          if (b.status == Status.running && a.status != Status.running) {
            return 1;
          }
          return b.updatedAt.compareTo(a.updatedAt);
        });

  String? selectionAnchor;
  String? selectionCursor;

  void reconcileSelection(List<String> visibleIds) {
    final visible = visibleIds.toSet();
    final retained = selectedTaskIds.where(visible.contains).toList();
    if (retained.length != selectedTaskIds.length) selectedTaskIds(retained);
    if (!visible.contains(selectionAnchor)) selectionAnchor = null;
    if (!visible.contains(selectionCursor)) selectionCursor = null;
  }

  void selectVisible(List<String> visibleIds) {
    selectedTaskIds(visibleIds);
    selectionAnchor = visibleIds.isEmpty ? null : visibleIds.first;
    selectionCursor = visibleIds.isEmpty ? null : visibleIds.last;
  }

  void selectRow(List<String> visibleIds, String id,
      {bool shift = false, bool toggle = false}) {
    final index = visibleIds.indexOf(id);
    if (index < 0) return;
    if (shift) {
      final anchorIndex = visibleIds.indexOf(selectionAnchor ?? '');
      final start = anchorIndex < 0 ? index : anchorIndex;
      selectionAnchor = visibleIds[start];
      final range = visibleIds.sublist(
          start < index ? start : index, (start > index ? start : index) + 1);
      selectedTaskIds(toggle ? {...selectedTaskIds, ...range}.toList() : range);
    } else if (toggle) {
      final ids = selectedTaskIds.toList();
      ids.contains(id) ? ids.remove(id) : ids.add(id);
      selectedTaskIds(ids);
      selectionAnchor = id;
    } else {
      selectedTaskIds([id]);
      selectionAnchor = id;
    }
    selectionCursor = id;
  }

  String? moveSelection(List<String> visibleIds,
      {required int direction, bool shift = false}) {
    if (visibleIds.isEmpty) return null;
    final current = visibleIds.indexOf(selectionCursor ?? '');
    final target = direction == -2
        ? 0
        : direction == 2
            ? visibleIds.length - 1
            : current < 0
                ? (direction < 0 ? visibleIds.length - 1 : 0)
                : (current + direction).clamp(0, visibleIds.length - 1);
    final id = visibleIds[target];
    if (shift && current >= 0 && selectionAnchor == null) {
      selectionAnchor = visibleIds[current];
    }
    selectRow(visibleIds, id, shift: shift);
    return id;
  }
}

List<Task> filterMacTasks(Iterable<Task> tasks, int filter, String query) {
  final needle = query.trim().toLowerCase();
  return tasks.where((task) {
    final inSegment = filter == 2 ||
        (filter == 1 ? task.status == Status.done : task.status != Status.done);
    return inSegment &&
        (needle.isEmpty ||
            task.name.toLowerCase().contains(needle) ||
            task.meta.req.url.toLowerCase().contains(needle));
  }).toList();
}
