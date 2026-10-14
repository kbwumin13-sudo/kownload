"""Local, deterministic HTTP Range fixture for GUI pause/resume acceptance."""
import argparse
import hashlib
import http.server
import time

parser = argparse.ArgumentParser()
parser.add_argument('--port', type=int, default=18793)
args = parser.parse_args()
BLOCK = bytes(range(256)) * 256
LENGTH = len(BLOCK) * 1024


class Handler(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.send_fixture(False)

    def do_GET(self):
        self.send_fixture(True)

    def send_fixture(self, body):
        if self.path.split('?')[0] != '/fixture.bin':
            self.send_error(404)
            return
        start, end = 0, LENGTH - 1
        partial = self.headers.get('Range', '').startswith('bytes=')
        if partial:
            lo, hi = self.headers['Range'][6:].split('-', 1)
            start, end = int(lo or 0), int(hi or end)
            if start >= LENGTH or end < start:
                self.send_error(416)
                return
            end = min(end, LENGTH - 1)
        self.send_response(206 if partial else 200)
        self.send_header('Content-Length', str(end - start + 1))
        self.send_header('Accept-Ranges', 'bytes')
        self.send_header('Content-Type', 'application/octet-stream')
        self.send_header('Content-Disposition', 'attachment; filename="fixture.bin"')
        if partial:
            self.send_header('Content-Range', f'bytes {start}-{end}/{LENGTH}')
        self.end_headers()
        if not body:
            return
        try:
            position = start
            while position <= end:
                count = min(16384, end - position + 1)
                index = position % len(BLOCK)
                self.wfile.write((BLOCK + BLOCK)[index:index + count])
                self.wfile.flush()
                position += count
                time.sleep(.08)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, fmt, *values):
        print(fmt % values, flush=True)


print(f'http://127.0.0.1:{args.port}/fixture.bin bytes={LENGTH} sha256={hashlib.sha256(BLOCK * 1024).hexdigest()}', flush=True)
http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Handler).serve_forever()
