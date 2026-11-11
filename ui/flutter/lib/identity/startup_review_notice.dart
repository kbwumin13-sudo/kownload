import 'package:flutter/material.dart';
import 'startup_policy.dart';

/// A notice inside the existing App. It does not activate a native window,
/// change preferences, or turn dismissal into execution permission.
class IdentityStartupReviewNotice extends StatelessWidget {
  final IdentityStartupPolicy policy;

  const IdentityStartupReviewNotice({super.key, required this.policy});

  @override
  Widget build(BuildContext context) => AlertDialog(
        title: const Text('导入任务需要逐项确认'),
        content: Text(
            '${policy.reason == 'imported_data' ? '数据已导入。' : '旧迁移标记没有记录任务的执行兼容证明。'}'
            '本次没有自动继续旧任务或恢复上传，也没有自动清理缺失文件的记录。\n\n'
            '原自动开始设置保持不变。你可以查看记录、创建新任务；确认原文件与来源后，'
            '逐项继续当前版本支持的任务。未知状态及待交接任务仍会被拒绝。'
            '关闭此提示不会自动解除限制。'),
        actions: [
          TextButton(
              onPressed: () => Navigator.of(context).pop(),
              child: const Text('查看任务')),
        ],
      );
}
