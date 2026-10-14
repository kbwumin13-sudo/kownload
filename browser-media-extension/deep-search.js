(() => {
  const KEY = Symbol.for('gopeed.media.deepSearch');
  window[KEY]?.stop?.();
  const SINGLE = 1024 * 1024, TOTAL = 4 * SINGLE, LIMIT = 64;
  const searchId = window.__gopeedMediaSearchId;
  const state = {active:true,used:0,urls:new Set(),stop:null,
    searchId:typeof searchId === 'string' && searchId.length > 0 && searchId.length <= 128 ? searchId : ''};
  const readers = new Set();
  // Metadata only: never retain or inspect media bytes. Missing/ambiguous evidence
  // (including transmuxing, workers and pre-hook sources) stays manual.
  const MAX_TRACK = 512;
  let tracked = 0, patched = 0;
  const ranges = new WeakMap(), parents = new WeakMap(), objectUrls = new WeakMap();
  const restorations = new WeakMap(), targets = new Set();
  const xhrListeners = new WeakMap(), xhrTargets = new Set();
  const manifests = new Map(), appended = new Map(), emittedProofs = new Set();
  function patch(target,key,wrap) {
    if (!target || typeof WeakRef !== 'function' || patched >= MAX_TRACK) return;
    try {
      if (restorations.get(target)?.some(entry => entry.key === key)) return;
      const original = target[key];
      if (typeof original !== 'function') return;
      const descriptor = Object.getOwnPropertyDescriptor(target,key);
      const wrapper = wrap(original);
      Object.defineProperty(target,key,{configurable:true,writable:true,value:wrapper,enumerable:descriptor?.enumerable ?? false});
      let entries = restorations.get(target);
      if (!entries) { entries = []; restorations.set(target,entries); targets.add(new WeakRef(target)); }
      entries.push({key,descriptor,wrapper}); patched++;
    } catch { /* Frozen/nonconfigurable page objects remain untouched. */ }
  }
  function observe(promise,callback) {
    // Observe a native Promise without replacing the Promise returned to the page.
    try { Promise.prototype.then.call(promise,value => { if (state.active) { try { callback(value); } catch {} } },() => {}); } catch {}
  }
  function range(value) {
    if (ArrayBuffer.isView(value)) {
      if (Object.prototype.toString.call(value.buffer) !== '[object ArrayBuffer]') return null;
      return {buffer:value.buffer,start:value.byteOffset,end:value.byteOffset+value.byteLength};
    }
    if (Object.prototype.toString.call(value) === '[object ArrayBuffer]') return {buffer:value,start:0,end:value.byteLength};
    return null;
  }
  function remember(value,url) {
    if (!state.active || !safeUrl(url) || sensitivePath(url)) return;
    const part = range(value);
    if (!part || part.end <= part.start) return;
    let entries = ranges.get(part.buffer);
    // Once the budget is exhausted, invalidate reused buffers rather than retain
    // stale provenance when a BYOB reader reuses storage for another response.
    if (entries === null) return;
    if (tracked >= MAX_TRACK) { if (entries) ranges.delete(part.buffer); return; }
    if (entries?.length >= 8) { ranges.set(part.buffer,null); return; }
    if (!entries) { entries = []; ranges.set(part.buffer,entries); }
    entries.push({start:part.start,end:part.end,url}); tracked++;
  }
  function provenance(value) {
    const part = range(value);
    if (!part || part.end <= part.start) return '';
    const entries = ranges.get(part.buffer) || [];
    const covering = entries.filter(item => item.start <= part.start && item.end >= part.end);
    const urls = new Set(entries.filter(item => item.start < part.end && item.end > part.start).map(item => item.url));
    return covering.length && urls.size === 1 ? covering[0].url : '';
  }
  function prove(url,blob) {
    let current = url;
    const visited = new Set(), chain = [];
    while (!visited.has(current) && visited.size < LIMIT) {
      visited.add(current);
      if (manifests.has(current)) chain.push(current);
      const owners = [...manifests].filter(([,item]) => item.refs.has(current)).map(([owner]) => owner);
      if (owners.length > 1) return; // Shared init/segment URLs cannot identify a playlist.
      if (!owners.length) break;
      current = owners[0];
    }
    for (const candidate of chain) {
      const key = candidate+'\n'+blob, item = manifests.get(candidate);
      if (emittedProofs.has(key) || emittedProofs.size >= MAX_TRACK) continue;
      emittedProofs.add(key);
      publish(candidate,item.mime,item.relatedUrls,item.contextUrls,{manifest:true,protocolHint:'manifest',mediaObjectUrl:blob});
    }
  }
  function manifest(url,mime,details) {
    if (!manifests.has(url) && manifests.size >= LIMIT) return;
    manifests.set(url,{...details,mime});
    publish(url,mime,details.relatedUrls,details.contextUrls,{manifest:true,protocolHint:'manifest'});
    for (const [source,blobs] of appended) for (const blob of blobs) prove(source,blob);
  }
  function inspectMedia(response) {
    const url = safeUrl(response?.url);
    if (!state.active || !response?.ok || !url || sensitivePath(url)) return;
    const targetRef = new WeakRef(response);
    patch(response,'arrayBuffer',original => function(...args) {
      const result = original.apply(this,args);
      // Borrowed methods must not inherit the response's URL.
      if (state.active && this === targetRef.deref()) observe(result,value => remember(value,url));
      return result;
    });
    const body = response.body;
    const bodyRef = body && new WeakRef(body);
    patch(body,'getReader',original => function(...args) {
      const reader = original.apply(this,args);
      if (state.active && this === bodyRef.deref() && args[0]?.mode !== 'byob') {
        const readerRef = new WeakRef(reader);
        patch(reader,'read',read => function(...readArgs) {
          const result = read.apply(this,readArgs);
          if (state.active && this === readerRef.deref()) observe(result,item => { if (item.value) remember(item.value,url); });
          return result;
        });
      }
      return reader;
    });
  }
  patch(window.URL || URL,'createObjectURL',original => function(...args) {
    const result = original.apply(this,args);
    try {
      if (state.active && typeof window.MediaSource === 'function' && args[0] instanceof window.MediaSource && tracked < MAX_TRACK) {
        const urls = objectUrls.get(args[0]) || new Set();
        if (urls.size < 8) { urls.add(result); objectUrls.set(args[0],urls); tracked++; }
      }
    } catch {}
    return result;
  });
  patch(window.MediaSource?.prototype,'addSourceBuffer',original => function(...args) {
    const result = original.apply(this,args);
    if (state.active && tracked < MAX_TRACK) { parents.set(result,this); tracked++; }
    return result;
  });
  patch(window.SourceBuffer?.prototype,'appendBuffer',original => function(...args) {
    const result = original.apply(this,args);
    try {
      if (state.active) {
        const url = provenance(args[0]), blobs = objectUrls.get(parents.get(this));
        if (url && blobs) for (const blob of blobs) {
          let known = appended.get(url);
          if (!known && appended.size < LIMIT) { known = new Set(); appended.set(url,known); }
          if (known && known.size < 8) known.add(blob);
          prove(url,blob);
        }
      }
    } catch {}
    return result;
  });
  window[KEY] = state;
  const originalFetch = window.fetch;
  const originalSend = window.XMLHttpRequest?.prototype?.send;

  function safeUrl(value) {
    if (typeof value !== 'string' || value.length > 2048) return '';
    try { const url = new URL(value); return /^https?:$/.test(url.protocol) ? url.href : ''; }
    catch { return ''; }
  }
  const mediaPath = url => /\.(?:m3u8|mpd|mp4|m4v|mov|webm|mp3|m4a|aac|flac|ogg)(?:$|[?#])/i.test(url);
  const manifestPath = url => /\.(?:m3u8|mpd)(?:$|[?#])/i.test(url);
  const sensitivePath = url => /(?:^|\/)(?:key|keys|license|drm)(?:[\/._-]|$)|\.key(?:$|[?#])/i.test(new URL(url).pathname);
  const manifestMime = mime => /^(?:application\/(?:vnd\.apple\.mpegurl|x-mpegurl|dash\+xml)|audio\/(?:mpegurl|x-mpegurl))\b/i.test(mime);
  const jsonMime = mime => /(?:^|\/)json\b|\+json\b/i.test(mime);
  function category(url,mime) {
    if (!safeUrl(url) || sensitivePath(url)) return '';
    if (manifestMime(mime)) return 'manifest';
    if (/^(?:video|audio)\//i.test(mime)) return '';
    if (manifestPath(url)) return 'manifest';
    // Explicit, bounded search must not depend on sites naming APIs "player".
    // Only media references are emitted; arbitrary JSON state is never retained.
    if (jsonMime(mime)) return 'json';
    return '';
  }
  function publish(url,mime,relatedUrls=[],contextUrls=[],more={}) {
    url = safeUrl(url);
    if (!state.active || !url || sensitivePath(url) || (!more.mediaObjectUrl && state.urls.has(url)) || (!state.urls.has(url) && state.urls.size >= LIMIT)) return;
    state.urls.add(url);
    window.postMessage({type:'GOPEED_MEDIA_CANDIDATE',url,mime:mime || '',relatedUrls,contextUrls,searchId:state.searchId,...more},'*');
  }
  function reference(value,base,found,limit,allowSensitive=false) {
    if (typeof value !== 'string' || value.length > 2048 || found.size >= limit) return;
    try {
      const url = safeUrl(new URL(value.trim(),base).href);
      if (url && url !== base && (allowSensitive || !sensitivePath(url))) found.add(url);
    } catch { /* Ignore invalid manifest references. */ }
  }
  const related = (value,base,found) => reference(value,base,found,16);
  const context = (value,base,found) => reference(value,base,found,64,true);
  const uri = line => { const match = line.match(/\bURI\s*=\s*(?:"([^"]*)"|'([^']*)'|([^,\s]*))/i); return match?.[1] ?? match?.[2] ?? match?.[3]; };
  function hlsRelated(text,base) {
    const found = new Set(), contextUrls = new Set(), refs = new Set();
    let variant = false;
    for (const raw of text.split(/\r?\n/)) {
      const line = raw.trim();
      if (!line) continue;
      if (/^#EXT-X-STREAM-INF:/i.test(line)) { variant = true; continue; }
      if (/^#EXT-X-MEDIA:/i.test(line)) {
        related(uri(line),base,found);
        reference(uri(line),base,refs,64);
      }
      if (/^#EXT-X-(?:KEY|SESSION-KEY|MAP):/i.test(line)) context(uri(line),base,contextUrls);
      if (/^#EXT-X-MAP:/i.test(line)) reference(uri(line),base,refs,64);
      if (line.startsWith('#')) { variant = false; continue; }
      if (variant) related(line,base,found);
      else context(line,base,contextUrls);
      reference(line,base,refs,64);
      variant = false;
    }
    return {relatedUrls:[...found],contextUrls:[...contextUrls],refs};
  }
  function dashRelated(text,base) {
    const found = new Set(), contextUrls = new Set();
    const bases = [base];
    for (const match of text.matchAll(/<BaseURL(?:\s[^>]*)?>([^<]+)<\/BaseURL>/gi)) {
      related(match[1],base,found);
      context(match[1],base,contextUrls);
      try { if (bases.length < 16) bases.push(new URL(match[1].trim(),base).href); } catch {}
    }
    for (const match of text.matchAll(/<(?:SegmentURL|Initialization|SegmentTemplate)(?:\s[^>]*?)?\s(?:media|sourceURL|initialization)\s*=\s*["']([^"']+)["']/gi)) {
      // DASH templates contain placeholders, not concrete request URLs.
      if (!match[1].includes('$')) for (const path of bases) context(match[1],path,contextUrls);
    }
    // Existing discovery tolerates alternative BaseURLs; proof must not invent
    // cross-products of scoped DASH bases. Only unbased explicit references qualify.
    const refs = new Set();
    const explicit = text.replace(/<!--[\s\S]*?-->|<!\[CDATA\[[\s\S]*?\]\]>/g,'');
    if (!/<BaseURL\b|xml:base\s*=|<!DOCTYPE/i.test(explicit)) {
      for (const match of explicit.matchAll(/<(?:SegmentURL|Initialization)\b[^>]*>/gi)) {
        for (const attr of match[0].matchAll(/\b(?:media|sourceURL)\s*=\s*["']([^"']+)["']/gi)) {
          if (!/[$&]/.test(attr[1])) reference(attr[1],base,refs,64);
        }
      }
    }
    return {relatedUrls:[...found],contextUrls:[...contextUrls],refs};
  }
  function extractJson(text,base) {
    let parsed;
    try { parsed = JSON.parse(text); } catch { return; }
    const stack = [{value:parsed,depth:0,key:'',hint:''}];
    let visited = 0;
    while (stack.length && visited++ < 512 && state.active && state.urls.size < LIMIT) {
      const {value,depth,key,hint} = stack.pop();
      if (typeof value === 'string') {
        const explicit = /^(?:playback|manifest|playlist|hls|dash|media|stream|video|audio)(?:_?url|_?src)?$/i.test(key);
        const relative = explicit && (/^(?:\/|\.\.?\/)/.test(value) || mediaPath(value));
        let url = safeUrl(value);
        if (!url && relative) { try { url = safeUrl(new URL(value,base).href); } catch {} }
        if (url && (mediaPath(url) || explicit)) {
          const dash = /\.mpd(?:$|[?#])/i.test(url) || /dash/i.test(key) || /\bdash\b|application\/dash\+xml/i.test(hint);
          const hls = /\.m3u8(?:$|[?#])/i.test(url) || /hls/i.test(key) || /\bhls\b|mpegurl/i.test(hint);
          const opaqueManifest = /manifest|playlist/i.test(key) || /manifest|playlist/i.test(hint);
          const mime = dash ? 'application/dash+xml' : hls ? 'application/vnd.apple.mpegurl' : opaqueManifest ? '' : /\.(?:mp3|m4a|aac|flac|ogg)(?:$|[?#])/i.test(url) ? 'audio/mpeg' : 'video/mp4';
          publish(url,mime,[],[],opaqueManifest || dash || hls ? {manifest:true,protocolHint:'manifest'} : {mediaRequired:!mediaPath(url)});
        }
      } else if (value && typeof value === 'object' && depth < 10) {
        const entries = Array.isArray(value) ? value.slice(0,64).map((item,index) => [String(index),item]) : Object.entries(value).slice(0,64);
        const ownHint = !Array.isArray(value) ? ['type','format','mime','contentType','protocol'].map(name => value[name]).filter(part => typeof part === 'string').join(' ') : '';
        for (const [name,item] of entries) stack.push({value:item,depth:depth+1,key:name,hint:ownHint || hint});
      }
    }
  }
  function parse(text,kind,url,mime) {
    if (!state.active) return;
    if (kind === 'json') { extractJson(text,url); return; }
    if (/^\s*#EXTM3U(?:\s|$)/.test(text)) manifest(url,'application/vnd.apple.mpegurl',hlsRelated(text,url));
    else if (/<MPD(?:\s|>)/i.test(text.slice(0,4096))) manifest(url,'application/dash+xml',dashRelated(text,url));
  }
  async function inspectResponse(response) {
    if (!state.active || !response?.ok || state.used >= TOTAL) return;
    const url = response.url, mime = response.headers?.get('content-type') || '';
    const kind = category(url,mime);
    if (!kind || Number(response.headers?.get('content-length')) > SINGLE) return;
    let clone, reader;
    try {
      clone = response.clone();
      reader = clone.body?.getReader();
      if (!reader) return;
      readers.add(reader);
      const decoder = new TextDecoder();
      let size = 0, text = '';
      while (state.active && size <= SINGLE && state.used < TOTAL) {
        const {done,value} = await reader.read();
        if (done) {
          text += decoder.decode();
          parse(text,kind,url,mime);
          return;
        }
        const bytes = value?.byteLength || 0;
        if (!bytes) return;
        if (bytes > SINGLE - size || bytes > TOTAL - state.used) { state.used = Math.min(TOTAL,state.used+bytes); return; }
        size += bytes; state.used += bytes;
        text += decoder.decode(value,{stream:true});
      }
    } catch { /* The original response still belongs to the page. */ }
    finally { readers.delete(reader); try { const canceled = reader?.cancel(); canceled?.catch?.(() => {}); } catch { /* Ignore clone cancellation. */ } }
  }
  function fetchHook(...args) {
    const promise = originalFetch.apply(this,args);
    observe(promise,response => { if (typeof WeakRef === 'function') inspectMedia(response); void inspectResponse(response); });
    return promise;
  }
  if (typeof originalFetch === 'function') window.fetch = fetchHook;

  function xhrHook(...args) {
    // Capture runs before ordinary target listeners consuming an XHR arraybuffer.
    // BYOB streams and buffers consumed before these hooks are intentionally unsupported.
    if (state.active && typeof WeakRef === 'function' && typeof this.addEventListener === 'function' && xhrTargets.size < MAX_TRACK) {
      const previous = xhrListeners.get(this);
      if (previous) this.removeEventListener?.('readystatechange',previous,true);
      const listener = () => {
        try {
          if (this.readyState !== 4) return;
          this.removeEventListener?.('readystatechange',listener,true);
          if (this.status >= 200 && this.status < 300 && this.responseType === 'arraybuffer') remember(this.response,this.responseURL);
        } catch {}
      };
      xhrListeners.set(this,listener); xhrTargets.add(new WeakRef(this));
      this.addEventListener('readystatechange',listener,{capture:true});
    }
    if (state.active && typeof this.addEventListener === 'function') this.addEventListener('loadend',() => {
      if (!state.active || this.readyState !== 4 || this.status < 200 || this.status >= 300 || state.used >= TOTAL || (this.responseType && this.responseType !== 'text')) return;
      const url = this.responseURL, mime = this.getResponseHeader?.('content-type') || '';
      const kind = category(url,mime);
      if (!kind || Number(this.getResponseHeader?.('content-length')) > SINGLE) return;
      try {
        const text = this.responseText;
        if (typeof text !== 'string' || text.length > SINGLE) return;
        const bytes = new TextEncoder().encode(text).byteLength;
        if (bytes > SINGLE || bytes > TOTAL - state.used) return;
        state.used += bytes; parse(text,kind,url,mime);
      } catch { /* Binary and inaccessible XHR bodies are ignored. */ }
    },{once:true});
    return originalSend.apply(this,args);
  }
  if (typeof originalSend === 'function') window.XMLHttpRequest.prototype.send = xhrHook;
  state.stop = () => {
    if (!state.active) return;
    state.active = false;
    clearTimeout(timer);
    for (const reader of readers) { try { reader.cancel()?.catch?.(() => {}); } catch {} }
    readers.clear();
    for (const ref of targets) {
      const target = ref.deref();
      if (!target) continue;
      for (const {key,descriptor,wrapper} of restorations.get(target) || []) {
        try {
          if (Object.getOwnPropertyDescriptor(target,key)?.value !== wrapper) continue;
          if (descriptor) Object.defineProperty(target,key,descriptor);
          else delete target[key];
        } catch {}
      }
    }
    for (const ref of xhrTargets) {
      const xhr = ref.deref();
      try { if (xhr) xhr.removeEventListener?.('readystatechange',xhrListeners.get(xhr),true); } catch {}
    }
    xhrTargets.clear();
    targets.clear(); manifests.clear(); appended.clear(); emittedProofs.clear();
    if (window.fetch === fetchHook) window.fetch = originalFetch;
    if (window.XMLHttpRequest?.prototype?.send === xhrHook) window.XMLHttpRequest.prototype.send = originalSend;
    if (window[KEY] === state) delete window[KEY];
  };
  const timer = setTimeout(state.stop,60000);
})();
