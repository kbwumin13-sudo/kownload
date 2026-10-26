#!/usr/bin/env python3
"""Compare direct and proxied paths for public sources (W05 diagnosis).

Each pair downloads the same byte range over both paths in alternating order
with curl; bodies are discarded. Prints JSON lines with timings only, no
headers or cookies. Limited ranges show path differences, not whole-file speed.
"""
import argparse
import json
import subprocess
import time


def fetch(url, start, length, proxy):
    cmd = ['curl', '--silent', '--show-error', '--location', '--max-time', '60', '--output', '/dev/null',
           '--range', f'{start}-{start + length - 1}', '--write-out', '%{json}']
    cmd += ['--proxy', proxy] if proxy else ['--noproxy', '*']
    out = subprocess.run(cmd + [url], capture_output=True, text=True, timeout=90)
    try:
        raw = json.loads(out.stdout)
    except ValueError:
        return {'exit': out.returncode, 'ok': False}
    return {'exit': out.returncode, 'http': raw.get('http_code'), 'bytes': raw.get('size_download'),
            'MBps': round(raw.get('speed_download', 0) / 1e6, 3), 'connect': raw.get('time_connect'),
            'tls': raw.get('time_appconnect'), 'ttfb': raw.get('time_starttransfer'),
            'total': raw.get('time_total'), 'ok': out.returncode == 0 and raw.get('size_download') == length}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--url', action='append', required=True, help='NAME=URL')
    parser.add_argument('--proxy', default='http://127.0.0.1:7891')
    parser.add_argument('--pairs', type=int, default=3)
    parser.add_argument('--length', type=int, default=16 << 20)
    parser.add_argument('--offset', type=int, default=32 << 20)
    args = parser.parse_args()
    for item in args.url:
        name, _, url = item.partition('=')
        for pair in range(args.pairs):
            start = args.offset + pair * args.length
            order = [('direct', ''), ('proxy', args.proxy)]
            if pair % 2:
                order.reverse()
            for path, proxy in order:
                result = fetch(url, start, args.length, proxy)
                print(json.dumps({'source': name, 'pair': pair, 'path': path, **result}), flush=True)
                time.sleep(1)


if __name__ == '__main__':
    main()
