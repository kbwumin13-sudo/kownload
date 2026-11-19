import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/media_tools_status.dart';
import 'package:gopeed/app/modules/setting/views/mac_media_tools_editor.dart';

const _tool = MediaToolStatus(MediaToolState.ready);
const _probe = MediaToolsStatus(
    ytDlp: _tool,
    ffmpeg: _tool,
    node: _tool,
    bridge: MediaBridgeStatus(MediaToolState.ready));

void main() {
  testWidgets('managed operations preserve draft until selecting stable entry',
      (tester) async {
    final calls = <ManagedYtDlpAction>[];
    final draft = MediaConfig(ytDlpPath: '/external/yt-dlp');
    const entry = '/managed/current/active/yt-dlp';
    var failUpdate = false;
    var state = const ManagedYtDlpState();
    Future<ManagedYtDlpState> manage(ManagedYtDlpAction action) async {
      calls.add(action);
      switch (action) {
        case ManagedYtDlpAction.read:
          break;
        case ManagedYtDlpAction.update:
          if (failUpdate) {
            throw const ManagedYtDlpException('managed_ytdlp_signature');
          }
          state = const ManagedYtDlpState(
              entry: entry,
              activeVersion: '2026.10.26',
              previousVersion: '2026.10.01');
        case ManagedYtDlpAction.rollback:
          state = const ManagedYtDlpState(
              entry: entry,
              activeVersion: '2026.10.01',
              previousVersion: '2026.10.26');
      }
      return state;
    }

    await tester.pumpWidget(MaterialApp(
        home: Scaffold(
            body: SingleChildScrollView(
                child: MacMediaToolsEditor(
                    draft: draft,
                    probe: () async => _probe,
                    manage: manage)))));
    await tester.pumpAndSettle();
    expect(calls, [ManagedYtDlpAction.read]);

    Future<void> click(String label) async {
      final item = find.text(label);
      await tester.ensureVisible(item);
      await tester.tap(item);
      await tester.pumpAndSettle();
    }

    await click('安装自管版本');
    expect(calls.last, ManagedYtDlpAction.update);
    expect(draft.ytDlpPath, '/external/yt-dlp');
    expect(find.text('2026.10.26 · 上一版本 2026.10.01'), findsOneWidget);
    failUpdate = true;
    await click('检查并更新自管版本');
    expect(find.text('官方发布清单的签名校验失败。'), findsOneWidget);
    expect(find.text('2026.10.26 · 上一版本 2026.10.01'), findsOneWidget);
    expect(draft.ytDlpPath, '/external/yt-dlp');

    await click('回滚自管版本');
    expect(calls.last, ManagedYtDlpAction.rollback);
    expect(find.text('2026.10.01 · 上一版本 2026.10.26'), findsOneWidget);
    expect(draft.ytDlpPath, '/external/yt-dlp');
    final count = calls.length;
    await click('选用自管入口');
    expect(calls, hasLength(count));
    expect(draft.ytDlpPath, entry);
  });
}
