#!/usr/bin/env python3
"""Generate local media samples and serve browser/download integration fixtures."""

import argparse
import http.server
import mimetypes
import os
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import unquote, urlsplit


def ffmpeg(*args):
    subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", *map(str, args)], check=True)


def generate(root):
    root.mkdir(parents=True, exist_ok=True)
    (root / "hls").mkdir(exist_ok=True)
    (root / "dash").mkdir(exist_ok=True)
    (root / "images").mkdir(exist_ok=True)
    (root / "fmp4").mkdir(exist_ok=True)
    key = root / "hls" / "key.bin"
    if not key.exists():
        key.write_bytes(os.urandom(16))
    (root / "hls" / "key_info.txt").write_text(
        "key.bin\n" + str(key.resolve()) + "\n" + os.urandom(16).hex() + "\n"
    )
    video = ["-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "8", "-shortest"]
    encode = ["-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac"]
    ffmpeg(*video, *encode, "-movflags", "+faststart", root / "sample.mp4")
    ffmpeg(*video, *encode, "-g", "24", "-hls_time", "1", "-hls_list_size", "0", "-hls_key_info_file", root / "hls" / "key_info.txt", root / "hls" / "high.m3u8")
    ffmpeg(*video, "-vf", "scale=160:90", *encode, "-g", "24", "-hls_time", "1", "-hls_list_size", "0", root / "hls" / "low.m3u8")
    ffmpeg("-f", "lavfi", "-i", "sine=frequency=660:sample_rate=44100", "-t", "8", "-vn", "-c:a", "aac", "-hls_time", "1", "-hls_list_size", "0", root / "hls" / "audio.m3u8")
    (root / "hls" / "master.m3u8").write_text(
        '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="audio.m3u8"\n'
        '#EXT-X-STREAM-INF:BANDWIDTH=350000,RESOLUTION=160x90,AUDIO="audio"\nlow.m3u8\n'
        '#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=320x180,AUDIO="audio"\nhigh.m3u8\n'
    )
    ffmpeg(*video, *encode, "-seg_duration", "1", "-use_template", "1", "-use_timeline", "1", "-adaptation_sets", "id=0,streams=v id=1,streams=a", "-f", "dash", root / "dash" / "stream.mpd")
    ffmpeg(*video, *encode, "-profile:v", "baseline", "-level:v", "3.0", "-g", "24", "-hls_time", "1", "-hls_list_size", "0", "-hls_segment_type", "fmp4", root / "fmp4" / "index.m3u8")
    for i, color in enumerate(("red", "green", "blue"), 1):
        ffmpeg("-f", "lavfi", "-i", f"color=c={color}:s=320x180", "-frames:v", "1", root / "images" / f"{i}.jpg")
    (root / "index.html").write_text("""<!doctype html><title>Media fixture</title>
<style>.background{width:160px;height:90px;background-image:url('/images/3.jpg')}</style>
<video controls src="/sample.mp4"></video><video controls src="/hls/master.m3u8"></video>
<a href="/dash/stream.mpd">DASH</a><a href="/hls/master.m3u8">HLS</a>
<img src="/images/1.jpg"><img srcset="/images/1.jpg 1x, /images/2.jpg 2x" src="/images/1.jpg">
<img loading="lazy" data-src="/images/2.jpg" src="/images/1.jpg"><div class="background"></div>
<iframe src="/frame.html"></iframe><a href="/spa.html">SPA navigation</a>
<a href="/protected/sample.mp4">Cookie protected MP4</a>
<script>document.querySelector('[data-src]').src=document.querySelector('[data-src]').dataset.src</script>
""")
    (root / "frame.html").write_text('<video controls src="/sample.mp4"></video><img src="/images/2.jpg">')
    (root / "spa.html").write_text('''<a href="/spa.html#media" id="go">Open media</a><main id="view"></main><script>
function render(){document.querySelector('#view').innerHTML=location.hash ? '<video controls src="/sample.mp4"></video><img src="/images/3.jpg">' : 'Navigate to media'}
addEventListener('hashchange',render);render();</script>''')

    (root / "target.html").write_text('<!doctype html><meta charset="utf-8"><title>单个视频下载验收</title><style>body{font:16px system-ui;padding:20px}section{display:inline-block;margin:12px}video{width:400px;height:225px}iframe{width:450px;height:270px}</style><h1>点击哪一个，只下载哪一个</h1><section><h2>视频 A · MP4</h2><video controls src="/sample.mp4?target=A"></video></section><section><h2>视频 B · HLS</h2><video controls src="/hls/master.m3u8?target=B"></video></section><p>无关图片，不应出现在播放器下载页</p><img src="/images/1.jpg"><img src="/images/2.jpg"><iframe src="/frame.html"></iframe>', encoding="utf-8")

    (root / 'optimization.html').write_text('<!doctype html><meta charset="utf-8"><title>嗅探优化验收</title><style>body{font:16px system-ui;padding:20px}section{display:inline-block;margin:12px}video{width:360px;height:203px}iframe{width:420px;height:250px}</style><h1>单视频、组件播放器与按需识别</h1><section><h2>A · 直链</h2><video controls src="/sample.mp4?target=A"></video></section><section id="shadow"></section><section><h2>C · 未识别播放器</h2><video id="opaque" controls></video><button id="load">加载播放器配置</button></section><img src="/images/1.jpg"><iframe src="/frame.html"></iframe><script>const shadow=document.querySelector(\'#shadow\').attachShadow({mode:\'open\'});shadow.innerHTML=\'<h2>B · Shadow DOM HLS</h2><video style="width:360px;height:203px" controls src="/hls/master.m3u8?target=B"></video>\';document.querySelector(\'#opaque\').src=URL.createObjectURL(new Blob([\'opaque\'],{type:\'video/mp4\'}));document.querySelector(\'#load\').onclick=()=>fetch(\'/playback-config.json\').then(r=>r.json()).then(()=>document.querySelector(\'#load\').textContent=\'配置已加载 · \'+fetch.name);</script>', encoding='utf-8')
    (root / 'playback-config.json').write_text('{"playback":{"manifest":"/dash/stream.mpd?target=C"}}')
    (root / 'images' / 'image(1).jpg').write_bytes((root / 'images' / '1.jpg').read_bytes())
    (root / 'captions.vtt').write_text('WEBVTT\n\n00:00.000 --> 00:07.500\nGopeed subtitle acceptance\n')
    (root / 'watch-subtitles.html').write_text('<!doctype html><meta charset="utf-8"><title>Gopeed 字幕验收</title><video controls src="/sample.mp4"><track kind="subtitles" src="/captions.vtt" srclang="en" label="English"></video>',encoding='utf-8')
    (root / 'system.html').write_text('''<!doctype html><meta charset="utf-8"><title>Gopeed 全系统验收</title>
<style>body{font:16px system-ui;padding:20px}video{width:400px;height:225px}.image{display:inline-block;width:160px;height:90px}.quoted{background-image:url("/images/image(1).jpg")}.set{background-image:image-set("/images/1.jpg" 1x,"/images/2.jpg" 2x)}.pseudo:before{content:"";display:block;width:160px;height:90px;background-image:url('/images/3.jpg')}</style>
<h1>媒体与图片验收</h1><video id="mse" controls muted></video><button id="play">加载 MSE 播放器</button><div id="result" role="status">等待增强识别</div>
<div class="image quoted"></div><div class="image set"></div><div class="image pseudo"></div><img width="160" src="/images/1.jpg" srcset="/images/1.jpg 1x,/images/2.jpg 2x"><a href="/watch-subtitles.html">字幕样本</a><a href="/profile.html">性能样本</a>
<script>
const video=document.querySelector('#mse'),result=document.querySelector('#result');video.src=URL.createObjectURL(new Blob(['placeholder'],{type:'video/mp4'}));
document.querySelector('#play').onclick=async()=>{try{const source=new MediaSource();video.src=URL.createObjectURL(source);await new Promise(resolve=>source.addEventListener('sourceopen',resolve,{once:true}));const buffer=source.addSourceBuffer('video/mp4; codecs="avc1.42c01e, mp4a.40.2"');const manifestURL=new URL('/fmp4/index.m3u8',location.href);const text=await(await fetch(manifestURL)).text();const init=text.match(/#EXT-X-MAP:.*URI="([^"]+)"/)[1];const parts=[init,...text.split(/\\r?\\n/).filter(line=>line&&!line.startsWith('#'))];for(const part of parts){const bytes=await(await fetch(new URL(part,manifestURL))).arrayBuffer();await new Promise((resolve,reject)=>{buffer.addEventListener('updateend',resolve,{once:true});buffer.addEventListener('error',reject,{once:true});buffer.appendBuffer(bytes);});}source.endOfStream();await video.play();result.textContent='MSE 已播放 · '+video.duration.toFixed(2)+' 秒';}catch(e){result.textContent='MSE 失败 · '+e.name;}};
</script>''',encoding='utf-8')
    (root / 'profile.html').write_text('''<!doctype html><meta charset="utf-8"><title>Gopeed 性能验收</title><style>body{font:16px system-ui}#nodes>div{height:1px}</style><h1>10,000 节点增量性能样本</h1><button id="run">运行性能样本</button><pre id="result" role="status">等待运行</pre><main id="nodes"></main><script>
const tasks=[];try{new PerformanceObserver(list=>tasks.push(...list.getEntries().map(e=>({start:e.startTime,duration:e.duration})))).observe({type:'longtask',buffered:true});}catch{}
document.querySelector('#run').onclick=async()=>{const started=performance.now(),root=document.querySelector('#nodes');root.replaceChildren();const fragment=document.createDocumentFragment();for(let i=0;i<10000;i++){const node=document.createElement('div');node.textContent='节点 '+i;fragment.append(node);}root.append(fragment);const delays=[];for(let i=0;i<40;i++){const at=performance.now();await new Promise(requestAnimationFrame);delays.push(performance.now()-at);}for(let i=0;i<10;i++){const img=document.createElement('img');img.src='/images/'+(i%3+1)+'.jpg';root.append(img);}await new Promise(r=>setTimeout(r,700));const report={elapsedMs:Math.round(performance.now()-started),longTasks:tasks.filter(t=>t.start>=started).length,maxLongTaskMs:Math.round(Math.max(0,...tasks.filter(t=>t.start>=started).map(t=>t.duration))),p95FrameMs:Math.round(delays.sort((a,b)=>a-b)[Math.floor(delays.length*.95)])};document.querySelector('#result').textContent=JSON.stringify(report);};
</script>''',encoding='utf-8')


class Handler(http.server.SimpleHTTPRequestHandler):
    root = None
    delay = 0

    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(self.root), **kwargs)

    def do_GET(self):
        self.serve_file(False)

    def do_HEAD(self):
        self.serve_file(True)

    def serve_file(self, head):
        path = unquote(urlsplit(self.path).path)
        protected = path.startswith('/protected/')
        if protected:
            if 'fixture_session=synthetic-ok' not in self.headers.get('Cookie', '') or self.headers.get('Referer', '') != f'http://127.0.0.1:{self.server.server_port}/index.html':
                self.send_error(403)
                return
            path = path[len('/protected'):]
        target = (self.root / path.lstrip('/')).resolve()
        if not target.is_relative_to(self.root.resolve()) or not target.is_file() or target.name == 'key_info.txt':
            self.send_error(404)
            return
        size = target.stat().st_size
        start, end = 0, size - 1
        range_header = self.headers.get('Range')
        if range_header:
            match = re.fullmatch(r'bytes=(\d*)-(\d*)', range_header)
            if not match or (not match[1] and not match[2]):
                self.send_error(416)
                return
            if not match[1]:
                start = max(0, size - int(match[2]))
            else:
                start = int(match[1])
                if match[2]:
                    end = min(end, int(match[2]))
            if start >= size or end < start:
                self.send_error(416)
                return
        self.send_response(206 if range_header else 200)
        media_type = mimetypes.guess_type(target.name)[0] or 'application/octet-stream'
        if target.suffix == '.m3u8':
            media_type = 'application/vnd.apple.mpegurl'
        elif target.suffix == '.mpd':
            media_type = 'application/dash+xml'
        self.send_header('Content-Type', media_type)
        self.send_header('Accept-Ranges', 'bytes')
        self.send_header('Content-Length', str(end - start + 1))
        if range_header:
            self.send_header('Content-Range', f'bytes {start}-{end}/{size}')
        self.end_headers()
        if head:
            return
        try:
            with target.open('rb') as stream:
                stream.seek(start)
                remaining = end - start + 1
                while remaining:
                    data = stream.read(min(16384, remaining))
                    self.wfile.write(data)
                    remaining -= len(data)
                    if self.delay and target.suffix in ('.ts', '.m4s', '.mp4'):
                        time.sleep(self.delay)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, fmt, *args):
        # Request paths and headers may contain browser credentials.
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', type=int, default=18794)
    parser.add_argument('--root', type=Path, default=Path('../artifacts/media-fixtures'))
    parser.add_argument('--delay', type=float, default=0, help='seconds between media chunks')
    args = parser.parse_args()
    if args.delay < 0:
        parser.error('--delay must be nonnegative')
    Handler.root = args.root.resolve()
    Handler.delay = args.delay
    generate(Handler.root)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    print(f'Fixture: http://127.0.0.1:{server.server_port}/index.html', flush=True)
    server.serve_forever()


if __name__ == '__main__':
    main()
