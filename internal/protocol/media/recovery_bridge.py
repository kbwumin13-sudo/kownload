# Embedded by the Go media launcher only for a parent-managed process. No
# listener, shared credential file, private budget or fallback transport.
def install_recovery_bridge():
    import itertools
    import http.client
    import io
    from yt_dlp.networking.exceptions import HTTPError, TransportError

    descriptor = context.get('recovery')
    if descriptor is None:
        return
    reader = os.fdopen(descriptor['read'], 'rb', buffering=0)
    writer = os.fdopen(descriptor['write'], 'wb', buffering=0)
    lock = threading.Lock()
    write_lock = threading.Lock()
    pending = {}
    sequence = itertools.count(1)
    stopped = False

    def receive():
        nonlocal stopped
        try:
            source = io.BufferedReader(reader)
            while True:
                line = source.readline(4097)
                if not line or len(line) > 4096:
                    break
                reply = json.loads(line)
                with lock:
                    item = pending.get(reply['id'])
                    if item is None or type(reply.get('ok')) is not bool:
                        break
                    item[1] = reply['ok']
                    item[0].set()
        finally:
            with lock:
                stopped = True
                for item in pending.values():
                    item[0].set()

    threading.Thread(target=receive, name='media-parent-replies', daemon=True).start()

    def rpc(operation, lease=0):
        with lock:
            if stopped:
                raise TransportError('media recovery permission unavailable')
            identifier = next(sequence)
            item = [threading.Event(), False]
            pending[identifier] = item
        try:
            payload = json.dumps({'id': identifier, 'op': operation, 'lease': lease}, separators=(',', ':')).encode() + b'\n'
            with write_lock:
                writer.write(payload)
            # Parent admission is cancellable by the parent context. The Go
            # launcher kills and joins this process group on cancellation.
            item[0].wait()
            if not item[1]:
                raise TransportError('media recovery permission unavailable')
            return identifier
        except (OSError, ValueError):
            raise TransportError('media recovery permission unavailable') from None
        finally:
            with lock:
                pending.pop(identifier, None)

    def guard_response(response, lease):
        state_lock = threading.RLock()
        ended = False
        original_close = response.close

        def finish():
            nonlocal ended
            with state_lock:
                if ended:
                    return
                ended = True
            rpc('leave', lease)

        def close():
            try:
                return original_close()
            finally:
                finish()

        def wrap_read(original, into=False, whole=False):
            def read(*args, **kwargs):
                rpc('valid')
                with state_lock:
                    if ended:
                        return 0 if into else b''
                rpc('check', lease)
                try:
                    result = original(*args, **kwargs)
                    rpc('valid')
                except BaseException:
                    try:
                        close()
                    except Exception:
                        pass
                    raise
                # read(0) is not EOF. A successful unbounded read consumes the
                # body; sized reads finish on actual EOF/closed HTTPResponse.
                requested = args[0] if args else kwargs.get('amt', kwargs.get('size'))
                eof = not result and requested != 0
                if eof or (whole and requested is None) or getattr(response, 'closed', False):
                    finish()
                return result
            return read

        for name in ('read', 'read1', 'readline', 'readinto'):
            original = getattr(response, name, None)
            if original is not None:
                setattr(response, name, wrap_read(original, name == 'readinto', name == 'read'))
        response.close = close
        return response

    original_open = _urllib.HTTPHandler.do_open

    def admitted_open(self, http_class, request, **kwargs):
        lease = rpc('enter')
        try:
            response = original_open(self, http_class, request, **kwargs)
            rpc('check', lease)
            return guard_response(response, lease)
        except BaseException:
            rpc('leave', lease)
            raise

    _urllib.HTTPHandler.do_open = admitted_open
    if _curlcffi is not None:
        original_send = _curlcffi.CurlCFFIRH._send

        def admitted_send(self, request):
            lease = rpc('enter')
            try:
                try:
                    response = original_send(self, request)
                except HTTPError as error:
                    error.response = guard_response(error.response, lease)
                    raise
                rpc('check', lease)
                return guard_response(response, lease)
            except HTTPError:
                # The existing redirect/error handler owns and closes this
                # response. Preserve its lease until that body is released.
                raise
            except BaseException:
                rpc('leave', lease)
                raise

        _curlcffi.CurlCFFIRH._send = admitted_send
