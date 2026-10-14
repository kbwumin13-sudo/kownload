import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';

const script = fs.readFileSync(new URL('../deep-search.js',import.meta.url),'utf8');
const bytes = text => new TextEncoder().encode(text);
function response(url,mime,chunks,{length} = {}) {
  const record = {clones:0,reads:0,cancels:0};
  const value = {ok:true,url,headers:{get(name) {
    if (name === 'content-type') return mime;
    if (name === 'content-length') return length ?? null;
    return null;
  }},originalBody:chunks.map(chunk => typeof chunk === 'string' ? chunk : new TextDecoder().decode(chunk)).join(''),
  clone() {
    record.clones++;
    let index = 0;
    return {body:{getReader() { return {
      async read() { record.reads++; return index < chunks.length ? {done:false,value:typeof chunks[index] === 'string' ? bytes(chunks[index++]) : chunks[index++]} : {done:true}; },
      async cancel() { record.cancels++; }
    }; }}};
  }};
  return {value,record};
}
function harness(searchId) {
  const posted = [], timers = new Map();
  let nextTimer = 0, promise;
  const originalFetch = () => promise;
  class XHR {
    constructor() { this.listeners = {}; this.readyState = 4; this.status = 200; this.responseType = ''; }
    addEventListener(name,fn) { this.listeners[name] = fn; }
    getResponseHeader(name) { return this.headers?.[name] ?? null; }
    removeEventListener(name,fn) { if (this.listeners[name] === fn) delete this.listeners[name]; }
    send() { this.listeners.readystatechange?.(); this.onLoad?.(); this.listeners.loadend?.(); return 'original-send'; }
  }
  const originalSend = XHR.prototype.send;
  class SourceBuffer {
    appendBuffer(value) { if (this.fail) throw new Error('append failed'); this.last = value; return 'append-result'; }
  }
  class MediaSource { addSourceBuffer() { return new SourceBuffer(); } }
  class LocalURL extends URL { static createObjectURL(value) { return `blob:test/${++nextTimer}`; } }
  const originals = {create:LocalURL.createObjectURL,add:MediaSource.prototype.addSourceBuffer,append:SourceBuffer.prototype.appendBuffer};
  const window = {URL:LocalURL,MediaSource,SourceBuffer,fetch:originalFetch,XMLHttpRequest:XHR,__gopeedMediaSearchId:searchId,
    postMessage(data) { if (data.type === 'GOPEED_MEDIA_CANDIDATE') posted.push(data); }};
  const context = vm.createContext({window,URL:LocalURL,TextDecoder,TextEncoder,Promise,
    setTimeout(fn,ms) { const id = ++nextTimer; timers.set(id,{fn,ms}); return id; },
    clearTimeout(id) { timers.delete(id); }});
  vm.runInContext(script,context);
  return {window,posted,originalFetch,originalSend,originals,timers,
    setResponse(value) { promise = Promise.resolve(value); return promise; },
    expire() { for (const [id,timer] of timers) { timers.delete(id); timer.fn(); } },
    async flush() { await settle(); await settle(); }};
}

test('fetch keeps the exact original Promise and Response while reporting a confirmed HLS manifest', async () => {
  const h = harness();
  const {value,record} = response('https://cdn.test/master.m3u8','application/vnd.apple.mpegurl',['#EXTM3U\n','#EXT-X-STREAM-INF:'],{});
  const originalPromise = h.setResponse(value);
  assert.strictEqual(h.window.fetch('/master.m3u8'),originalPromise);
  assert.strictEqual(await originalPromise,value);
  await h.flush();
  assert.equal(record.clones,1);
  assert.equal(value.originalBody,'#EXTM3U\n#EXT-X-STREAM-INF:');
  assert.equal(h.posted.length,1);
  assert.equal(h.posted[0].url,value.url);
});

test('HLS relatedUrls contain only variant and EXT-X-MEDIA references, capped at 16', async () => {
  const h = harness();
  const lines = ['#EXTM3U','#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",URI="../audio/a.m3u8"','#EXT-X-STREAM-INF:BANDWIDTH=800000','low/playlist.m3u8','#EXTINF:4,','segment-1.ts','#EXT-X-KEY:METHOD=AES-128,URI="key.key"'];
  for (let i=0;i<20;i++) lines.push('#EXT-X-STREAM-INF:BANDWIDTH=1000',`variant-${i}.m3u8`);
  const item = response('https://cdn.test/live/master.m3u8','application/vnd.apple.mpegurl',[lines.join('\n')]);
  h.setResponse(item.value); await h.window.fetch('/'); await h.flush();
  const related = h.posted[0].relatedUrls;
  assert.equal(related.length,16);
  assert.deepEqual(Array.from(related.slice(0,2)),['https://cdn.test/audio/a.m3u8','https://cdn.test/live/low/playlist.m3u8']);
  assert.ok(related.every(url => !url.includes('segment') && !url.includes('key.key')));
});

test('DASH and media JSON are discovered without guessing endpoint names', async () => {
  const h = harness();
  const dash = response('https://cdn.test/live.mpd','application/dash+xml',['<?xml version="1.0"?><MPD type="static"><BaseURL>https://media.test/streams/</BaseURL></MPD>']);
  h.setResponse(dash.value); await h.window.fetch('/dash'); await h.flush();
  assert.equal(h.posted[0].url,dash.value.url);
  assert.equal(h.posted[0].mime,'application/dash+xml');
  assert.deepEqual(Array.from(h.posted[0].relatedUrls),['https://media.test/streams/']);
  const unrelated = response('https://api.test/catalog','application/json',[JSON.stringify({videoUrl:'https://cdn.test/extra.mp4'})]);
  h.setResponse(unrelated.value); await h.window.fetch('/catalog'); await h.flush();
  assert.equal(unrelated.record.clones,1);
  assert.equal(h.posted.length,2);
  assert.equal(h.posted[1].url,'https://cdn.test/extra.mp4');
});

test('real streamed bytes stop at the per-response limit; large media responses are never cloned', async () => {
  const h = harness();
  const large = response('https://cdn.test/master.m3u8','application/vnd.apple.mpegurl',[bytes('#EXTM3U\n'),new Uint8Array(1024*1024)]);
  h.setResponse(large.value); await h.window.fetch('/a'); await h.flush();
  assert.equal(large.record.clones,1);
  assert.equal(large.record.cancels,1);
  assert.equal(h.posted.length,0);
  const movie = response('https://cdn.test/video.mp4','video/mp4',[bytes('x')]);
  h.setResponse(movie.value); await h.window.fetch('/b'); await h.flush();
  assert.equal(movie.record.clones,0);
});

test('aggregate byte budget stops later eligible responses', async () => {
  const h = harness();
  for (let i=0;i<5;i++) {
    const item = response(`https://cdn.test/${i}.m3u8`,'application/vnd.apple.mpegurl',[`#EXTM3U\n${'x'.repeat(900000)}`]);
    h.setResponse(item.value); await h.window.fetch('/'); await h.flush();
  }
  assert.equal(h.posted.length,4);
});

test('bounded JSON emits explicit media URLs only, and XHR ignores binary and oversize bodies', async () => {
  const h = harness();
  const json = response('https://api.test/player','application/json',[JSON.stringify({image:'https://cdn.test/cover.jpg',streams:[{url:'https://cdn.test/a.m3u8'},{mediaUrl:'https://cdn.test/opaque?sig=keep'},{mediaUrl:'https://cdn.test/key?sig=secret'}]})]);
  h.setResponse(json.value); await h.window.fetch('/'); await h.flush();
  assert.deepEqual(h.posted.map(x => x.url),['https://cdn.test/opaque?sig=keep','https://cdn.test/a.m3u8']);
  const xhr = new h.window.XMLHttpRequest();
  Object.assign(xhr,{responseURL:'https://api.test/media',headers:{'content-type':'application/json'},responseText:JSON.stringify({videoUrl:'https://cdn.test/c.mp4'})});
  assert.equal(xhr.send(),'original-send');
  assert.equal(h.posted.at(-1).url,'https://cdn.test/c.mp4');
  const binary = new h.window.XMLHttpRequest();
  Object.assign(binary,{responseURL:'https://api.test/media',responseType:'arraybuffer',headers:{'content-type':'application/json'},responseText:JSON.stringify({videoUrl:'https://cdn.test/d.mp4'})});
  binary.send();
  const tooLarge = new h.window.XMLHttpRequest();
  Object.assign(tooLarge,{responseURL:'https://api.test/media',headers:{'content-type':'application/json'},responseText:'x'.repeat(1024*1024+1)});
  tooLarge.send();
  assert.equal(h.posted.length,3);
});

test('expiry restores only its own hooks and does not replace a later fetch wrapper', async () => {
  const h = harness();
  const later = (...args) => h.originalFetch(...args);
  h.window.fetch = later;
  h.expire();
  assert.strictEqual(h.window.fetch,later);
  assert.strictEqual(h.window.XMLHttpRequest.prototype.send,h.originalSend);
  const h2 = harness();
  h2.expire();
  assert.strictEqual(h2.window.fetch,h2.originalFetch);
});

test('explicit relative manifest URLs resolve against the configuration response without turning filenames into URLs',async () => {
  const h = harness();
  const item = response('https://api.test/player/config.json','application/json',[JSON.stringify({manifest:'/streams/master.m3u8?sig=keep',name:'poster.mp4'})]);
  h.setResponse(item.value);await h.window.fetch('/player/config.json');await h.flush();
  assert.equal(h.posted.length,1);
  assert.equal(h.posted[0].url,'https://api.test/streams/master.m3u8?sig=keep');
});

test('opaque JSON manifests keep manifest intent and search identity',async () => {
  const h = harness('search-1');
  const item = response('https://api.test/player','application/json',[JSON.stringify({streams:[
    {manifestUrl:'https://cdn.test/opaque?token=1'},
    {manifestUrl:'https://cdn.test/opaque-dash',format:'dash'},
    {mediaUrl:'https://cdn.test/movie.mp4'}]})]);
  h.setResponse(item.value); await h.window.fetch('/'); await h.flush();
  const opaque = h.posted.find(x => x.url.includes('opaque?'));
  assert.equal(opaque.mime,''); assert.equal(opaque.manifest,true);
  assert.equal(opaque.protocolHint,'manifest'); assert.equal(opaque.searchId,'search-1');
  assert.equal(h.posted.find(x => x.url.includes('opaque-dash')).mime,'application/dash+xml');
});

test('manifest context URLs are separate and bounded; keys never become candidates',async () => {
  const h = harness();
  const lines = ['#EXTM3U','#EXT-X-KEY:METHOD=AES-128,URI="key.key"','#EXT-X-MAP:URI="init.mp4"','#EXTINF:4,'];
  for (let i=0;i<80;i++) lines.push(`segment-${i}.ts`);
  const item = response('https://cdn.test/live/index.m3u8','application/vnd.apple.mpegurl',[lines.join('\n')]);
  h.setResponse(item.value); await h.window.fetch('/'); await h.flush();
  assert.equal(h.posted.length,1);
  assert.equal(h.posted[0].contextUrls.length,64);
  assert.ok(h.posted[0].contextUrls.includes('https://cdn.test/live/key.key'));
  assert.ok(h.posted[0].contextUrls.includes('https://cdn.test/live/init.mp4'));
  assert.ok(h.posted[0].relatedUrls.every(url => !url.includes('key.key') && !url.includes('segment-')));
  const dash = response('https://cdn.test/live/index.mpd','application/dash+xml',['<MPD><BaseURL>segments/</BaseURL><SegmentURL media="one.m4s"/><Initialization sourceURL="init.mp4"/></MPD>']);
  h.setResponse(dash.value); await h.window.fetch('/'); await h.flush();
  assert.ok(h.posted.at(-1).contextUrls.includes('https://cdn.test/live/one.m4s'));
});

async function loadManifest(h,url,text) {
  const item = response(url,url.endsWith('.mpd') ? 'application/dash+xml' : 'application/vnd.apple.mpegurl',[text]);
  h.setResponse(item.value); await h.window.fetch(url); await h.flush();
}
function mse(h) {
  const source = new h.window.MediaSource();
  const blob = h.window.URL.createObjectURL(source);
  return {source,blob,buffer:source.addSourceBuffer('video/mp4')};
}
function binary(url,data = new ArrayBuffer(16)) {
  const item = response(url,'video/mp4',[]);
  const promise = Promise.resolve(data);
  item.value.arrayBuffer = function() { assert.equal(this,item.value); return promise; };
  return {...item,data,promise};
}
const proofs = h => h.posted.filter(item => item.mediaObjectUrl);

test('exact response reference proves segment -> variant -> root only for its MediaSource; no binary clone',async () => {
  const h = harness('proof-search'), first = mse(h), other = mse(h);
  await loadManifest(h,'https://cdn.test/root.m3u8','#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nvariant.m3u8');
  await loadManifest(h,'https://cdn.test/variant.m3u8','#EXTM3U\n#EXTINF:4,\nsegment.m4s');
  const item = binary('https://cdn.test/segment.m4s');
  const fetchPromise = h.setResponse(item.value);
  assert.strictEqual(h.window.fetch('/segment'),fetchPromise);
  assert.strictEqual(await fetchPromise,item.value);
  assert.strictEqual(item.value.arrayBuffer(),item.promise);
  assert.strictEqual(await item.promise,item.data);
  assert.equal(proofs(h).length,0);
  assert.equal(first.buffer.appendBuffer(new Uint8Array(item.data)),'append-result');
  assert.strictEqual(first.buffer.last.buffer,item.data);
  assert.deepEqual(proofs(h).map(x => x.url),['https://cdn.test/variant.m3u8','https://cdn.test/root.m3u8']);
  assert.ok(proofs(h).every(x => x.mediaObjectUrl === first.blob && x.mediaObjectUrl !== other.blob && x.searchId === 'proof-search'));
  assert.equal(item.record.clones,0); assert.equal(item.record.reads,0);
  h.expire();
});

test('copies, unknown buffers, pre-hook sources and failed append never prove association',async () => {
  const h = harness(), current = mse(h);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  const item = binary('https://cdn.test/segment.m4s');
  h.setResponse(item.value); await h.window.fetch('/'); await item.value.arrayBuffer();
  current.buffer.appendBuffer(item.data.slice(0));
  current.buffer.appendBuffer(new Uint8Array(16));
  const old = new h.window.SourceBuffer(); old.appendBuffer(item.data);
  current.buffer.fail = true;
  assert.throws(() => current.buffer.appendBuffer(item.data),/append failed/);
  assert.equal(proofs(h).length,0); h.expire();
});

test('stream reads keep receiver, reader and Promise identity and prove only the observed byte range',async () => {
  const h = harness(), current = mse(h), storage = new ArrayBuffer(24);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  const item = binary('https://cdn.test/segment.m4s');
  const value = new Uint8Array(storage,8,8), result = {done:false,value}, promise = Promise.resolve(result);
  const reader = {read() { assert.strictEqual(this,reader); return promise; }};
  const body = {getReader() { assert.strictEqual(this,body); return reader; }};
  item.value.body = body;
  h.setResponse(item.value); await h.window.fetch('/');
  assert.strictEqual(body.getReader(),reader);
  assert.strictEqual(reader.read(),promise);
  assert.strictEqual(await promise,result);
  current.buffer.appendBuffer(new Uint8Array(storage,0,8));
  current.buffer.appendBuffer(storage);
  assert.equal(proofs(h).length,0);
  current.buffer.appendBuffer(new DataView(storage,8,8));
  assert.equal(proofs(h)[0].mediaObjectUrl,current.blob);
  assert.equal(item.record.clones,0); h.expire();
});

test('XHR arraybuffer is associated before the page load consumer without reading responseText',async () => {
  const h = harness(), current = mse(h);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  const xhr = new h.window.XMLHttpRequest();
  Object.assign(xhr,{responseType:'arraybuffer',response:new ArrayBuffer(8),responseURL:'https://cdn.test/segment.m4s'});
  Object.defineProperty(xhr,'responseText',{get() { throw new Error('must not read binary'); }});
  xhr.onLoad = () => current.buffer.appendBuffer(xhr.response);
  assert.equal(xhr.send(),'original-send');
  assert.equal(proofs(h)[0].mediaObjectUrl,current.blob); h.expire();
});

test('late explicit DASH graph resolves saved URL metadata; scoped bases and templates stay manual',async () => {
  const h = harness(), current = mse(h);
  const item = binary('https://cdn.test/one.m4s');
  h.setResponse(item.value); await h.window.fetch('/'); await item.value.arrayBuffer();
  current.buffer.appendBuffer(item.data);
  assert.equal(proofs(h).length,0);
  await loadManifest(h,'https://cdn.test/scoped.mpd','<MPD><BaseURL>https://cdn.test/</BaseURL><SegmentURL media="one.m4s"/></MPD>');
  await loadManifest(h,'https://cdn.test/template.mpd','<MPD><SegmentTemplate media="$Number$.m4s"/></MPD>');
  assert.equal(proofs(h).length,0);
  await loadManifest(h,'https://cdn.test/main.mpd','<MPD><SegmentList><SegmentURL media="one.m4s"/></SegmentList></MPD>');
  assert.equal(proofs(h).length,1);
  assert.equal(proofs(h)[0].url,'https://cdn.test/main.mpd'); h.expire();
});

test('shared segment references and overlapping origins never choose the nearest request',async () => {
  const h = harness(), current = mse(h);
  for (const name of ['a','b']) await loadManifest(h,`https://cdn.test/${name}.m3u8`,'#EXTM3U\nshared.m4s');
  const data = new ArrayBuffer(8);
  for (const url of ['https://cdn.test/shared.m4s','https://other.test/unrelated.m4s']) {
    const item = binary(url,data); h.setResponse(item.value); await h.window.fetch('/'); await item.value.arrayBuffer();
    current.buffer.appendBuffer(data);
  }
  assert.equal(proofs(h).length,0); h.expire();
});

test('expiry restores response/reader descriptors and MSE hooks while preserving later third-party wrappers',async () => {
  const h = harness(), current = mse(h), item = binary('https://cdn.test/segment.m4s');
  const originalArrayBuffer = item.value.arrayBuffer;
  const reader = {read() { return Promise.resolve({done:true}); }};
  const originalRead = reader.read;
  const body = {getReader() { return reader; }};
  const originalGetReader = body.getReader;
  item.value.body = body;
  h.setResponse(item.value); await h.window.fetch('/'); body.getReader();
  const later = function(...args) { return originalArrayBuffer.apply(this,args); };
  item.value.arrayBuffer = later;
  h.expire();
  assert.strictEqual(item.value.arrayBuffer,later);
  assert.strictEqual(body.getReader,originalGetReader);
  assert.strictEqual(reader.read,originalRead);
  assert.strictEqual(h.window.URL.createObjectURL,h.originals.create);
  assert.strictEqual(h.window.MediaSource.prototype.addSourceBuffer,h.originals.add);
  assert.strictEqual(h.window.SourceBuffer.prototype.appendBuffer,h.originals.append);
  current.buffer.appendBuffer(item.data); assert.equal(proofs(h).length,0);
});

test('bounded wrapper tracking falls back to manual and deferred results cannot revive expired proof',async () => {
  const h = harness(), current = mse(h);
  assert.deepEqual([...h.timers.values()].map(x => x.ms),[60000]);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  let resolve;
  const delayed = binary('https://cdn.test/segment.m4s');
  const pending = new Promise(done => { resolve = done; });
  const original = function() { return pending; };
  delayed.value.arrayBuffer = original;
  h.setResponse(delayed.value); await h.window.fetch('/');
  assert.strictEqual(delayed.value.arrayBuffer(),pending);
  let last;
  for (let i=0;i<520;i++) {
    last = binary('https://cdn.test/segment.m4s');
    last.original = last.value.arrayBuffer;
    h.setResponse(last.value); await h.window.fetch('/');
  }
  assert.strictEqual(last.value.arrayBuffer,last.original);
  await last.value.arrayBuffer(); current.buffer.appendBuffer(last.data);
  assert.equal(proofs(h).length,0);
  h.expire(); resolve(delayed.data); await pending; await h.flush();
  assert.strictEqual(delayed.value.arrayBuffer,original);
  current.buffer.appendBuffer(delayed.data);
  assert.equal(proofs(h).length,0);
});

test('borrowed response methods and BYOB readers do not borrow provenance',async () => {
  const h = harness(), current = mse(h);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  const item = binary('https://cdn.test/segment.m4s'), stranger = binary('https://other.test/unknown');
  item.value.arrayBuffer = function() { return Promise.resolve(this.data); };
  item.value.data = item.data; stranger.value.data = stranger.data;
  const reader = {read() { return Promise.resolve({done:false,value:new Uint8Array(item.data)}); }};
  item.value.body = {getReader() { return reader; }};
  h.setResponse(item.value); await h.window.fetch('/');
  const borrowed = await item.value.arrayBuffer.call(stranger.value);
  current.buffer.appendBuffer(borrowed);
  const byob = item.value.body.getReader({mode:'byob'}); await byob.read();
  current.buffer.appendBuffer(item.data);
  assert.equal(proofs(h).length,0); h.expire();
});

test('later MSE and reader wrappers survive expiry; inherited methods lose our own shadow',async () => {
  const h = harness(), item = binary('https://cdn.test/segment.m4s');
  const proto = {read() { return Promise.resolve({done:true}); }};
  const reader = Object.create(proto), body = {getReader() { return reader; }};
  item.value.body = body;
  h.setResponse(item.value); await h.window.fetch('/'); body.getReader();
  assert.equal(Object.hasOwn(reader,'read'),true);
  const later = function() {};
  h.window.SourceBuffer.prototype.appendBuffer = later;
  h.window.URL.createObjectURL = later;
  h.expire();
  assert.equal(Object.hasOwn(reader,'read'),false);
  assert.strictEqual(reader.read,proto.read);
  assert.strictEqual(h.window.SourceBuffer.prototype.appendBuffer,later);
  assert.strictEqual(h.window.URL.createObjectURL,later);
});

test('overlapping ranges with different response origins remain unproven even for a unique manifest',async () => {
  const h = harness(), current = mse(h), data = new ArrayBuffer(16);
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\nsegment.m4s');
  for (const url of ['https://cdn.test/segment.m4s','https://other.test/not-a-segment']) {
    const item = binary(url,data); h.setResponse(item.value); await h.window.fetch('/'); await item.value.arrayBuffer();
  }
  current.buffer.appendBuffer(new Uint8Array(data,4,8));
  assert.equal(proofs(h).length,0); h.expire();
});

test('DASH comments and HLS key references do not establish a segment graph',async () => {
  const h = harness(), current = mse(h);
  await loadManifest(h,'https://cdn.test/main.mpd','<MPD><!-- <SegmentURL media="secret"/> --><![CDATA[<SegmentURL media="secret"/>]]></MPD>');
  await loadManifest(h,'https://cdn.test/main.m3u8','#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI="secret"');
  const item = binary('https://cdn.test/secret');
  h.setResponse(item.value); await h.window.fetch('/'); await item.value.arrayBuffer();
  current.buffer.appendBuffer(item.data);
  assert.equal(proofs(h).length,0); h.expire();
});
