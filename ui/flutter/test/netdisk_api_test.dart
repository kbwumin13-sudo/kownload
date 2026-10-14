import 'package:dio/dio.dart' hide Options;
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/netdisk.dart';

import 'netdisk_test_support.dart';

Response response(dynamic data, {int code = 0, String msg = ''}) => Response(
    requestOptions: RequestOptions(path: '/mock'),
    data: {'code': code, 'msg': msg, 'data': data});

void main() {
  test(
      'recovery reads and continues one existing task without replacing its request',
      () async {
    final original = mockExistingTask();
    original.id = 'task/with space';
    final calls = <(String, String, dynamic)>[];
    final api = NetdiskApi(
        forward: (path, {required method, data, queryParameters}) async {
      calls.add((path, method, data));
      return response(method == 'GET' ? original.toJson() : null);
    });
    final saved = await api.getTask(original.id);
    await api.continueTask(saved.id);
    expect(saved.toJson(), original.toJson());
    expect(calls, [
      ('/api/v1/tasks/task%2Fwith%20space', 'GET', null),
      ('/api/v1/tasks/task%2Fwith%20space/continue', 'PUT', null),
    ]);
  });

  test('all nine page families and 123 aliases are recognized precisely', () {
    final pages = {
      'baidu': 'https://pan.baidu.com/s/abc',
      'aliyun': 'https://www.alipan.com/s/abc',
      'mcloud': 'https://yun.139.com/shareweb/#/w/i/abc',
      'tcloud': 'https://cloud.189.cn/web/share?code=abc',
      'xunlei': 'https://pan.xunlei.com/s/abc',
      'quark': 'https://pan.quark.cn/s/abc',
      'uc': 'https://drive.uc.cn/s/abc',
      '123pan': 'https://www.123912.com/s/abc',
      'guangya': 'https://www.guangyapan.com/s/abc',
    };
    for (final entry in pages.entries) {
      expect(netdiskProviderForUrl(entry.value), entry.key);
      expect(
          parseNetdiskInputs('分享链接：${entry.value} 提取码：A1b2')
              .single
              .source
              .password,
          'A1b2');
    }
    for (final domain in [
      '123pan.cn',
      '123684.com',
      '123865.com',
      '123952.com'
    ]) {
      expect(netdiskProviderForUrl('https://www.$domain/s/abc'), '123pan');
    }
    expect(
        netdiskProviderForUrl('https://www.aliyundrive.com/drive'), 'aliyun');
    expect(netdiskProviderForUrl('https://caiyun.139.com/'), 'mcloud');
    for (final url in [
      'https://pan.baidu.com.evil.test/s/a',
      'https://evilpan.baidu.com/s/a',
      'https://evil.test/?next=https://pan.quark.cn/s/a',
      'https://user:password@pan.baidu.com/s/a',
      'file://pan.baidu.com/s/a',
      'https://api.aliyundrive.com/v2/file/list',
    ]) {
      expect(netdiskProviderForUrl(url), isNull, reason: url);
    }
  });

  test('share prose and multiline mixes keep each extraction code in memory',
      () {
    const text = '给你两个分享：\n链接：https://pan.baidu.com/s/first，提取码：abcd\n'
        'https://example.com/file.zip\n'
        'https://pan.quark.cn/s/second?pwd=WXYZ 密码：zzzz\n'
        'https://www.alipan.com/s/third。访问码：C123';
    final inputs = parseNetdiskInputs(text);
    expect(inputs.map((i) => i.source.provider), ['baidu', 'quark', 'aliyun']);
    expect(inputs.map((i) => i.source.password), ['abcd', 'WXYZ', 'C123']);
    expect(inputs.map((i) => i.source.shareId), ['first', 'second', 'third']);
    expect(inputs.first.source.url, 'https://pan.baidu.com/s/first');
    expect(
        ordinaryNetdiskCompanionLinks(text), ['https://example.com/file.zip']);
  });

  test(
      'native captured selection validates page identity and preserves folders',
      () {
    final request = Request(
        url: 'https://pan.quark.cn/s/share',
        protocol: 'netdisk',
        extra: {
          'provider': 'quark',
          'source': {'url': 'https://pan.quark.cn/s/share', 'password': 'abcd'},
          'sessionId': 'session',
          'files': [
            {'id': 'folder', 'name': '资料', 'size': 0, 'isDir': true},
            {'id': 'file', 'name': '报告.pdf', 'size': 123, 'isDir': false},
          ]
        });
    final input = NetdiskInput.fromRequest(request);
    expect(input.source.provider, 'quark');
    expect(input.sessionId, 'session');
    expect(input.files.first.isDir, isTrue);
    request.extra = {
      'provider': 'baidu',
      'source': {'url': request.url}
    };
    expect(() => NetdiskInput.fromRequest(request),
        throwsA(isA<NetdiskException>()));
  });

  test('wrapper unwraps raw responses and sends the fixed REST shapes',
      () async {
    final calls = <(String, String, dynamic)>[];
    final source =
        parseNetdiskInputs('https://pan.quark.cn/s/a 提取码：abcd').single.source;
    final api = NetdiskApi(
        forward: (path, {required method, data, queryParameters}) async {
      calls.add((path, method, data));
      if (path.endsWith('/providers')) {
        return response([
          {
            'id': 'quark',
            'name': '夸克网盘',
            'hosts': ['pan.quark.cn'],
            'apiHosts': ['drive-pc.quark.cn']
          }
        ]);
      }
      if (path.endsWith('/browser/request') || method == 'GET') {
        return response(
            {'id': 'request', 'state': 'waiting', 'source': source.toJson()});
      }
      if (method == 'DELETE') return response(null);
      if (path.endsWith('/list')) {
        return response({
          'source': source.toJson(),
          'sessionId': 'session',
          'files': [],
          'nextPage': 'page2'
        });
      }
      if (path.endsWith('/prepare')) {
        return response({
          'batchId': 'batch',
          'reqs': [
            {
              'req': {
                'url': source.url,
                'protocol': 'netdisk',
                'extra': {'batchId': 'batch'}
              },
              'opts': {'path': '/downloads'}
            }
          ]
        });
      }
      return response(['task']);
    });
    expect((await api.providers()).single.apiHosts, ['drive-pc.quark.cn']);
    await api.requestBrowser(source, instanceId: 'instance');
    await api.browserStatus('request');
    await api.cancelBrowser('request');
    final listing = await api.list(source.inDirectory('folder'), 'session',
        cursor: 'page1');
    expect(listing.nextPage, 'page2');
    final prepared = await api.prepare(
        source,
        'session',
        [const NetdiskFile(id: 'f', name: 'file', isDir: true)],
        Options(path: '/downloads'),
        proxy: RequestProxy(mode: RequestProxyMode.none));
    expect(await api.createBatch(prepared), ['task']);
    expect(calls.map((c) => c.$1), [
      '/api/v1/netdisk/providers',
      '/api/v1/netdisk/browser/request',
      '/api/v1/netdisk/browser/requests/request',
      '/api/v1/netdisk/browser/requests/request',
      '/api/v1/netdisk/list',
      '/api/v1/netdisk/prepare',
      '/api/v1/tasks/batch',
    ]);
    expect(calls[1].$3, {'source': source.toJson(), 'instanceId': 'instance'});
    expect(calls[4].$3['source']['parentId'], 'folder');
    expect(calls[4].$3['cursor'], 'page1');
    expect(calls[5].$3['files'][0]['isDir'], true);
    expect(calls[5].$3['proxy']['mode'], 'none');
    expect(calls.last.$3, {'reqs': prepared.reqs});
  });

  test('reasonCode and partial batch data survive HTTP and standard errors',
      () async {
    for (final httpError in [false, true]) {
      final api = NetdiskApi(
          forward: (path, {required method, data, queryParameters}) async {
        final resp =
            response({'reasonCode': 'login_required'}, code: 1, msg: '请先登录');
        if (httpError) {
          throw DioException(
              requestOptions: resp.requestOptions,
              response: resp,
              type: DioExceptionType.badResponse);
        }
        return resp;
      });
      await expectLater(
          api.providers(),
          throwsA(isA<NetdiskException>()
              .having((e) => e.reasonCode, 'reason', 'login_required')));
    }
    final api = NetdiskApi(
        forward: (path, {required method, data, queryParameters}) async =>
            response({
              'reasonCode': 'partial_batch',
              'taskIds': ['created']
            }, code: 1, msg: '第二项失败'));
    final prepared = NetdiskPrepared.fromJson({
      'batchId': 'b',
      'reqs': [{}, {}]
    });
    await expectLater(
        api.createBatch(prepared),
        throwsA(isA<NetdiskException>()
            .having((e) => e.data['taskIds'], 'created IDs', ['created'])));
    final incomplete = NetdiskApi(
        forward: (path, {required method, data, queryParameters}) async =>
            response(['one']));
    await expectLater(
        incomplete.createBatch(prepared),
        throwsA(isA<NetdiskException>()
            .having((e) => e.reasonCode, 'reason', 'partial_batch')));
  });

  test('netdisk task protocol round-trips alongside the existing protocols',
      () {
    final task = Task(
        id: 't',
        name: '报告.pdf',
        meta: Meta(
            req: Request(url: 'https://pan.quark.cn/s/a', protocol: 'netdisk'),
            opts: Options()),
        status: Status.ready,
        uploading: false,
        progress: Progress(
            used: 0, speed: 0, downloaded: 0, uploadSpeed: 0, uploaded: 0),
        createdAt: DateTime.utc(2026),
        updatedAt: DateTime.utc(2026));
    for (final protocol in Protocol.values) {
      task.protocol = protocol;
      expect(Task.fromJson(task.toJson()).protocol, protocol);
    }
  });
}
