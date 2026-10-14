"""Loopback regressions for extractor navigation headers and scope isolation."""
import hashlib
import http.server
import json
import os
import pathlib
import sys
import tempfile
import threading
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import yt_dlp
from yt_dlp.downloader.external import FFmpegFD
from yt_dlp.extractor.bilibili import BilibiliBaseIE
from yt_dlp.extractor.twitter import TwitterBaseIE
from yt_dlp.networking import Request
from yt_dlp.networking.impersonate import ImpersonateTarget
from yt_dlp.utils import DownloadError
from yt_dlp.utils.networking import std_headers


class QuietLogger:
    def debug(self, *args):
        pass

    warning = error = debug


class NavigationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.seen, cls.routes = [], {}
        cls.upstream = {**BilibiliBaseIE._HEADERS, 'User-Agent': 'Extractor fixture/1.0'}
        cls.bili_navigation = {'Referer': 'https://www.bilibili.com/video/BV1t8411b75p',
                               'User-Agent': 'Browser fixture/1.0'}
        cls.media_referers = {'/clip.mp4': cls.upstream['Referer'],
                             '/same-value.mp4': cls.bili_navigation['Referer']}
        # Exercise the installed extractor's API header builder with synthetic
        # credentials only. No real bearer or browser state enters this fixture.
        with patch.object(TwitterBaseIE, '_AUTH', 'fixture'), patch.object(
                TwitterBaseIE, '_get_cookies', return_value={'ct0': SimpleNamespace(value='fixture')}):
            cls.api_headers = TwitterBaseIE()._set_base_headers()
        cls.api_headers['x-guest-token'] = 'fixture'
        sample = os.environ.get('GOPEED_MEDIA_SAMPLE')
        cls.media = pathlib.Path(sample).read_bytes() if sample else b'media-fixture' * 128

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                cls.seen.append((self.path, {k.lower(): v for k, v in self.headers.items()}))
                route = cls.routes.get(self.path)
                if route:
                    self.send_response(302)
                    self.send_header('Location', route)
                    self.send_header('Set-Cookie', 'fixture=local; Path=/private/')
                    data = b''
                elif self.path in cls.media_referers:
                    ok = self.headers.get('Referer') == cls.media_referers[self.path]
                    self.send_response(200 if ok else 403)
                    data = cls.media if ok else b''
                elif self.path == '/api':
                    ok = all(self.headers.get(key) == value for key, value in cls.api_headers.items())
                    self.send_response(200 if ok else 401)
                    data = b'ok' if ok else b''
                else:
                    self.send_response(200)
                    data = b'ok'
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        cls.servers = [http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) for _ in range(2)]
        cls.a, cls.b = [f'http://127.0.0.1:{s.server_port}' for s in cls.servers]
        cls.foreign = cls.b.replace('127.0.0.1', 'localhost')
        for server in cls.servers:
            threading.Thread(target=server.serve_forever, daemon=True).start()
        cls.tmp = tempfile.TemporaryDirectory()
        cls.options = {'quiet': True, 'noprogress': True, 'proxy': '', 'logger': QuietLogger()}
        cls.info = {'id': 'bili-fixture', 'title': 'bili-fixture', 'ext': 'mp4',
                    'url': cls.b + '/clip.mp4', 'http_headers': cls.upstream}
        # Use the installed unmodified yt-dlp downloader as the control.
        with yt_dlp.YoutubeDL({**cls.options, 'outtmpl': cls.tmp.name + '/direct.mp4'}) as ydl:
            ydl.process_info(dict(cls.info))
        cls.direct = pathlib.Path(cls.tmp.name, 'direct.mp4').read_bytes()
        # Bili's format http_headers can repeat the exact browser SourcePage.
        # This is extractor-owned even when both navigation values match scopes.
        cls.same_info = {**cls.info, 'url': cls.b + '/same-value.mp4',
                         'http_headers': cls.bili_navigation}
        with yt_dlp.YoutubeDL({**cls.options, 'outtmpl': cls.tmp.name + '/direct-same.mp4'}) as ydl:
            ydl.process_info(dict(cls.same_info))
        cls.direct_same = pathlib.Path(cls.tmp.name, 'direct-same.mp4').read_bytes()
        with yt_dlp.YoutubeDL(cls.options) as ydl:
            with ydl.urlopen(Request(cls.b + '/fingerprint',
                                    headers={'User-Agent': std_headers['User-Agent']},
                                    extensions={'impersonate': ImpersonateTarget()})) as response:
                response.read()
        cls.curl_user_agent = cls.seen[-1][1]['user-agent']
        with yt_dlp.YoutubeDL(cls.options) as ydl:
            with ydl.urlopen(Request(cls.b + '/api', headers=cls.api_headers)) as response:
                if response.status != 200:
                    raise AssertionError('upstream API fixture rejected extractor headers')
                response.read()
        payload = pathlib.Path(cls.tmp.name, 'context.json')
        cls.browser = {'Referer': cls.a + '/private/page', 'User-Agent': 'Browser fixture/1.0'}
        payload.write_text(json.dumps({'args': [], 'scopes': [
            {'origin': cls.a, 'headers': {**cls.browser, 'Authorization': 'origin-fixture', 'X-Scope': 'origin-fixture'}},
            {'origin': 'https://www.bilibili.com', 'headers': cls.bili_navigation},
            {'url': cls.b + '/exact', 'headers': {'Authorization': 'exact-fixture', 'Referer': cls.a + '/explicit'}},
            {'origin': cls.b, 'pathPrefix': '/private/', 'headers': {'Authorization': 'path-fixture'}},
            {'url': cls.b + '/scoped-end', 'headers': {'Referer': cls.a + '/destination', 'User-Agent': 'Destination fixture/1.0'}},
        ]}))
        source = pathlib.Path(__file__).with_name('context.py').read_text()
        source = source.replace("yt_dlp.main(context['args'] + ['--downloader', 'native'])", '')
        cls.ns = {}
        with patch.object(sys, 'argv', [sys.argv[0], str(payload)]):
            exec(compile(source, 'context.py', 'exec'), cls.ns)

    @classmethod
    def tearDownClass(cls):
        for server in cls.servers:
            server.shutdown()
            server.server_close()
        cls.tmp.cleanup()

    def setUp(self):
        self.seen.clear()
        self.routes.clear()

    def fetch(self, ydl, url, headers, curl):
        request = Request(url, headers=headers,
                          extensions={'impersonate': ImpersonateTarget()} if curl else {})
        with ydl.urlopen(request) as response:
            self.assertEqual(bool(response.extensions.get('impersonate')), curl)
            response.read()
        return request

    def test_bili_headers_download_matches_direct_tool(self):
        self.assertEqual(self.direct, self.media)
        downloaded = []
        for curl in (False, True):
            with self.subTest(curl=curl):
                target = pathlib.Path(self.tmp.name, f'wrapped-{curl}.mp4')
                options = {**self.options, 'outtmpl': str(target)}
                if curl:
                    options['impersonate'] = ImpersonateTarget()
                with yt_dlp.YoutubeDL(options) as ydl:
                    ydl.process_info(dict(self.info))
                self.assertEqual(target.read_bytes(), self.direct)
                downloaded.append(target)
        self.assertEqual(len(downloaded), 2)
        print(f'Bili-header fixture: direct/urllib/curl bytes={len(self.media)}, '
              f'sha256={hashlib.sha256(self.media).hexdigest()}')

    def test_bili_same_value_navigation_matches_direct_tool(self):
        self.assertEqual(self.direct_same, self.media)
        downloaded = []
        for curl in (False, True):
            with self.subTest(curl=curl):
                target = pathlib.Path(self.tmp.name, f'wrapped-same-{curl}.mp4')
                options = {**self.options, 'outtmpl': str(target)}
                if curl:
                    options['impersonate'] = ImpersonateTarget()
                with yt_dlp.YoutubeDL(options) as ydl:
                    ydl.process_info(dict(self.same_info))
                self.assertEqual(target.read_bytes(), self.direct_same)
                for key, value in self.bili_navigation.items():
                    self.assertEqual(self.seen[-1][1][key.lower()], value)
                downloaded.append(target)
        self.assertEqual(len(downloaded), 2)
        print(f'Bili same-value fixture: direct/urllib/curl bytes={len(self.media)}, '
              f'sha256={hashlib.sha256(self.media).hexdigest()}')

    def test_same_value_navigation_not_inherited_on_redirect(self):
        for curl in (False, True):
            for destination in (self.b + '/end', self.foreign + '/end'):
                with self.subTest(curl=curl, destination=destination):
                    self.seen.clear()
                    self.routes.update({'/start': self.b + '/step', '/step': destination})
                    with yt_dlp.YoutubeDL(self.options) as ydl:
                        self.fetch(ydl, self.b + '/start', self.bili_navigation, curl)
                    first, *redirects = [h for _, h in self.seen]
                    for key, value in self.bili_navigation.items():
                        self.assertEqual(first[key.lower()], value)
                        for headers in redirects:
                            self.assertNotEqual(headers.get(key.lower()), value)

    def test_extractor_navigation_is_only_for_explicit_target(self):
        for curl in (False, True):
            for destination in (self.b + '/end', self.foreign + '/end', self.a + '/end', self.b + '/scoped-end'):
                with self.subTest(curl=curl, destination=destination):
                    self.seen.clear()
                    # Even a same-origin redirect must not inherit extractor headers.
                    self.routes.update({'/start': self.b + '/step', '/step': destination})
                    with yt_dlp.YoutubeDL(self.options) as ydl:
                        self.fetch(ydl, self.b + '/start', self.upstream, curl)
                    first, middle, last = [h for _, h in self.seen]
                    self.assertEqual(first['referer'], self.upstream['Referer'])
                    self.assertEqual(first['user-agent'], self.upstream['User-Agent'])
                    self.assertNotIn('referer', middle)
                    self.assertNotEqual(middle.get('user-agent'), self.upstream['User-Agent'])
                    if destination == self.a + '/end':
                        self.assertEqual(last['referer'], self.browser['Referer'])
                        self.assertEqual(last['user-agent'], self.browser['User-Agent'])
                    elif destination.endswith('/scoped-end'):
                        self.assertEqual(last['referer'], self.a + '/destination')
                        self.assertEqual(last['user-agent'], 'Destination fixture/1.0')
                    else:
                        self.assertNotIn('referer', last)
                        self.assertNotEqual(last.get('user-agent'), self.upstream['User-Agent'])

    def test_browser_transport_context_and_credentials_stay_scoped(self):
        # Production browser headers enter only at the transport boundary;
        # they are never seeded into an extractor's Request.headers.
        copied = {'Authorization': 'copied-fixture',
                  'Proxy-Authorization': 'copied-fixture', 'X-Scope': 'copied-fixture'}
        for curl in (False, True):
            with self.subTest(curl=curl):
                self.seen.clear()
                self.routes.update({'/browser': self.b + '/exact',
                                    '/exact': self.b + '/private/step',
                                    '/private/step': self.b + '/outside',
                                    '/outside': self.foreign + '/private/end'})
                with yt_dlp.YoutubeDL(self.options) as ydl:
                    request = self.fetch(ydl, self.a + '/browser', copied, curl)
                    self.assertNotIn('Referer', request.headers)
                    self.assertNotIn('User-Agent', request.headers)
                    self.fetch(ydl, self.foreign + '/direct', {}, curl)
                browser, exact, private, outside, foreign, direct = [h for _, h in self.seen]
                self.assertEqual(browser['referer'], self.browser['Referer'])
                self.assertEqual(browser['user-agent'], self.browser['User-Agent'])
                self.assertEqual(browser['authorization'], 'origin-fixture')
                self.assertEqual(exact['authorization'], 'exact-fixture')
                self.assertEqual(exact['referer'], self.a + '/explicit')
                self.assertEqual(private['authorization'], 'path-fixture')
                self.assertIn('cookie', private)
                for headers in (outside, foreign, direct):
                    for name in ('authorization', 'cookie', 'referer'):
                        self.assertNotIn(name, headers)
                for _, headers in self.seen[1:]:
                    for name in ('proxy-authorization', 'x-scope'):
                        self.assertNotIn(name, headers)
                    self.assertNotEqual(headers.get('user-agent'), self.browser['User-Agent'])

    def test_external_ffmpeg_transport_still_blocked(self):
        with yt_dlp.YoutubeDL(self.options) as ydl:
            with self.assertRaisesRegex(DownloadError, 'cannot preserve request scopes'):
                FFmpegFD(ydl, {}).real_download('unused', {'url': self.b + '/clip.mp4'})
        self.assertEqual(self.seen, [])

    def test_curl_keeps_upstream_default_user_agent_policy(self):
        with yt_dlp.YoutubeDL(self.options) as ydl:
            self.fetch(ydl, self.b + '/fingerprint', {'User-Agent': std_headers['User-Agent']}, True)
        self.assertEqual(self.seen[-1][1]['user-agent'], self.curl_user_agent)

    def test_extractor_api_authorization_matches_direct_tool(self):
        for curl in (False, True):
            with self.subTest(curl=curl), yt_dlp.YoutubeDL(self.options) as ydl:
                request = self.fetch(ydl, self.b + '/api', self.api_headers, curl)
                self.assertTrue(all(request.headers.get(key) == value
                                    for key, value in self.api_headers.items()))

    def test_extractor_api_credentials_are_target_local(self):
        for curl in (False, True):
            for destination in (self.b + '/end', self.foreign + '/end', self.a + '/end'):
                with self.subTest(curl=curl, destination=destination):
                    self.seen.clear()
                    self.routes.update({'/api-start': self.b + '/api-step', '/api-step': destination})
                    with yt_dlp.YoutubeDL(self.options) as ydl:
                        self.fetch(ydl, self.b + '/api-start', self.api_headers, curl)
                    first, middle, last = [h for _, h in self.seen]
                    self.assertTrue(all(first.get(key.lower()) == value
                                        for key, value in self.api_headers.items()))
                    for key in self.api_headers:
                        self.assertNotIn(key.lower(), middle)
                        if destination == self.a + '/end' and key.lower() == 'authorization':
                            self.assertTrue(last.get('authorization') == 'origin-fixture')
                        else:
                            self.assertNotIn(key.lower(), last)

    def test_global_api_auth_and_raw_cookie_are_not_trusted(self):
        options = {**self.options, 'http_headers': self.api_headers}
        for curl in (False, True):
            with self.subTest(curl=curl), yt_dlp.YoutubeDL(options) as ydl:
                self.fetch(ydl, self.b + '/global', {'Cookie': 'fixture=raw'}, curl)
                for key in (*self.api_headers, 'Cookie'):
                    self.assertNotIn(key.lower(), self.seen[-1][1])


if __name__ == '__main__':
    unittest.main()
