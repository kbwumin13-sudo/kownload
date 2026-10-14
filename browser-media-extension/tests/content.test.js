import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';

function node(localName = 'div', props = {}) {
  return {nodeType:1,localName,children:[],isConnected:true,style:{},attributes:{},
    getAttribute(name) { return this.attributes[name] ?? null; },
    querySelector() { return null; },querySelectorAll() { return []; },
    closest(selector) { return this.match?.(selector) ? this : null; },
    getBoundingClientRect() { return {width:640,height:360,top:20,bottom:380,left:0,right:640}; },
    append(child) { child.parentElement = this; this.children.push(child); },
    ...props};
}

async function page({url='https://example.test/watch',baseURI=url,roots=[],intersection=false,selectors={}} = {}) {
  const root = node('html'), pending = [], longTimers = [], messages = [], listeners = {}, buttons = [], observed = [];
  roots.forEach(child => root.append(child));
  let mutation, intersectionCallback, styleReads = 0;
  const document = {title:'媒体示例',documentElement:root,baseURI,
    createElement:() => {
      const button = node('button',{textContent:'',disabled:false,
        addEventListener(name,fn) { this[name] = fn; },
        remove() { this.removed = true; }});
      buttons.push(button); return button;
    },
    querySelectorAll(selector) {return selectors[selector] || [];},
    addEventListener(name,fn) { listeners[name] = fn; }};
  const runtime = {onMessage:{addListener(fn) { listeners.runtimeMessage = fn; }},sendMessage(message) {
    messages.push(message);
    return Promise.resolve(message.type === 'ENABLED' ? {ok:true,data:true} : {ok:true});
  }};
  class MutationObserver { constructor(fn) { mutation = fn; } observe(target) { observed.push(target); } }
  const window = {addEventListener(name,fn) { listeners[name] = fn; },requestAnimationFrame(fn) { assert.strictEqual(this,window); pending.push(fn); }};
  const context = vm.createContext({document,chrome:{runtime},MutationObserver,URL,
    location:{href:url},window,
    ...(intersection ? {IntersectionObserver:class { constructor(fn) { intersectionCallback = fn; } observe() {} unobserve() {} }} : {}),
    innerHeight:800,innerWidth:1200,setTimeout:(fn,ms) => { (ms >= 60000 ? longTimers : pending).push(fn); return fn; },
    clearTimeout:fn => { const i = longTimers.indexOf(fn); if (i >= 0) longTimers.splice(i,1); },setInterval:() => {},
    getComputedStyle(el,pseudo) { styleReads++; return {backgroundImage:pseudo ? el.pseudoBackground?.[pseudo] || 'none' : el.backgroundImage || 'none'}; }});
  vm.runInContext(fs.readFileSync(new URL('../content.js',import.meta.url),'utf8'),context);
  async function flush() {
    await settle();
    let steps = 0;
    while (pending.length) { pending.shift()(); if (++steps > 1000) throw Error('runaway scheduler'); }
  }
  await flush();
  return {root,buttons,messages,listeners,observed,flush,window:context.window,
    expireSearch() { longTimers.shift()?.(); },
    mutate(records) { mutation(records); },
    styleReads:() => styleReads,
    intersect(target,isIntersecting) { intersectionCallback([{target,isIntersecting}]); },
    setUrl(next) { context.location.href = next; },
    click:button => button.click({preventDefault(){},stopPropagation(){}})};
}

test('each player keeps a stable id and click uses its own current source', async () => {
  const a = node('video',{currentSrc:'https://cdn.test/a.mp4'});
  const b = node('video',{currentSrc:'https://cdn.test/b.mp4'});
  const p = await page({roots:[a,b]});
  assert.equal(p.buttons.length,2);
  const items = p.messages.filter(m => m.type === 'ADD_DOM').flatMap(m => m.items).filter(i => i.kind === 'video');
  assert.equal(new Set(items.map(i => i.playerId)).size,2);
  a.currentSrc = 'https://cdn.test/a-new.mp4?key=keep';
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,a.currentSrc);
  assert.equal(p.messages.at(-1).playerId,items[0].playerId);
  await p.click(p.buttons[1]);
  assert.equal(p.messages.at(-1).url,b.currentSrc);
  assert.equal(p.messages.at(-1).playerId,items[1].playerId);
});

test('unknown blob opens explicit selection with an empty URL', async () => {
  const p = await page({roots:[node('video',{currentSrc:'blob:https://example.test/opaque'})]});
  assert.equal(p.buttons[0].textContent,'识别这个视频');
  assert.equal(p.buttons[0].disabled,false);
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).type,'PANEL');
  assert.equal(p.messages.at(-1).url,'');
  assert.ok(p.messages.at(-1).playerId);
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,0);
});

test('explicit manifest wins over page adapter; YouTube and Bilibili require exact page and player', async () => {
  const yt = node('video',{currentSrc:'blob:https://www.youtube.com/a',match:s => s.split(',').some(part => part.trim() === '#movie_player')});
  const p = await page({url:'https://www.youtube.com/watch?v=abc',roots:[yt]});
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,'https://www.youtube.com/watch?v=abc');
  assert.equal(p.messages.at(-1).sourceType,'page');
  assert.equal(p.messages.at(-1).expectedMediaId,'abc');
  yt.attributes['data-hls'] = 'https://cdn.test/master.m3u8';
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,'https://cdn.test/master.m3u8');
  assert.equal(p.messages.at(-1).sourceType,undefined);
  const feed = await page({url:'https://www.youtube.com/feed/subscriptions',roots:[node('video',{currentSrc:'blob:https://www.youtube.com/x',match:() => true})]});
  await feed.click(feed.buttons[0]);
  assert.equal(feed.messages.at(-1).url,'');
  const playlist = await page({url:'https://www.youtube.com/watch?v=abc&list=xyz',roots:[node('video',{currentSrc:'blob:https://www.youtube.com/x',match:() => true})]});
  await playlist.click(playlist.buttons[0]);
  assert.equal(playlist.messages.at(-1).url,'');
  const bv = await page({url:'https://www.bilibili.com/video/BV1abc?p=2',roots:[node('video',{currentSrc:'blob:https://www.bilibili.com/x',match:s => s.split(',').some(part => part.trim() === '.bpx-player-container')})]});
  await bv.click(bv.buttons[0]);
  assert.equal(bv.messages.at(-1).sourceType,'page');
  assert.equal(bv.messages.at(-1).expectedMediaId,'BV1abc');
  assert.equal(bv.messages.at(-1).url,'https://www.bilibili.com/video/BV1abc?p=2');
  const av = await page({url:'https://www.bilibili.com/video/av123',roots:[node('video',{currentSrc:'blob:https://www.bilibili.com/x',match:s => s.split(',').some(part => part.trim() === '.bpx-player-container')})]});
  await av.click(av.buttons[0]);
  assert.equal(av.messages.at(-1).expectedMediaId,'av123');
  const mobile = await page({url:'https://m.youtube.com/watch?v=mobile',roots:[node('video',{currentSrc:'blob:https://m.youtube.com/x',match:s => s.split(',').some(part => part.trim() === 'ytd-player')})]});
  await mobile.click(mobile.buttons[0]);
  assert.equal(mobile.messages.at(-1).expectedMediaId,'mobile');
  const extraPath = await page({url:'https://www.bilibili.com/video/BV1abc/another',roots:[node('video',{currentSrc:'blob:https://www.bilibili.com/x',match:() => true})]});
  await extraPath.click(extraPath.buttons[0]);
  assert.equal(extraPath.messages.at(-1).url,'');
  const unrelated = await page({url:'https://www.bilibili.com/video/av123',roots:[node('video',{currentSrc:'blob:https://www.bilibili.com/x'})]});
  await unrelated.click(unrelated.buttons[0]);
  assert.equal(unrelated.messages.at(-1).url,'');
});

test('ad state keeps an opaque YouTube or Bilibili blob out of page mode', async () => {
  const yt = node('video',{currentSrc:'blob:https://www.youtube.com/ad',
    match:s => s.split(',').some(part => part.trim() === '#movie_player'),
    classList:{contains:name => name === 'ad-showing'}});
  const youtube = await page({url:'https://www.youtube.com/watch?v=main',roots:[yt]});
  await youtube.click(youtube.buttons[0]);
  assert.equal(youtube.messages.at(-1).url,'');
  assert.equal(youtube.messages.at(-1).sourceType,undefined);
  const bili = node('video',{currentSrc:'blob:https://www.bilibili.com/ad',
    match:s => s.split(',').some(part => part.trim() === '.bpx-player-container'),
    classList:{contains:name => name === 'bpx-player-ad-active'}});
  const bilibili = await page({url:'https://www.bilibili.com/video/BV1main?p=2',roots:[bili]});
  await bilibili.click(bilibili.buttons[0]);
  assert.equal(bilibili.messages.at(-1).url,'');
  assert.equal(bilibili.messages.at(-1).sourceType,undefined);
});

test('after 10000 initial elements a ten-node mutation reads only the added subtree', async () => {
  const root = node('div');
  for (let i=0;i<10000;i++) root.append(node('div'));
  const p = await page({roots:[root]});
  assert.equal(p.styleReads(),10002);
  const small = node('div');
  for (let i=0;i<9;i++) small.append(node('div'));
  root.append(small);
  const before = p.styleReads();
  p.mutate([{type:'childList',target:root,addedNodes:[small],removedNodes:[]}]);
  await p.flush();
  assert.equal(p.styleReads()-before,10);
});

test('stylesheet change deliberately rescans backgrounds; load and srcset changes stay local', async () => {
  const img = node('img',{srcset:'https://cdn.test/1.jpg 1x, https://cdn.test/2.jpg 2x'});
  const p = await page({roots:[img]});
  const item = p.messages.find(m => m.type === 'ADD_DOM').items[0];
  assert.equal(item.url,'https://cdn.test/1.jpg');
  assert.equal(item.variants.length,2);
  const before = p.styleReads();
  p.listeners.load({target:img}); await p.flush();
  assert.equal(p.styleReads()-before,1);
  const style = node('style');
  p.mutate([{type:'childList',target:style,addedNodes:[],removedNodes:[]}]);
  await p.flush();
  assert.equal(p.styleReads()-before,3);
});

test('open shadow root is scanned and observed; removed player prompt is cleaned', async () => {
  const video = node('video',{currentSrc:'https://cdn.test/shadow.mp4'});
  const shadow = {mode:'open',children:[video]};
  const host = node('div',{shadowRoot:shadow});
  const p = await page({roots:[host]});
  assert.ok(p.observed.includes(shadow));
  assert.equal(p.buttons[0].style.display,'block');
  video.isConnected = false;
  p.mutate([{type:'childList',target:shadow,addedNodes:[],removedNodes:[video]}]);
  await p.flush();
  assert.equal(p.buttons[0].removed,true);
});

test('intersection updates a visible player prompt without rescanning styles', async () => {
  const video = node('video',{currentSrc:'https://cdn.test/v.mp4'});
  const p = await page({roots:[video],intersection:true});
  const reads = p.styleReads();
  assert.equal(p.buttons[0].style.display,'block');
  p.intersect(video,false); await p.flush();
  assert.equal(p.buttons[0].style.display,'none');
  p.intersect(video,true); await p.flush();
  assert.equal(p.buttons[0].style.display,'block');
  assert.equal(p.styleReads(),reads);
});

test('image keeps its imageId as currentSrc and srcset variants change', async () => {
  const img = node('img',{currentSrc:'https://cdn.test/1.jpg',srcset:'https://cdn.test/1.jpg 1x'});
  const p = await page({roots:[img]});
  const first = p.messages.find(m => m.type === 'ADD_DOM').items[0];
  img.currentSrc = 'https://cdn.test/2.jpg';
  img.srcset = 'https://cdn.test/1.jpg 1x, https://cdn.test/2.jpg 2x';
  p.mutate([{type:'attributes',target:img,attributeName:'srcset'}]); await p.flush();
  const latest = p.messages.at(-1).items[0];
  assert.equal(latest.imageId,first.imageId);
  assert.equal(latest.url,img.currentSrc);
  assert.equal(latest.variants.length,2);
});

test('SEARCH_START accepts only same-window candidate messages for 60 seconds and never opens PANEL', async () => {
  const p = await page({roots:[node('video',{currentSrc:'blob:https://example.test/v'})]});
  const playerId = p.messages.find(m => m.type === 'ADD_DOM')?.items[0]?.playerId || 'frame-player-1';
  const candidate = {type:'GOPEED_MEDIA_CANDIDATE',url:'https://cdn.test/master.m3u8',mime:'application/vnd.apple.mpegurl',playerId:'forged',searchId:'s1'};
  p.listeners.message({source:p.window,data:candidate});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,0);
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'forged',searchId:'s1'});
  p.listeners.message({source:p.window,data:candidate});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,0);
  p.listeners.message({source:{},data:candidate});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,0);
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId,searchId:'s1'});
  p.listeners.message({source:{},data:candidate});
  p.listeners.message({source:p.window,data:candidate});
  p.listeners.message({source:p.window,data:candidate});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,1);
  assert.equal(p.messages.at(-1).searchId,'s1');
  assert.equal(p.messages.at(-1).items[0].playerId,undefined);
  assert.equal(p.messages.filter(m => m.type === 'PANEL').length,0);
  p.listeners.message({source:p.window,data:{...candidate,url:'https://cdn.test/key',mime:'video/mp4'}});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,1);
  p.expireSearch();
  p.listeners.message({source:p.window,data:{...candidate,url:'https://cdn.test/other.m3u8'}});
  assert.equal(p.messages.filter(m => m.type === 'ADD_DOM').length,1);
});

test('SEARCH_START bridge caps candidates at 64 and never assigns a network URL to playerId', async () => {
  const p = await page({roots:[node('video',{currentSrc:'blob:https://example.test/v'})]});
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'frame-player-1',searchId:'s1'});
  for (let i=0;i<65;i++) p.listeners.message({source:p.window,data:{type:'GOPEED_MEDIA_CANDIDATE',url:`https://cdn.test/${i}.mp4`,mime:'video/mp4',playerId:'frame-player-1',searchId:'s1'}});
  const items = p.messages.filter(m => m.type === 'ADD_DOM').flatMap(m => m.items);
  assert.equal(items.length,64);
  assert.ok(items.every(item => !item.playerId));
});

test('frame sourcePage stays the actual frame URL in HTTP, about:blank and srcdoc frames', async () => {
  for (const frameUrl of ['https://embed.test/player','about:blank','about:srcdoc']) {
    const p = await page({url:frameUrl,baseURI:'https://embed.test/base/',roots:[node('video',{currentSrc:'https://cdn.test/v.mp4'})]});
    await p.click(p.buttons[0]);
    assert.equal(p.messages.at(-1).sourcePage,frameUrl);
    assert.equal(p.messages.find(m => m.type === 'ADD_DOM').sourcePage,frameUrl);
  }
  const p = await page({url:'about:srcdoc',baseURI:'https://embed.test/base/',roots:[node('img',{srcset:'image.jpg 1x'})]});
  assert.equal(p.messages.find(m => m.type === 'ADD_DOM').items[0].url,'https://embed.test/base/image.jpg');
  assert.equal(p.messages.find(m => m.type === 'ADD_DOM').sourcePage,'about:srcdoc');
});

test('bridge forwards at most 16 distinct HTTP(S) relatedUrls on the manifest item', async () => {
  const p = await page({roots:[node('video',{currentSrc:'blob:https://example.test/v'})]});
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'frame-player-1',searchId:'s1'});
  const urls = ['https://cdn.test/audio.m3u8','/relative.m3u8','https://cdn.test/key.key','https://cdn.test/audio.m3u8',
    ...Array.from({length:20},(_,i) => `https://cdn.test/v${i}.m3u8`)];
  p.listeners.message({source:p.window,data:{type:'GOPEED_MEDIA_CANDIDATE',url:'https://cdn.test/master.m3u8',mime:'application/vnd.apple.mpegurl',relatedUrls:urls,searchId:'s1'}});
  const item = p.messages.find(m => m.type === 'ADD_DOM').items[0];
  assert.equal(item.relatedUrls.length,13);
  assert.equal(item.relatedUrls[0],'https://cdn.test/audio.m3u8');
  assert.ok(item.relatedUrls.every(url => url.startsWith('https://') && !url.includes('key.key')));
  assert.equal(item.playerId,undefined);
});

test('CSS URLs preserve parentheses and image-set alternatives; measurable pseudo backgrounds are bounded',async () => {
  const image = node('div',{backgroundImage:'url("image(1).jpg"), image-set("a.jpg" 1x type("image/avif"), "b.jpg" 2x)',
    clientWidth:100,clientHeight:100,pseudoBackground:{'::before':'url("pseudo(2).png")'}});
  const p = await page({roots:[image]});
  const urls = p.messages.filter(m => m.type === 'ADD_DOM').flatMap(m => m.items.map(i => i.url));
  assert.deepEqual(urls.sort(),['https://example.test/a.jpg','https://example.test/b.jpg','https://example.test/image(1).jpg','https://example.test/pseudo(2).png'].sort());
  assert.ok(p.styleReads() <= 15);
});

test('srcset keeps URL commas, currentSrc wins, and picture source is an image',async () => {
  const img = node('img',{currentSrc:'https://cdn.test/chosen.jpg',srcset:'https://cdn.test/image,large.jpg 1x, https://cdn.test/other.jpg 2x'});
  const picture = node('picture'); const source = node('source',{srcset:'https://cdn.test/a,b.jpg 1x, https://cdn.test/c.jpg 2x'});
  picture.append(source);
  const p = await page({roots:[img,picture]});
  const items = p.messages.filter(m => m.type === 'ADD_DOM').flatMap(m => m.items);
  assert.equal(items.find(i => i.imageId).url,img.currentSrc);
  assert.equal(items.find(i => i.imageId).variants[0],'https://cdn.test/image,large.jpg');
  assert.equal(items.find(i => i.url === 'https://cdn.test/a,b.jpg').kind,'image');
});

test('search emits only new DOM URLs with searchId and associates exact matching object URL',async () => {
  const video = node('video',{currentSrc:'blob:https://example.test/object'});
  const old = node('img',{src:'https://cdn.test/old.jpg'});
  const p = await page({roots:[video,old]});
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'frame-player-1',searchId:'fresh'});
  const fresh = node('img',{src:'https://cdn.test/new.jpg'}); p.root.append(fresh);
  p.mutate([{type:'childList',target:p.root,addedNodes:[fresh],removedNodes:[]}]); await p.flush();
  assert.equal(p.messages.at(-1).searchId,'fresh');
  assert.equal(p.messages.at(-1).items[0].url,fresh.src);
  const another = node('video',{currentSrc:'https://cdn.test/new.mp4'}); p.root.append(another);
  p.mutate([{type:'childList',target:p.root,addedNodes:[another],removedNodes:[]}]); await p.flush();
  assert.equal(p.messages.at(-1).searchId,'fresh');
  assert.equal(p.messages.at(-1).items[0].playerId,undefined);
  p.listeners.message({source:p.window,data:{type:'GOPEED_MEDIA_CANDIDATE',url:'https://cdn.test/opaque',manifest:true,protocolHint:'manifest',searchId:'fresh',mediaObjectUrl:video.currentSrc,contextUrls:['https://cdn.test/key.key']}});
  const exact = p.messages.at(-1).items[0];
  assert.equal(exact.playerId,'frame-player-1'); assert.equal(exact.exactAssociation,true);
  assert.equal(exact.contextUrls[0],'https://cdn.test/key.key');
  assert.deepEqual(Array.from(exact.relatedUrls),[]);
  p.listeners.message({source:p.window,data:{type:'GOPEED_MEDIA_CANDIDATE',url:'https://cdn.test/other.m3u8',searchId:'wrong',mediaObjectUrl:video.currentSrc}});
  assert.equal(p.messages.at(-1).items[0],exact);
});

test('stylesheet rescan does not label preexisting page items as search discoveries',async () => {
  const img = node('img',{src:'https://cdn.test/old.jpg'});
  const p = await page({roots:[node('video',{currentSrc:'blob:https://example.test/v'}),img]});
  p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'frame-player-1',searchId:'fresh'});
  const before = p.messages.length;
  const style = node('style'); p.mutate([{type:'childList',target:style,addedNodes:[],removedNodes:[]}]);
  await p.flush();
  assert.ok(p.messages.slice(before).every(message => message.searchId === undefined));
});

test('X feed players target their own status and quote or ambiguous multi-video cards require confirmation',async () => {
  const link = node('a',{href:'https://x.com/example/status/12345',querySelector:selector => selector === 'time' ? {} : null});
  const article = node('article');
  const video = node('video',{currentSrc:'blob:https://x.com/video',closest:selector => selector === '[data-testid="tweet"]' ? article : null});
  article.querySelectorAll = selector => selector === 'video' ? [video] : selector.startsWith('a[') ? [link] : [];
  const p = await page({url:'https://x.com/home',roots:[video]});
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,'https://x.com/example/status/12345');
  assert.equal(p.messages.at(-1).expectedMediaId,'12345');
  assert.equal(p.messages.at(-1).sourceType,'page');
  article.querySelectorAll = selector => selector === 'video' ? [video,video] : selector.startsWith('a[') ? [link] : [];
  await p.click(p.buttons[0]);assert.equal(p.messages.at(-1).url,'');
  article.querySelectorAll = selector => selector === 'video' ? [video] : selector.startsWith('a[') ? [link] : [];
  video.closest = selector => selector === '[data-testid="tweet"]' ? article : selector.includes('quoteTweet') ? {} : null;
  await p.click(p.buttons[0]);assert.equal(p.messages.at(-1).url,'');
});

test('a previously discovered manifest can upgrade to exact MSE player proof without adding an unrelated choice',async()=>{
  const video=node('video',{currentSrc:'blob:https://example.test/exact'});
  const p=await page({roots:[video]});p.listeners.runtimeMessage({type:'SEARCH_START',playerId:'frame-player-1',searchId:'upgrade'});
  const value={type:'GOPEED_MEDIA_CANDIDATE',url:'https://cdn.test/master.m3u8',mime:'application/vnd.apple.mpegurl',searchId:'upgrade'};
  p.listeners.message({source:p.window,data:value});
  p.listeners.message({source:p.window,data:{...value,mediaObjectUrl:video.currentSrc}});
  const adds=p.messages.filter(m=>m.type==='ADD_DOM');assert.equal(adds.length,2);assert.equal(adds[0].items[0].playerId,undefined);assert.equal(adds[1].items[0].playerId,'frame-player-1');
  p.listeners.message({source:p.window,data:{...value,mediaObjectUrl:'blob:https://example.test/other'}});assert.equal(p.messages.filter(m=>m.type==='ADD_DOM').length,2);
});

test('X video widget receives one prompt before a video element is mounted',async () => {
  const link = node('a',{href:'https://x.com/example/status/12345',querySelector:s => s === 'time' ? {} : null});
  const article = node('article',{querySelectorAll:s => s.startsWith('a[') ? [link] : []});
  const widget = node('div',{attributes:{'data-testid':'videoPlayer'},closest:s => s === '[data-testid="tweet"]' ? article : s === '[data-testid="videoPlayer"]' ? widget : null});
  const p = await page({url:'https://x.com/home',roots:[widget]});
  assert.equal(p.buttons.length,1);
  assert.equal(p.buttons[0].textContent,'用 kownload 下载');
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,link.href);
  const video = node('video',{currentSrc:'blob:https://x.com/video',closest:s => s === '[data-testid="videoPlayer"]' ? widget : s === '[data-testid="tweet"]' ? article : null});
  widget.append(video);
  widget.querySelector = s => s === 'video' ? video : null;
  p.mutate([{type:'childList',target:widget,addedNodes:[video],removedNodes:[]}]);await p.flush();
  assert.equal(p.buttons.length,1);
});

test('logged-in X nested videoPlayer and videoComponent are one player bound to its own post',async () => {
  const build = id => {
    const link = node('a',{href:`https://x.com/example/status/${id}`,querySelector:s => s === 'time' ? {} : null});
    const article = node('article',{attributes:{'data-testid':'tweet'}});
    const outer = node('div',{attributes:{'data-testid':'videoPlayer'}});
    const inner = node('div',{attributes:{'data-testid':'videoComponent'}});
    const video = node('video',{currentSrc:`blob:https://x.com/${id}`});
    for (const el of [outer,inner,video]) el.closest = s => s === '[data-testid="tweet"]' ? article :
      s === '[data-testid="videoPlayer"]' ? outer : s === '[data-testid="videoComponent"]' && el !== outer ? inner : null;
    for (const el of [outer,inner]) { el.querySelector = s => s === 'video' ? video : null; el.querySelectorAll = s => s === 'video' ? [video] : []; }
    article.querySelectorAll = s => s === 'video' ? [video] : s.startsWith('a[') ? [link] : [outer,inner];
    outer.append(inner); inner.append(video); article.append(outer);
    return {article,video};
  };
  const first = build('12345'), second = build('67890');
  const p = await page({url:'https://x.com/home',roots:[first.article,second.article]});
  assert.equal(p.buttons.length,2,'one button per video, not one per nested wrapper');
  for (const [index,id] of ['12345','67890'].entries()) {
    await p.click(p.buttons[index]);
    assert.equal(p.messages.at(-1).url,`https://x.com/example/status/${id}`);
    assert.equal(p.messages.at(-1).sourceType,'page');
    assert.equal(p.messages.at(-1).expectedMediaId,id);
  }
  const identity = p.messages.at(-1).playerId;
  second.video.currentSrc = 'https://video.twimg.com/second.mp4?sig=keep';
  await p.click(p.buttons[1]);
  assert.equal(p.messages.at(-1).url,second.video.currentSrc);
  assert.equal(p.messages.at(-1).playerId,identity);
});

test('X shared video/1 timestamp never assigns the first video to a second player',async () => {
  const article = node('article');
  const shared = node('a',{href:'https://x.com/example/status/12345/video/1',querySelector:s => s === 'time' ? {} : null});
  const videos = [1,2].map(i => node('video',{currentSrc:`blob:https://x.com/${i}`,closest:s => s === '[data-testid="tweet"]' ? article : null}));
  article.querySelectorAll = s => s === 'video' ? videos : s.startsWith('a[') ? [shared] : [];
  const p = await page({url:'https://x.com/home',roots:videos});
  for (const button of p.buttons) {
    await p.click(button);
    assert.equal(p.messages.at(-1).url,'');
  }
  for (const [i,video] of videos.entries()) {
    const own = node('a',{href:`https://x.com/example/status/12345/video/${i+1}`,querySelectorAll:s => s === 'video' ? [video] : []});
    video.closest = s => s === '[data-testid="tweet"]' ? article : s === 'a[href*="/status/"]' ? own : null;
    await p.click(p.buttons[i]);
    assert.equal(p.messages.at(-1).url,own.href);
  }
});

test('X mounting an outer player before its video preserves the existing button and identity',async () => {
  let outer;
  const article = node('article');
  const link = node('a',{href:'https://x.com/example/status/12345',querySelector:s => s === 'time' ? {} : null});
  const inner = node('div',{attributes:{'data-testid':'videoComponent'},closest:s => s === '[data-testid="tweet"]' ? article : s === '[data-testid="videoPlayer"]' ? outer || null : s === '[data-testid="videoComponent"]' ? inner : null});
  article.querySelectorAll = s => s === 'video' ? [] : s.startsWith('a[') ? [link] : outer ? [outer,inner] : [inner];
  const p = await page({url:'https://x.com/home',roots:[inner]});
  await p.click(p.buttons[0]); const id = p.messages.at(-1).playerId;
  outer = node('div',{attributes:{'data-testid':'videoPlayer'},closest:s => s === '[data-testid="tweet"]' ? article : s === '[data-testid="videoPlayer"]' ? outer : null});
  outer.append(inner); p.root.append(outer);
  p.mutate([{type:'childList',target:p.root,addedNodes:[outer],removedNodes:[]}]); await p.flush();
  assert.equal(p.buttons.length,1);
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).playerId,id);
  assert.equal(p.messages.at(-1).url,link.href);
});

test('standalone X post without a timestamp link maps only a sole tweet card',async () => {
  const article = node('div',{attributes:{'data-testid':'tweet'},querySelectorAll:() => []});
  const video = node('video',{currentSrc:'blob:https://x.com/a',closest:s => s === '[data-testid="tweet"]' ? article : null});
  const p = await page({url:'https://x.com/example/status/12345',roots:[video],selectors:{'[data-testid="tweet"]':[article]}});
  await p.click(p.buttons[0]);assert.equal(p.messages.at(-1).url,'https://x.com/example/status/12345');
  const ambiguous = await page({url:'https://x.com/example/status/12345',roots:[video],selectors:{'[data-testid="tweet"]':[article,node('div')]}});
  await ambiguous.click(ambiguous.buttons[0]);assert.equal(ambiguous.messages.at(-1).url,'');
});

function xSSR({src='',links=[],extraVideos=[],markers={},parentTag='div'} = {}) {
  const video = node('video',{currentSrc:src,getBoundingClientRect:() => ({width:0,height:0,top:0,bottom:0,left:0,right:0})});
  const poster = node(parentTag,{
    // A layout container must never become a source candidate.
    src:'https://cdn.test/unrelated.mp4',
    getBoundingClientRect:() => ({width:564,height:318,top:100,bottom:418,left:50,right:614}),
    querySelectorAll:s => s === 'video' ? [video] : [],
    querySelector:s => s === 'video' ? video : null});
  poster.append(video);
  return {video,poster,options:{url:'https://x.com/example/status/12345',roots:[poster,...extraVideos],
    selectors:{video:[video,...extraVideos],'a[href*="/status/"]':links,...markers}}};
}

test('X SSR without test IDs maps a sole empty-src video to the exact post and uses the visible poster anchor',async () => {
  const fixture = xSSR();
  const p = await page({...fixture.options,intersection:true});
  assert.equal(p.buttons.length,1);
  const button = p.buttons[0];
  assert.equal(button.style.display,'block');
  assert.equal(button.style.top,'110px');
  assert.equal(button.style.left,'469px');
  assert.equal(button.textContent,'用 kownload 下载');
  await p.click(button);
  const first = p.messages.at(-1);
  assert.equal(first.url,'https://x.com/example/status/12345');
  assert.equal(first.sourceType,'page');
  assert.equal(first.expectedMediaId,'12345');
  p.intersect(fixture.poster,false); await p.flush();
  assert.equal(button.style.display,'none');
  p.intersect(fixture.poster,true); await p.flush();
  assert.equal(button.style.display,'block');
  fixture.video.currentSrc = 'https://cdn.test/exact.mp4';
  p.mutate([{type:'attributes',target:fixture.video,attributeName:'src'}]); await p.flush();
  await p.click(button);
  assert.equal(p.messages.at(-1).url,fixture.video.currentSrc);
  assert.equal(p.messages.at(-1).playerId,first.playerId);
  assert.equal(p.buttons.length,1);
  fixture.video.isConnected = false;
  p.mutate([{type:'childList',target:fixture.poster,addedNodes:[],removedNodes:[fixture.video]}]); await p.flush();
  assert.equal(button.removed,true);
});

test('X SSR blob source and explicit video path map without retaining tracking parameters',async () => {
  const f = xSSR({src:'blob:https://x.com/opaque'});
  const p = await page({...f.options,url:'https://twitter.com/example/status/12345/video/2?s=20#video'});
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).url,'https://x.com/example/status/12345/video/2');
  assert.equal(p.messages.at(-1).expectedMediaId,'12345');
});

test('X SSR feed, non-post paths and multiple videos remain explicit selection, including empty src',async () => {
  for (const url of ['https://x.com/home','https://x.com/example','https://x.com/example/status/12345/likes']) {
    const f = xSSR(); const p = await page({...f.options,url});
    assert.equal(p.buttons[0].textContent,'识别这个视频');
    await p.click(p.buttons[0]);
    assert.equal(p.messages.at(-1).url,'');
    assert.equal(p.messages.at(-1).sourceType,undefined);
  }
  const f = xSSR({extraVideos:[node('video')]});
  const p = await page({...f.options,url:'https://x.com/example/status/12345/video/1'});
  for (const button of p.buttons) {
    await p.click(button);
    assert.equal(p.messages.at(-1).url,'');
    assert.equal(p.messages.at(-1).sourceType,undefined);
  }
});

test('X SSR refuses quote, photo, conflicting post and multiple media slot evidence',async () => {
  for (const options of [
    {links:[node('a',{href:'https://x.com/quoted/status/67890'})]},
    {links:[node('a',{href:'https://x.com/example/status/12345/photo/1'})]},
    {links:[node('a',{href:'https://x.com/example/status/12345/video/2'})]},
    {links:[1,2].map(n => node('a',{href:`https://x.com/example/status/12345/video/${n}`}))},
    {markers:{'[data-testid="tweet"]':[node('article')]}},
    {markers:{'[data-testid="quoteTweet"], [data-testid="quotedTweet"], [data-testid="tweetPhoto"]':[node('div')]}}
  ]) {
    const f = xSSR(options); const p = await page(f.options);
    await p.click(p.buttons[0]);
    assert.equal(p.messages.at(-1).url,'');
    assert.equal(p.messages.at(-1).sourceType,undefined);
  }
});

test('X SSR with another status link suggests only the current post for manual confirmation',async () => {
  const f = xSSR({links:[node('a',{href:'https://x.com/other/status/67890'})]});
  const p = await page(f.options);
  assert.equal(p.buttons[0].textContent,'识别这个视频');
  await p.click(p.buttons[0]);
  const message = p.messages.at(-1);
  assert.equal(message.url,'');
  assert.equal(message.sourceType,undefined);
  assert.equal(message.suggestedPage,'https://x.com/example/status/12345');
  assert.equal(message.expectedMediaId,'12345');
  const multi = xSSR({links:[node('a',{href:'https://x.com/example/status/12345/video/1'}),node('a',{href:'https://x.com/example/status/12345/video/2'})]});
  const m = await page(multi.options);
  await m.click(m.buttons[0]);
  assert.equal(m.messages.at(-1).suggestedPage,undefined);
  const quote = xSSR({markers:{'[data-testid="tweet"], [data-testid="quoteTweet"], [data-testid="quotedTweet"], [data-testid="tweetPhoto"]':[node('div')]}});
  const q = await page(quote.options);
  await q.click(q.buttons[0]);
  assert.equal(q.messages.at(-1).suggestedPage,undefined);
  const otherMedia = xSSR({links:[node('a',{href:'https://x.com/other/status/67890/video/1'})]});
  const o = await page(otherMedia.options);
  await o.click(o.buttons[0]);
  assert.equal(o.messages.at(-1).suggestedPage,undefined);
});

test('X layout fallback never anchors body, html or a container shared by videos',async () => {
  for (const parentTag of ['body','html','main','article']) {
    const f = xSSR({parentTag}); const p = await page(f.options);
    assert.equal(p.buttons.length,1);
    assert.equal(p.buttons[0].style.display,'none');
  }
  const f = xSSR();
  f.poster.querySelectorAll = s => s === 'video' ? [f.video,node('video')] : [];
  const p = await page(f.options);
  assert.equal(p.buttons[0].style.display,'none');
});

test('X SSR hydration into a named widget preserves one button, player id and exact video source',async () => {
  const f = xSSR();
  f.poster.closest = selector => selector === '[data-testid="videoPlayer"]' && f.poster.attributes['data-testid'] ? f.poster : null;
  f.video.closest = selector => f.poster.closest(selector);
  const p = await page(f.options);
  await p.click(p.buttons[0]);
  const id = p.messages.at(-1).playerId;
  f.poster.attributes['data-testid'] = 'videoPlayer';
  p.mutate([{type:'attributes',target:f.poster,attributeName:'class'}]); await p.flush();
  f.video.currentSrc = 'https://cdn.test/hydrated.mp4';
  p.mutate([{type:'attributes',target:f.video,attributeName:'src'}]); await p.flush();
  assert.equal(p.buttons.length,1);
  await p.click(p.buttons[0]);
  assert.equal(p.messages.at(-1).playerId,id);
  assert.equal(p.messages.at(-1).url,f.video.currentSrc);
});
