# Loaded only by the interpreter belonging to the configured yt-dlp entrypoint.
import http.cookiejar
import json
import os
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import urllib.parse
import urllib.request
import yt_dlp
from yt_dlp.networking import _urllib
from yt_dlp.downloader.external import ExternalFD
from yt_dlp.networking import Request
from yt_dlp.utils import DownloadError, parse_m3u8_attributes

with open(sys.argv[1], encoding='utf-8') as context_file:
    context = json.load(context_file)


def origin(raw):
    parsed = urllib.parse.urlsplit(raw)
    port = parsed.port or (443 if parsed.scheme == 'https' else 80)
    return parsed.scheme.lower(), (parsed.hostname or '').lower(), port


scopes = context['scopes']
navigation_names = {'referer', 'user-agent'}
api_auth_names = {'authorization', 'x-csrf-token', 'x-guest-token'}
initial_names = navigation_names | api_auth_names
scoped_names = {key.lower() for scope in scopes for key in scope['headers']}
scoped_names.update(initial_names | {'cookie', 'proxy-authorization'})


def extractor_headers(headers):
    # requestArgs never seeds browser context into yt-dlp options/Request.
    # Scopes enter only urllib unredirected_hdrs or curl's outgoing headers;
    # redirects drop initial headers before reapplying destination scopes.
    # Extractor navigation and API auth belong to the explicit initial target,
    # even when values equal browser context (e.g. Bili Referer == SourcePage).
    result = {}
    for key, value in headers.items():
        name = key.lower()
        if name not in initial_names:
            continue
        if not value or any(char in value for char in '\r\n'):
            continue
        if name == 'referer':
            try:
                parsed = urllib.parse.urlsplit(value)
                if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username is not None:
                    continue
            except ValueError:
                continue
        result[name] = value
    return result


def matching_headers(raw):
    matched = []
    for scope in scopes:
        if scope.get('url'):
            if raw == scope['url']:
                matched.append(((2, 0), scope['headers']))
        elif origin(raw) == origin(scope['origin']):
            prefix = scope.get('pathPrefix', '')
            if urllib.parse.urlsplit(raw).path.startswith(prefix):
                matched.append(((1 if prefix else 0, len(prefix)), scope['headers']))
    headers = {}
    for _, values in sorted(matched, key=lambda item: item[0]):
        headers.update({key.lower(): value for key, value in values.items()})
    return headers


original_prepare_headers = _urllib.UrllibRH._prepare_headers


def scoped_prepare_headers(self, request, headers):
    original_prepare_headers(self, request, headers)
    # The urllib request is built from merged defaults. Only explicit Request
    # API credentials may be retained, never a global default authorization.
    for key in api_auth_names:
        if key not in request.headers:
            headers.pop(key, None)


_urllib.UrllibRH._prepare_headers = scoped_prepare_headers
original_request = _urllib.HTTPHandler.http_request


def scoped_request(self, req):
    # Called for initial requests AND every redirect by the urllib opener.
    # Remove extractor/copy-propagated credentials before applying destination scope.
    initial = extractor_headers(req.headers)
    for key in list(req.headers) + list(req.unredirected_hdrs):
        if key.lower() in scoped_names:
            req.remove_header(key)
    # unredirected headers are not copied by urllib's redirect handler. Explicit
    # destination scopes override extractor headers, including on redirects.
    for key, value in (initial | matching_headers(req.full_url)).items():
        req.add_unredirected_header(key, value)
    return original_request(self, req)


_urllib.HTTPHandler.http_request = scoped_request
_urllib.HTTPHandler.https_request = scoped_request
original_director = yt_dlp.YoutubeDL.build_request_director


try:
    from yt_dlp.networking import _curlcffi
except ImportError:
    _curlcffi = None


if _curlcffi is not None:
    from yt_dlp.networking.exceptions import HTTPError, RequestError

    class ScopedCurlSession(_curlcffi.curl_cffi.requests.Session):
        def __init__(self):
            super().__init__()
            self._scope_lock = threading.Lock()

        def request(self, *args, **kwargs):
            # Only the handler may follow redirects. Cookie policy is enforced
            # by yt-dlp's CookieJar, never curl's independent cookie engine.
            kwargs['allow_redirects'] = False
            with self._scope_lock:
                self.cookies = {}
                return super().request(*args, **kwargs)

    class ScopedCurlRH(_curlcffi.CurlCFFIRH):
        def _create_instance(self, cookiejar=None):
            return ScopedCurlSession()

        def _get_impersonate_headers(self, request):
            headers = super()._get_impersonate_headers(request)
            # Respect upstream's removal of default fingerprint headers, and
            # accept initial headers only when present on this explicit request.
            initial = extractor_headers({key: value for key, value in headers.items()
                                         if key in request.headers})
            for key in list(headers):
                if key.lower() in scoped_names or key.lower() == 'cookie':
                    del headers[key]
            headers.update(initial)
            headers.update(matching_headers(request.url))
            cookie_request = urllib.request.Request(request.url)
            self._get_cookiejar(request).add_cookie_header(cookie_request)
            if cookie_request.has_header('Cookie'):
                headers['Cookie'] = cookie_request.get_header('Cookie')
            return headers

        def _send(self, request):
            request = request.copy()
            for hop in range(6):
                # Validate each destination, including schemes introduced by
                # Location. Upstream retains TLS, proxy and impersonation setup.
                self.validate(request)
                try:
                    response = super()._send(request)
                except HTTPError as error:
                    response = error.response
                    redirect_error = error
                else:
                    redirect_error = None
                cookie_request = urllib.request.Request(request.url)
                self._get_cookiejar(request).extract_cookies(
                    SimpleNamespace(info=lambda: response.headers), cookie_request)
                location = response.headers.get('Location')
                if response.status not in (301, 302, 303, 307, 308) or not location:
                    if redirect_error:
                        raise redirect_error
                    return response
                if hop == 5:
                    response.close()
                    raise HTTPError(response, redirect_loop=True)
                destination = urllib.parse.urljoin(request.url, location)
                parsed = urllib.parse.urlsplit(destination)
                if parsed.scheme not in ('http', 'https') or parsed.username is not None:
                    response.close()
                    raise RequestError('Unsupported media redirect destination')
                if (response.status == 303 and request.method != 'HEAD'
                        or response.status in (301, 302) and request.method == 'POST'):
                    request.method = 'GET'
                    request.data = None
                    for key in ('Content-Type', 'Content-Length', 'Transfer-Encoding'):
                        request.headers.pop(key, None)
                elif request.data is not None and not isinstance(request.data, bytes):
                    response.close()
                    raise RequestError('Cannot replay streaming media request body')
                response.close()
                # The extractor authorized these headers for the original target,
                # not for a Location chosen by that target (even same-origin).
                for key in initial_names:
                    request.headers.pop(key, None)
                request.url = destination


def scoped_director(self, handlers, preferences=None):
    self.cookiejar.set_policy(http.cookiejar.DefaultCookiePolicy(
        strict_ns_domain=http.cookiejar.DefaultCookiePolicy.DomainStrictNonDomain))
    scoped_handlers = [_urllib.UrllibRH]
    if _curlcffi is not None:
        scoped_handlers.append(ScopedCurlRH)
    # Preserve urllib for ordinary requests; impersonation requires curl.
    return original_director(self, scoped_handlers, set(preferences or ()) | {
        lambda handler, request: 100 if isinstance(handler, _urllib.UrllibRH) else 0,
    })


yt_dlp.YoutubeDL.build_request_director = scoped_director


def no_external_network(self, *args, **kwargs):
    raise DownloadError('External media transport cannot preserve request scopes')


# FFmpeg remains available for local merge/subtitle postprocessing only.
ExternalFD.real_download = no_external_network


def probe_read(ydl, raw, limit, byte_range=None):
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username is not None:
        raise ValueError('Unsupported sample URL')
    headers = {}
    if byte_range:
        length, offset = byte_range
        if not 0 < length <= limit or offset < 0:
            raise ValueError('Sample range too large')
        headers['Range'] = f'bytes={offset}-{offset + length - 1}'
    # Reuse the scoped director, proxy, TLS policy and CookieJar for every hop.
    # No extractor/copied authorization enters this request.
    with ydl.urlopen(Request(raw, headers=headers, extensions={'timeout': 5})) as response:
        data = response.read(limit + 1)
        destination = response.url
        if len(data) > limit or byte_range and (response.status != 206 or len(data) != byte_range[0]):
            raise ValueError('Sample exceeds budget or range ignored')
        return data, destination


def hls_sample(ydl, raw):
    manifest, base = probe_read(ydl, raw, 512 * 1024)
    lines = manifest.decode('utf-8-sig').splitlines()
    if not lines or lines[0].strip() != '#EXTM3U' or '#EXT-X-ENDLIST' not in lines:
        return None
    # Encrypted, master, and low-latency manifests stay unknown. Never fetch keys.
    for line in lines:
        if line.startswith(('#EXT-X-STREAM-INF:', '#EXT-X-PART:', '#EXT-X-SESSION-KEY:')):
            return None
        if line.startswith('#EXT-X-KEY:') and parse_m3u8_attributes(line.split(':', 1)[1]).get('METHOD') != 'NONE':
            return None
    init, segment, segment_range = None, None, None
    for line in lines:
        line = line.strip()
        if line.startswith('#EXT-X-MAP:'):
            attrs = parse_m3u8_attributes(line.split(':', 1)[1])
            # Optional init ranges must be explicit; implicit ranges need state.
            init_range = attrs.get('BYTERANGE')
            if init_range:
                length, offset = init_range.split('@')
                init_range = (int(length), int(offset))
            init = (urllib.parse.urljoin(base, attrs['URI']), init_range)
        elif line.startswith('#EXT-X-BYTERANGE:'):
            value = line.split(':', 1)[1]
            length, _, offset = value.partition('@')
            segment_range = (int(length), int(offset or '0'))
        elif line and not line.startswith('#'):
            segment = urllib.parse.urljoin(base, line)
            break
    if not segment:
        return None
    data = b''
    if init:
        data, _ = probe_read(ydl, init[0], 2 * 1024 * 1024, init[1])
    chunk, _ = probe_read(ydl, segment, 2 * 1024 * 1024, segment_range)
    return data + chunk


def sample_codecs(ydl, fmt):
    probe = context.get('probe')
    if not probe or not os.path.isfile(probe):
        return None
    data = hls_sample(ydl, fmt['url'])
    if not data:
        return None
    with tempfile.TemporaryDirectory(prefix='gopeed-media-probe-') as directory:
        filename = os.path.join(directory, 'sample')
        with open(filename, 'wb') as sample:
            sample.write(data)
        # Both whitelists are required: playlist demuxers could open dependent
        # resources even when the top-level argument is a local file.
        result = subprocess.run([
            probe, '-v', 'error', '-protocol_whitelist', 'file,pipe',
            '-format_whitelist', 'mov,mpegts,aac', '-show_entries',
            'stream=codec_type,codec_name,width,height:stream_disposition=attached_pic',
            '-of', 'json', filename,
        ], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5, check=True)
    streams = json.loads(result.stdout).get('streams', [])
    # An unrecognized stream might be the missing track. Preserve uncertainty.
    if not streams or any(s.get('codec_type') not in ('audio', 'video', 'subtitle', 'data')
                          or s.get('codec_name') in (None, '', 'unknown') for s in streams):
        return None
    video = next((s for s in streams if s['codec_type'] == 'video'
                  and not s.get('disposition', {}).get('attached_pic')), None)
    audio = next((s for s in streams if s['codec_type'] == 'audio'), None)
    if not video and not audio:
        return None
    return {'vcodec': video['codec_name'] if video else 'none',
            'acodec': audio['codec_name'] if audio else 'none',
            **({'width': video['width'], 'height': video['height']} if video else {})}


original_video_result = yt_dlp.YoutubeDL.process_video_result


def scoped_video_result(self, info, download=True):
    # Enrich inspection only. Download completion independently probes its
    # finished file, including persisted tasks and changing remote resources.
    if self.params.get('dump_single_json') and not info.get('is_live') and not info.get('has_drm'):
        budget = 4
        for fmt in info.get('formats') or [info]:
            if budget == 0:
                break
            if (fmt.get('protocol') not in ('m3u8', 'm3u8_native') or fmt.get('has_drm')
                    or all(fmt.get(key) not in (None, '', 'unknown') for key in ('vcodec', 'acodec'))):
                continue
            budget -= 1
            try:
                codecs = sample_codecs(self, fmt)
            except Exception:
                # Best-effort metadata must never invent a track on failure.
                continue
            if codecs:
                fmt.update(codecs)
    return original_video_result(self, info, download)


yt_dlp.YoutubeDL.process_video_result = scoped_video_result
yt_dlp.main(context['args'] + ['--downloader', 'native'])
