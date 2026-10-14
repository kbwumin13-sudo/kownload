"""Loopback-only integration coverage for the optional scoped curl transport."""
import http.server
import json
import pathlib
import subprocess
import sys
import tempfile
import threading
import unittest

import yt_dlp
from yt_dlp.networking import Request
from yt_dlp.networking.impersonate import ImpersonateTarget
from yt_dlp.networking.exceptions import CertificateVerifyError, HTTPError


class TransportTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.seen = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                cls.seen.append((self.server.server_port, self.path, self.command,
                                 dict(self.headers), self.rfile.read(int(self.headers.get('Content-Length', 0)))))
                route = cls.routes.get(self.path)
                if route:
                    code, location = route
                    self.send_response(code)
                    self.send_header('Location', location)
                    self.send_header('Set-Cookie', 'session=ok; Path=/private/')
                else:
                    self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()
            do_POST = do_GET
            do_PUT = do_GET
            do_HEAD = do_GET
        cls.servers = [http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) for _ in range(2)]
        cls.a, cls.b = [f'http://127.0.0.1:{s.server_port}' for s in cls.servers]
        cls.foreign = cls.b.replace('127.0.0.1', 'localhost')
        cls.routes = {
            '/start': (302, cls.a + '/private/step'),
            '/private/step': (302, cls.foreign + '/landing'),
            '/explicit': (302, cls.b + '/allowed'),
            '/port': (302, cls.b + '/outside'),
            '/exact': (302, cls.a + '/outside'),
            '/loop': (302, '/loop'),
        }
        for server in cls.servers:
            threading.Thread(target=server.serve_forever, daemon=True).start()
        cls.tmp = tempfile.TemporaryDirectory()
        payload = pathlib.Path(cls.tmp.name) / 'context.json'
        payload.write_text(json.dumps({'args': [], 'scopes': [
            {'origin': cls.a, 'headers': {'Authorization': 'origin-auth', 'X-Token': 'origin-token', 'Referer': cls.a + '/page'}},
            {'origin': cls.a, 'pathPrefix': '/private/', 'headers': {'Authorization': 'path-auth'}},
            {'url': cls.a + '/exact', 'headers': {'Authorization': 'exact-auth', 'X-Exact': 'exact-token'}},
            {'origin': cls.b, 'pathPrefix': '/allowed', 'headers': {'Authorization': 'explicit-auth', 'X-Token': 'explicit-token', 'Referer': cls.b + '/page'}},
        ]}))
        wrapper = pathlib.Path(__file__).with_name('context.py').read_text()
        wrapper = wrapper.replace("yt_dlp.main(context['args'] + ['--downloader', 'native'])", '')
        old_argv = sys.argv
        sys.argv = [sys.argv[0], str(payload)]
        cls.ns = {}
        try:
            exec(compile(wrapper, 'context.py', 'exec'), cls.ns)
        finally:
            sys.argv = old_argv
        cls.wrapper, cls.payload = wrapper, payload

    @classmethod
    def tearDownClass(cls):
        for server in cls.servers:
            server.shutdown()
            server.server_close()
        cls.tmp.cleanup()

    def setUp(self):
        self.seen.clear()
        self.ydl = yt_dlp.YoutubeDL({'quiet': True, 'proxy': ''})
        self.addCleanup(self.ydl.close)

    def fetch(self, url, method='GET', data=None):
        request = Request(url, method=method, data=data,
                          headers={'aUtHoRiZaTiOn': 'stale', 'X-Token': 'stale', 'Referer': 'stale', 'Cookie': 'injected=bad'},
                          extensions={'impersonate': ImpersonateTarget()})
        with self.ydl.urlopen(request) as response:
            self.assertIsNotNone(response.extensions.get('impersonate'))
            response.read()

    def headers(self, index):
        return {k.lower(): v for k, v in self.seen[index][3].items()}

    def test_redirect_scopes_and_cookies(self):
        self.fetch(self.a + '/start')
        self.assertEqual(len(self.seen), 3)
        self.assertEqual(self.headers(0)['authorization'], 'origin-auth')
        self.assertEqual(self.headers(1)['authorization'], 'path-auth')
        self.assertEqual(self.headers(1)['x-token'], 'origin-token')
        self.assertEqual(self.headers(1)['referer'], self.a + '/page')
        self.assertEqual(self.headers(1)['cookie'], 'session=ok')
        for name in ('authorization', 'x-token', 'referer', 'cookie'):
            self.assertNotIn(name, self.headers(2))
        self.fetch(self.a + '/outside')
        self.assertNotIn('cookie', self.headers(3))

    def test_explicit_foreign_origin(self):
        self.fetch(self.a + '/explicit')
        self.assertEqual(self.headers(1)['authorization'], 'explicit-auth')
        self.assertEqual(self.headers(1)['x-token'], 'explicit-token')
        self.assertEqual(self.headers(1)['referer'], self.b + '/page')

    def test_foreign_port_is_a_different_origin(self):
        self.fetch(self.a + '/port')
        for name in ('authorization', 'x-token', 'referer'):
            self.assertNotIn(name, self.headers(1))

    def test_exact_scope_removed(self):
        self.fetch(self.a + '/exact')
        self.assertEqual(self.headers(0)['authorization'], 'exact-auth')
        self.assertEqual(self.headers(0)['x-exact'], 'exact-token')
        self.assertEqual(self.headers(1)['authorization'], 'origin-auth')
        self.assertNotIn('x-exact', self.headers(1))

    def test_global_impersonation_preference(self):
        with yt_dlp.YoutubeDL({'quiet': True, 'proxy': '', 'impersonate': ImpersonateTarget()}) as ydl:
            with ydl.urlopen(self.a + '/outside') as response:
                self.assertIsNotNone(response.extensions.get('impersonate'))
                response.read()

    def test_methods_and_limit(self):
        for code, method, want in [(301, 'POST', 'GET'), (302, 'POST', 'GET'), (303, 'PUT', 'GET'), (303, 'HEAD', 'HEAD'), (307, 'POST', 'POST'), (308, 'POST', 'POST')]:
            with self.subTest(code=code, method=method):
                self.routes['/method'] = (code, '/finish')
                self.fetch(self.a + '/method', method, b'body' if method != 'HEAD' else None)
                self.assertEqual(self.seen[-1][2], want)
                self.assertEqual(self.seen[-1][4], b'body' if want == 'POST' else b'')
        self.seen.clear()
        with self.assertRaises(HTTPError):
            self.fetch(self.a + '/loop')
        self.assertEqual(len(self.seen), 6)

    def test_tls_verification(self):
        import ssl
        cert, key = [str(pathlib.Path(self.tmp.name) / name) for name in ('cert.pem', 'key.pem')]
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-subj', '/CN=localhost', '-days', '1'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), http.server.BaseHTTPRequestHandler)
        server.handle_error = lambda *args: None
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.routes['/tls'] = (302, f'https://127.0.0.1:{server.server_port}/')
        with self.assertRaises(CertificateVerifyError):
            self.fetch(self.a + '/tls')

    def test_missing_curl_and_old_module(self):
        # A fresh interpreter avoids previously imported handlers or monkeypatches.
        probe = '''
import builtins, pathlib, sys
original = builtins.__import__
def no_curl(name, globals=None, locals=None, fromlist=(), level=0):
    if 'curl_cffi' in name or '_curlcffi' in name or '_curlcffi' in (fromlist or ()):
        raise ImportError('optional dependency unavailable')
    return original(name, globals, locals, fromlist, level)
builtins.__import__ = no_curl
source = pathlib.Path(sys.argv[2]).read_text().replace("yt_dlp.main(context['args'] + ['--downloader', 'native'])", '')
exec(compile(source, 'context.py', 'exec'))
assert _curlcffi is None
with yt_dlp.YoutubeDL({'quiet': True, 'proxy': ''}) as ydl:
    assert list(ydl._request_director.handlers) == ['Urllib']
    with ydl.urlopen(context['scopes'][0]['origin'] + '/outside') as response:
        response.read()
'''
        for python in dict.fromkeys([sys.executable, str(pathlib.Path.home() / '.local/pipx/venvs/yt-dlp/bin/python')]):
            if pathlib.Path(python).exists():
                result = subprocess.run([python, '-c', probe, str(self.payload), str(pathlib.Path(__file__).with_name('context.py'))], capture_output=True, text=True, timeout=15)
                self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
