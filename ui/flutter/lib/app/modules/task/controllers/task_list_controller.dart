import 'dart:async';

import 'package:get/get.dart';

import '../../../../api/api.dart';
import '../../../../api/model/task.dart';

abstract class TaskListController extends GetxController {
  List<Status> statuses;
  int Function(Task a, Task b) compare;

  TaskListController(this.statuses, this.compare);

  final tasks = <Task>[].obs;
  final selectedTaskIds = <String>[].obs;
  final isRunning = false.obs;
  final isLoading = true.obs;
  final error = Rx<String?>(null);
  bool _fetching = false;

  late final Timer _timer;

  @override
  void onInit() async {
    super.onInit();

    start();
    _timer = Timer.periodic(const Duration(milliseconds: 1000), (timer) async {
      if (isRunning.value) {
        await getTasksState();
      }
    });
  }

  @override
  void onClose() {
    super.onClose();
    _timer.cancel();
  }

  void start() async {
    isRunning.value = true;
    await getTasksState();
  }

  void stop() {
    isRunning.value = false;
  }

  Future<void> getTasksState() async {
    if (_fetching) return;
    _fetching = true;
    try {
      final result = await getTasks(statuses);
      result.sort(compare);
      tasks.value = result;
      error.value = null;
    } catch (e) {
      error.value = e.toString();
    } finally {
      isLoading.value = false;
      _fetching = false;
    }
  }
}
