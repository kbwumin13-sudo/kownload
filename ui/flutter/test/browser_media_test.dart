import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/services/browser_media.dart';
import 'package:gopeed/api/model/http_path_create.dart';
import 'package:gopeed/app/modules/create/views/browser_media_view.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';

Map<String, dynamic> item(String id,
        {String kind = 'image', bool media = false, int variants = 0}) =>
    {
      'id': id,
      'title': '资源$id',
      'kind': kind,
      'mediaRequired': media,
      'variantCount': variants,
      'width': 640,
      'height': 360,
    };
Future<BrowserMedia> open(List<Map<String, dynamic>> items,
    {String view = 'all'}) async {
  final media = BrowserMedia(show: () async {});
  await media.handle({
    'action': 'mediaOpen',
    'sessionId': 'session-one',
    'clientId': 'chrome-one',
    'view': view,
    'site': 'fixture.test',
    'items': items
  });
  return media;
}

Future<Map<String, dynamic>?> poll(BrowserMedia media,
    {String id = 'session-one'}) async {
  final response = await media.handle({'action': 'mediaPoll', 'sessionId': id});
  return response['command'] == null
      ? null
      : Map<String, dynamic>.from(response['command']);
}

Future<void> reply(BrowserMedia media, Map command, dynamic data,
        {bool ok = true}) =>
    media.handle({
      'action': 'mediaResult',
      'sessionId': 'session-one',
      'commandId': command['id'],
      'ok': ok,
      'data': data,
      'error': '资源已失效，请回网页重新捕获。'
    }).then((_) {});
Future<void> mount(WidgetTester tester, BrowserMedia media) async {
  await tester.binding.setSurfaceSize(const Size(640, 700));
  tester.binding.defaultBinaryMessenger
      .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
  addTearDown(() async {
    await tester.pumpWidget(const SizedBox.shrink());
    await tester.binding.setSurfaceSize(null);
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });
  await tester.pumpWidget(MaterialApp(
      theme: macTheme(Brightness.light),
      home: BrowserMediaView(
          media: media,
          session: media.active!,
          defaultDirectory: '/downloads')));
  await tester.pumpAndSettle();
}

void main() {
  test(
      'path claim requires a live declared carrier and owns original confirmation',
      () async {
    final media = await open([item('one', kind: 'video', media: true)]);
    final session = media.active!;
    final options = <String, dynamic>{
      'path': '/tmp/owned-media',
      'name': 'video.mp4',
      'format': 'picked',
      'subtitleLanguages': ['en']
    };
    final intent =
        HttpPathIntent(path: '/tmp/owned-media', origin: 'user_selected');
    await expectLater(
        media.send(session, 'CREATE',
            itemId: 'one', options: options, pathIntent: intent),
        throwsStateError);
    expect(await poll(media), isNull);
    expect(media.hasPendingPathCreate, false);
    await media.handle({
      'action': 'mediaOpen',
      'sessionId': session.id,
      'clientId': session.clientId,
      'view': session.view,
      'items': session.items,
      'pathFreezeVersion': 1
    });
    final sending = media.send(session, 'CREATE',
        itemId: 'one', options: options, pathIntent: intent);
    final stopped = expectLater(sending, throwsStateError);
    final command = (await poll(media))!;
    expect(media.hasPendingPathCreate, true);
    expect(command['pathFreeze'], true);
    expect(command.containsKey('pathIntent'), false);
    options['name'] = 'changed-after-send';
    (command['options'] as Map)['path'] = '/tmp/changed-after-poll';
    final request = <String, dynamic>{
      'req': {
        'url': 'https://fixture.invalid/media',
        'protocol': 'media',
        'extra': {
          'format': 'picked',
          'subtitleLanguages': ['en']
        }
      },
      'opts': {'path': '/tmp/owned-media', 'name': 'video.mp4'},
    };
    Map<String, dynamic> claim(
            {String? sessionId, String op = 'media:original', Map? body}) =>
        media.claimPathCreate(
            sessionId: sessionId ?? session.id,
            commandId: command['id'],
            operationId: op,
            request: body ?? request);
    expect(() => claim(sessionId: 'other-session'), throwsStateError);
    final first = claim();
    expect(first, intent.toJson());
    first['path'] = '/tmp/changed-return-value';
    expect(claim(body: {'opts': request['opts'], 'req': request['req']}),
        intent.toJson());
    expect(() => claim(op: 'media:different'), throwsStateError);
    expect(
        () => claim(body: {
              ...request,
              'req': {
                ...request['req'] as Map,
                'extra': {'format': 'changed'}
              }
            }),
        throwsStateError);
    media.close(session);
    await stopped;
    expect(media.hasPendingPathCreate, false);
    expect(() => claim(), throwsStateError);
    media.dispose();
  });
  test(
      'commands dispatch once, results remain session-bound, and close discards the session',
      () async {
    final media = await open([item('a')]);
    final session = media.active!;
    final future = media.send(session, 'CREATE', itemId: 'a');
    final command = (await poll(media))!;
    expect(command['type'], 'CREATE');
    expect(await poll(media), isNull,
        reason: 'never automatically repeat a download');
    expect(
        (await media.handle({
          'action': 'mediaResult',
          'sessionId': 'other',
          'commandId': command['id'],
          'ok': true
        }))['received'],
        false);
    await reply(media, command, {
      'state': 'created',
      'taskIds': ['one']
    });
    expect((await future)['taskIds'], ['one']);
    media.close(session);
    expect(media.active, isNull);
    expect(session.items, isEmpty);
    expect((await poll(media))!['type'], 'CLOSE');
  });

  testWidgets(
      'direct row action reduces the list interaction to one click without changing batch selection',
      (tester) async {
    final media = await open([item('a'), item('b')]);
    await mount(tester, media);
    expect(
        tester
            .widgetList<Checkbox>(find.byType(Checkbox))
            .every((c) => c.value == false),
        true);
    await tester.tap(find.text('下载此项').first);
    await tester.pump();
    final command = (await poll(media))!;
    expect(command['type'], 'CREATE');
    expect(command['itemId'], 'a');
    expect(
        tester
            .widgetList<Checkbox>(find.byType(Checkbox))
            .every((c) => c.value == false),
        true);
    await reply(media, command, {
      'state': 'created',
      'taskIds': ['a-task']
    });
    await tester.pumpAndSettle();
    expect(find.text('已交给 kownload 下载'), findsOneWidget);
    expect(await poll(media), isNull);
    await tester.tap(find.byType(Checkbox).last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('下载此项').first);
    await tester.pump();
    final direct = (await poll(media))!;
    expect(direct['itemId'], 'a');
    await reply(media, direct, {
      'state': 'created',
      'duplicate': true,
      'taskIds': ['a-task']
    });
    await tester.pumpAndSettle();
    expect(tester.widget<Checkbox>(find.byType(Checkbox).last).value, true);
  });

  testWidgets(
      'media formats, audio and explicit subtitles are returned as opaque options to the original handler',
      (tester) async {
    final media =
        await open([item('video', kind: 'video', media: true)], view: 'single');
    await mount(tester, media);
    final inspect = (await poll(media))!;
    expect(inspect['type'], 'INSPECT');
    await reply(media, inspect, {
      'title': '合成视频',
      'bestId': 'v+a',
      'options': [
        {'id': 'v+a', 'label': '1080p · 含音轨', 'audioOnly': false}
      ],
      'audioTracks': [
        {'id': 'en', 'label': 'English', 'language': 'en'}
      ],
      'subtitles': [
        {'language': 'zh-Hans', 'name': '中文', 'automatic': true}
      ],
    });
    await tester.pumpAndSettle();
    expect(find.text('音轨'), findsNothing);
    expect(find.text('名称'), findsNothing);
    expect(MacPanel.height, inInclusiveRange(240, 280));
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    expect(MacPanel.height, inInclusiveRange(300, 520));
    final controls = tester
        .widgetList<DropdownButtonFormField<String>>(
            find.byType(DropdownButtonFormField<String>))
        .toList();
    expect(controls, hasLength(3));
    for (final field
        in tester.elementList(find.byType(DropdownButtonFormField<String>))) {
      expect((field.renderObject as RenderBox).size.height,
          inInclusiveRange(28, 32));
    }
    final resizeCalls = <MethodCall>[];
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'resizePanel') resizeCalls.add(call);
      return null;
    });
    controls[1].onChanged!('en');
    controls[2].onChanged!('zh-Hans|auto');
    await tester.pumpAndSettle();
    expect(resizeCalls, isEmpty,
        reason: 'unchanged content height must not resize the native window');
    await tester.tap(find.text('下载这个资源'));
    await tester.pump();
    final create = (await poll(media))!;
    expect(create['itemId'], 'video');
    expect(create['options']['format'], 'v+a');
    expect(create['options']['audioId'], 'en');
    expect(create['options']['subtitleLanguages'], ['zh-Hans']);
    expect(create['options']['writeAutomaticSubtitles'], true);
    expect(create['options']['embedSubtitles'], true);
    await reply(media, create, {'state': 'accepted', 'taskIds': []});
    await tester.pumpAndSettle();
    expect(find.textContaining('等待确认'), findsOneWidget);
    expect(find.text('下载此项'), findsNothing);
  });

  testWidgets(
      'unknown tracks require manual choice and an audio-only choice never enables subtitles',
      (tester) async {
    final media =
        await open([item('video', kind: 'video', media: true)], view: 'single');
    await mount(tester, media);
    await reply(media, (await poll(media))!, {
      'bestId': '',
      'options': [
        {'id': 'unknown', 'label': '轨道未确认', 'audioOnly': false},
        {'id': 'audio', 'label': '仅音频', 'audioOnly': true}
      ],
      'subtitles': [
        {'language': 'zh', 'name': '中文', 'automatic': false}
      ],
    });
    await tester.pumpAndSettle();
    await tester.tap(find.text('下载这个资源'));
    await tester.pumpAndSettle();
    expect(await poll(media), isNull);
    expect(find.textContaining('需要手动选择'), findsOneWidget);
    final formats = tester
        .widgetList<DropdownButtonFormField<String>>(
            find.byType(DropdownButtonFormField<String>))
        .first;
    formats.onChanged!('audio');
    await tester.pumpAndSettle();
    expect(find.text('字幕'), findsNothing);
    await tester.tap(find.text('下载这个资源'));
    await tester.pump();
    final create = (await poll(media))!;
    expect(create['options']['format'], 'audio');
    expect(create['options']['embedSubtitles'], false);
    await reply(media, create, {
      'state': 'created',
      'taskIds': ['audio-task']
    });
    await tester.pumpAndSettle();
  });

  testWidgets(
      'batch selection and task recovery keep their original command paths',
      (tester) async {
    final media = await open([item('a'), item('b')]);
    await mount(tester, media);
    await tester.tap(find.text('全选当前列表'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('下载已选资源'));
    await tester.pump();
    final a = (await poll(media))!;
    expect(a['itemId'], 'a');
    await reply(media, a, {
      'state': 'created',
      'taskIds': ['task-a']
    });
    await tester.pump();
    final b = (await poll(media))!;
    expect(b['itemId'], 'b');
    await reply(media, b, {
      'state': 'created',
      'taskIds': ['task-b']
    });
    await tester.pumpAndSettle();
    await tester.tap(find.byType(Checkbox).last);
    await tester.pumpAndSettle();
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('更新暂停或失败的任务'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('查找本页任务'));
    await tester.pump();
    final recover = (await poll(media))!;
    expect(recover['type'], 'RECOVERABLE');
    expect(recover['itemId'], 'a');
    await reply(media, recover, [
      {'id': 'paused-task', 'name': '原暂停任务', 'status': 'pause'}
    ]);
    await tester.pumpAndSettle();
    tester
        .widget<DropdownButton<String>>(find.byType(DropdownButton<String>))
        .onChanged!('paused-task');
    await tester.pumpAndSettle();
    await tester.tap(find.text('更新并继续'));
    await tester.pump();
    final update = (await poll(media))!;
    expect(update['type'], 'UPDATE');
    expect(update['options']['taskId'], 'paused-task');
    await reply(media, update, {'taskId': 'paused-task'});
    await tester.pumpAndSettle();
    expect(find.text('已更新并继续'), findsOneWidget);
  });

  testWidgets(
      'image alternatives retain authenticated previews and opaque selection',
      (tester) async {
    final media = await open([item('photo', variants: 2)]);
    await mount(tester, media);
    await tester.tap(find.byType(Checkbox));
    await tester.pump();
    final inspect = (await poll(media))!;
    await reply(media, inspect, {
      'variants': [
        {'id': 'loaded', 'label': '当前已加载图片'},
        {
          'id': 'alternative',
          'label': '备选图片 1',
          'previewUrl': 'https://fixture.test/image.jpg',
          'previewHeaders': {'Referer': 'https://fixture.test/'}
        }
      ],
    });
    await tester.pumpAndSettle();
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    tester
        .widget<DropdownButtonFormField<String>>(
            find.byType(DropdownButtonFormField<String>))
        .onChanged!('alternative');
    await tester.pumpAndSettle();
    final network =
        tester.widget<Image>(find.byType(Image)).image as NetworkImage;
    expect(tester.widget<Image>(find.byType(Image)).fit, BoxFit.contain);
    expect(network.url, 'https://fixture.test/image.jpg');
    expect(network.headers, {'Referer': 'https://fixture.test/'});
    await tester.tap(find.text('下载已选资源'));
    await tester.pump();
    final create = (await poll(media))!;
    expect(create['options']['imageVariant'], 'alternative');
    expect(create['options'].containsKey('url'), false);
    await reply(media, create, {
      'state': 'created',
      'taskIds': ['photo-task']
    });
    await tester.pumpAndSettle();
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'identify never selects a resource implicitly and keeps enhanced search available',
      (tester) async {
    final media = await open(
        [item('a', kind: 'video'), item('b', kind: 'video')],
        view: 'identify');
    await mount(tester, media);
    expect(
        tester
            .widgetList<Checkbox>(find.byType(Checkbox))
            .every((c) => c.value == false),
        true);
    await tester.tap(find.byType(Checkbox).first);
    await tester.pumpAndSettle();
    await tester.tap(find.byType(Checkbox).last);
    await tester.pumpAndSettle();
    expect(tester.widget<Checkbox>(find.byType(Checkbox).first).value, false);
    expect(tester.widget<Checkbox>(find.byType(Checkbox).last).value, true);
    await tester.tap(find.text('增强识别这个视频'));
    await tester.pump();
    final command = (await poll(media))!;
    expect(command['type'], 'DEEP_SEARCH');
    await reply(media, command, {'started': true});
    await tester.pumpAndSettle();
    expect(find.textContaining('重新播放'), findsOneWidget);
  });
}
