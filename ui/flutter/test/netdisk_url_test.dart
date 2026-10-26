import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/netdisk.dart';

void main() {
  final cases = <(String, String, String, String)>[
    ('https://yun.139.com/shareweb/#/w/i/abc', 'mcloud', 'abc', ''),
    (
      'https://caiyun.139.com/shareweb/?password=A1b2#/w/i/abc',
      'mcloud',
      'abc',
      'A1b2'
    ),
    (
      'https://yun.139.com/shareweb/?shareId=wrong&pwd=A1b2#/w/i/abc',
      'mcloud',
      'abc',
      'A1b2'
    ),
    (
      'https://yun.139.com/shareweb/?pass_code=A1b2#/w/i/%61bc',
      'mcloud',
      'abc',
      'A1b2'
    ),
    ('https://pan.baidu.com/share/init?surl=abc', 'baidu', '1abc', ''),
    (
      'https://yun.baidu.com/share/init/?surl=abc&pwd=A1b2',
      'baidu',
      '1abc',
      'A1b2'
    ),
    ('https://pan.baidu.com/s/1abc#pwd=A1b2', 'baidu', '1abc', 'A1b2'),
    ('https://pan.baidu.com/share/init?surl=1abc', 'baidu', '11abc', ''),
    (
      'https://pan.baidu.com/share/init?surl=abc&shareId=wrong&receive_code=A1b2',
      'baidu',
      '1abc',
      'A1b2'
    ),
    ('https://pan.baidu.com/share/init', 'baidu', '', ''),
    ('https://pan.baidu.com/share/init?shareid=1abc', 'baidu', '1abc', ''),
    (
      'https://cloud.189.cn/web/share?code=abc&password=A1b2',
      'tcloud',
      'abc',
      'A1b2'
    ),
    ('https://www.alipan.com/s/abc?pass_code=A1b2', 'aliyun', 'abc', 'A1b2'),
    ('https://www.alipan.com/drive/file/all', 'aliyun', '', ''),
    ('https://alipan.com/drive/file/all', 'aliyun', '', ''),
    ('https://pan.xunlei.com/s/abc?receive_code=A1b2', 'xunlei', 'abc', 'A1b2'),
    (
      'https://pan.quark.cn/s/abc?pwd=A1b2&gopeed_request=public-marker',
      'quark',
      'abc',
      'A1b2'
    ),
    ('https://drive.uc.cn/s/abc?password=A1b2', 'uc', 'abc', 'A1b2'),
    ('https://www.123912.com/s/abc?pwd=A1b2', '123pan', 'abc', 'A1b2'),
    ('https://www.guangyapan.com/s/abc?pwd=A1b2', 'guangya', 'abc', 'A1b2'),
    ('https://yun.139.com/drive#/w/i/abc', 'mcloud', '', ''),
    ('https://pan.quark.cn/shareweb/#/w/i/abc', 'quark', '', ''),
    ('https://pan.quark.cn/share/init?surl=abc', 'quark', 'init', ''),
  ];
  for (final (url, provider, shareId, password) in cases) {
    test('pasted URL identity: $url', () {
      final source = parseNetdiskInputs(url).single.source;
      expect(source.provider, provider);
      expect(source.shareId, shareId);
      expect(source.password, password);
      expect(source.url, url);
    });
  }

  test('existing query aliases and precedence remain equivalent', () {
    for (final shareKey in ['shareId', 'shareid', 'linkID', 'code']) {
      for (final passwordKey in [
        'pwd',
        'password',
        'pass_code',
        'receive_code'
      ]) {
        final source = parseNetdiskInputs(
                'https://pan.quark.cn/web?$shareKey=abc&$passwordKey=A1b2')
            .single
            .source;
        expect(source.shareId, 'abc');
        expect(source.password, 'A1b2');
      }
    }
    final source = parseNetdiskInputs(
            'https://pan.quark.cn/s/abc?pwd=A1b2&password=other&shareId=wrong#pwd=third')
        .single
        .source;
    expect(source.shareId, 'abc');
    expect(source.password, 'A1b2');
  });

  test('Baidu and mobile domain aliases produce equal share identities', () {
    for (final urls in [
      [
        'https://pan.baidu.com/share/init?surl=abc',
        'https://yun.baidu.com/s/1abc'
      ],
      [
        'https://yun.139.com/shareweb/#/w/i/abc',
        'https://caiyun.139.com/shareweb/#/w/i/abc'
      ],
      ['https://cloud.189.cn/web/share?code=abc', 'https://cloud.189.cn/t/abc'],
    ]) {
      final sources =
          urls.map((url) => parseNetdiskInputs(url).single.source).toList();
      expect(sources[0].provider, sources[1].provider);
      expect(sources[0].shareId, sources[1].shareId);
    }
  });

  test('protected domains and URL credentials cannot become netdisk inputs',
      () {
    for (final url in [
      'https://yun.139.com.evil.test/shareweb/#/w/i/abc',
      'https://evilyun.139.com/shareweb/#/w/i/abc',
      'https://pan.baidu.com.evil.test/share/init?surl=abc',
      'https://evilpan.baidu.com/share/init?surl=abc',
      'https://evil.test/?next=https://pan.baidu.com/share/init?surl=abc',
      'https://user:password@pan.baidu.com/share/init?surl=abc',
      'file://pan.baidu.com/share/init?surl=abc',
      'https://api.alipan.com/v2/file/list',
    ]) {
      expect(netdiskProviderForUrl(url), isNull, reason: url);
      expect(parseNetdiskInputs(url), isEmpty, reason: url);
    }
  });

  test('share prose codes stay beside their URL and URL codes take precedence',
      () {
    const text = 'https://yun.139.com/shareweb/#/w/i/abc 提取码：A1b2\n'
        'https://pan.baidu.com/share/init?surl=abc&pwd=WXYZ 密码：zzzz\n'
        'https://example.com/file.zip';
    final sources =
        parseNetdiskInputs(text).map((input) => input.source).toList();
    expect(sources.map((s) => s.shareId), ['abc', '1abc']);
    expect(sources.map((s) => s.password), ['A1b2', 'WXYZ']);
    expect(
        ordinaryNetdiskCompanionLinks(text), ['https://example.com/file.zip']);
  });

  test(
      'native transferred personal source retains its deliberately empty shareId',
      () {
    for (final url in [
      'https://yun.139.com/shareweb/#/w/i/abc',
      'https://pan.baidu.com/share/init?surl=abc'
    ]) {
      final provider = netdiskProviderForUrl(url)!;
      final input = NetdiskInput.fromRequest(
          Request(url: url, protocol: 'netdisk', extra: {
        'provider': provider,
        'source': {
          'provider': provider,
          'url': url,
          'shareId': '',
          'parentId': 'own-copy-folder'
        },
        'sessionId': 'existing-session',
      }));
      expect(input.source.shareId, '');
      expect(input.source.password, '');
      expect(input.source.parentId, 'own-copy-folder');
      expect(input.sessionId, 'existing-session');
    }
  });
}
