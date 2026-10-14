import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';
import * as lib from '../lib.js';

function background(extra = {},frame = null) {
  let handler;
  const state = {items:{a:{id:'a',tabId:1,frameId:0,url:'https://cdn.test/a.mp4'},b:{id:'b',tabId:1,frameId:7,url:'https://cdn.test/b.mp4'}},submitted:{},routes:{},topCommit:{}};
  Object.assign(state.items,extra);
  const event = {addListener(){}};
  const opened = [];
  const chrome = {storage:{session:{get:async () => ({mediaStateV1:state}),set:async data => Object.assign(state,data.mediaStateV1)},local:{get:async () => ({})}},
    action:{setBadgeText:async () => {},setBadgeBackgroundColor:async () => {}},
    tabs:{get:async () => ({id:1,url:'https://example.test/watch'}),onRemoved:event,create:async tab => opened.push(tab)},
    webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(k => [k,event])),
    webNavigation:{onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event,getFrame:async () => frame},
    runtime:{onMessage:{addListener:fn => {handler = fn;}},sendMessage:async () => {},getURL:path => `chrome-extension://test/${path}`}};
  vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},chrome,URL,URLSearchParams});
  return {opened,request:(msg,sender = {}) => new Promise(resolve => handler(msg,sender,resolve))};
}

test('single-target LIST returns only the clicked resource; expired targets never expand to the page',async () => {
  const bg = background();
  assert.deepEqual((await bg.request({type:'LIST',tabId:1,target:'a'})).data.map(x => x.id).join(','),'a');
  assert.equal((await bg.request({type:'LIST',tabId:1,target:'gone'})).data.length,0);
  assert.equal((await bg.request({type:'LIST',tabId:1})).data.length,2);
});
test('a player click pins a candidate from its own document and frame even before background scanning completes',async () => {
  const bg = background();
  const reply = await bg.request({type:'PANEL',url:'https://cdn.test/a.mp4',sourcePage:'https://example.test/frame',timeStamp:Date.now()},
    {tab:{id:1,title:'test'},frameId:7,documentId:'doc-seven',url:'https://example.test/frame'});
  assert.equal(reply.ok,true);
  const url = new URL(bg.opened[0].url);
  assert.equal(url.searchParams.get('mode'),'single');
  const target = url.searchParams.get('target');
  assert.ok(target);
  const candidates = (await bg.request({type:'LIST',tabId:1,target})).data;
  assert.equal(candidates.length,1);
  assert.equal(candidates[0].frameId,7);
  assert.equal(candidates[0].documentId,'doc-seven');
});
test('unresolved players do not open the whole page as a download fallback',async () => {
  const bg = background();
  const reply = await bg.request({type:'PANEL',url:''},{tab:{id:1},frameId:0});
  assert.equal(reply.ok,false);
  assert.equal(bg.opened.length,0);
});

test('unresolved X feed player never inherits other players unbound network videos',async () => {
  const source = 'https://x.com/home';
  const bg = background({
    feedA:{id:'feedA',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',url:'https://video.twimg.com/a/master.m3u8',manifest:true},
    feedB:{id:'feedB',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',url:'https://video.twimg.com/b/master.m3u8',manifest:true}
  });
  const reply = await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId:'clicked-player'});
  assert.equal(reply.ok,true);
  assert.equal(reply.data.length,0,'unrelated network captures must stay in the toolbar panel');
  const wrong = await bg.request({type:'CREATE',tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId:'clicked-player',target:'feedB',id:'feedB'});
  assert.equal(wrong.ok,false,'the single-player boundary must also apply on submission');
});

test('identify submission rejects an unbound video even with a matching ID and target',async () => {
  const source = 'https://x.com/home';
  const bg = background({unbound:{id:'unbound',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',url:'https://video.twimg.com/other.mp4'}});
  for (const type of ['INSPECT','CREATE','UPDATE','RECOVERABLE']) {
    const reply = await bg.request({type,tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId:'clicked',id:'unbound',target:'unbound'});
    assert.equal(reply.ok,false,`${type} must reject an unbound resource`);
    assert.match(reply.error,/资源已失效/);
  }
});

test('identify lists only the clicked player binding in a shared feed document',async () => {
  const source = 'https://x.com/home';
  const bg = background({
    first:{id:'first',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',playerId:'player-a',url:'https://video.twimg.com/a.mp4'},
    second:{id:'second',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',playerId:'player-b',url:'https://video.twimg.com/b.mp4'}
  });
  for (const [playerId,expected] of [['player-a','first'],['player-b','second']]) {
    const reply = await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId});
    assert.deepEqual(Array.from(reply.data,x => x.id),[expected]);
  }
  const wrong = await bg.request({type:'CREATE',tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId:'player-a',id:'second',target:'second'});
  assert.equal(wrong.ok,false);
});

test('identify filters before grouping so an unbound parent cannot hide the exact player',async () => {
  const source = 'https://x.com/home';
  const boundUrl = 'https://video.twimg.com/clicked.mp4';
  const bg = background({
    unbound:{id:'unbound',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',url:'https://video.twimg.com/playlist.m3u8',relatedUrls:[boundUrl]},
    bound:{id:'bound',tabId:1,frameId:0,documentId:'feed-doc',sourcePage:source,kind:'video',playerId:'clicked',url:boundUrl}
  });
  const reply = await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'feed-doc',sourcePage:source,playerId:'clicked'});
  assert.deepEqual(Array.from(reply.data,x => x.id),['bound']);
});

class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.children = []; this.value = ''; this.checked = false; this.listeners = {}; this.classList = {toggle(){},add(){}}; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  addEventListener(event,fn) { this.listeners[event] = fn; }
}
async function popup(target = 'a',mode = 'single') {
  const elements = new Map();
  const get = id => { if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); };
  const requests = [];
  const candidates = [{id:'a',kind:'video',url:'https://cdn.test/a.mp4'},{id:'b',kind:'image',url:'https://cdn.test/b.jpg'}];
  const document = {body:new Element(),getElementById:get,querySelectorAll:() => [],createElement:tag => new Element(tag)};
  const chrome = {tabs:{get:async () => ({id:1,url:'https://example.test/watch'})},storage:{local:{get:async () => ({})}},runtime:{onMessage:{addListener(){}},sendMessage:async msg => {
    requests.push(msg);
    const data = msg.type === 'LIST' ? candidates.filter(x => !msg.target || x.id === msg.target) : msg.type === 'CONFIG' ? {} : false;
    return {ok:true,data};
  }}};
  vm.runInNewContext(fs.readFileSync(new URL('../popup.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,document,chrome,URL,URLSearchParams,location:{search:target === null ? '?tab=1' : `?tab=1&mode=${mode}&target=${target}&frame=0&doc=doc-1&source=https://example.test/watch&player=player-1`},Option:class extends Element {constructor(text,value){super('option');this.textContent = text;this.value = value;}}});
  await settle();
  return {get,requests};
}
test('player download UI shows and submits one resource without selecting from the entire page',async () => {
  const {get,requests} = await popup();
  assert.equal(get('list').children.length,1);
  assert.equal(get('download').textContent,'下载这个视频');
  await get('download').listeners.click();
  await settle();
  assert.deepEqual(requests.filter(x => x.type === 'CREATE').map(x => x.id),['a']);
});
test('an expired single target disables download instead of offering unrelated resources',async () => {
  const {get,requests} = await popup('expired');
  assert.equal(get('download').disabled,true);
  await get('download').listeners.click();
  await settle();
  assert.equal(requests.filter(x => x.type === 'CREATE').length,0);
  assert.match(get('list').children[0].textContent,/失效|重新/);
});

test('a MIME-only manifest keeps media inspection when clicked in its own player',async () => {
  const bg = background({opaque:{id:'opaque',tabId:1,frameId:0,documentId:'doc',sourcePage:'https://example.test/watch',url:'https://cdn.test/opaque?sig=keep',mime:'application/vnd.apple.mpegurl',kind:'video',manifest:true,header:{Referer:'https://example.test/watch'}}});
  await bg.request({type:'PANEL',url:'https://cdn.test/opaque?sig=keep',sourcePage:'https://example.test/watch'},
    {tab:{id:1},frameId:0,documentId:'doc'});
  const target = new URL(bg.opened[0].url).searchParams.get('target');
  const item = (await bg.request({type:'LIST',tabId:1,target})).data[0];
  assert.equal(item.manifest,true);
  assert.equal(item.header.Referer,'https://example.test/watch');
});
test('a single target cannot submit a different page resource',async () => {
  const bg = background();
  assert.equal((await bg.request({type:'CREATE',tabId:1,target:'a',id:'b'})).ok,false);
});
test('toolbar batch download remains explicit and starts with nothing selected',async () => {
  const {get,requests} = await popup(null);
  assert.equal(get('list').children.length,2);
  assert.equal(get('download').disabled,true);
  get('selectAll').checked = true;
  get('selectAll').listeners.change();
  await get('download').listeners.click();
  await settle();
  assert.deepEqual(requests.filter(x => x.type === 'CREATE').map(x => x.id),['a','b']);
});

test('identify mode offers a single manual choice and never preselects a candidate',async () => {
  const {get,requests} = await popup('','identify');
  assert.equal(get('download').disabled,true);
  await get('download').listeners.click();
  await settle();
  assert.equal(requests.filter(x => x.type === 'CREATE').length,0);
  const first = get('list').children[0].children[0];
  assert.equal(first.type,'radio');
  first.checked = true; first.listeners.change();
  await get('download').listeners.click();
  await settle();
  assert.deepEqual(requests.filter(x => x.type === 'CREATE').map(x => x.id),['a']);
  assert.equal(requests.find(x => x.type === 'CREATE').target,'a');
});

test('X SSR suggestion becomes an unselected identify candidate for its exact frame',async () => {
  const source = 'https://x.com/CaptainAmerica/status/719944021058060289';
  const bg = background({}, {url:source,documentId:'x-doc'});
  const reply = await bg.request({type:'PANEL',url:'',suggestedPage:source,expectedMediaId:'719944021058060289',sourcePage:source,playerId:'video-1'},
    {tab:{id:1,title:'X'},frameId:0,documentId:'x-doc',url:source});
  assert.equal(reply.ok,true);
  const opened = new URL(bg.opened[0].url);
  assert.equal(opened.searchParams.get('mode'),'identify');
  assert.equal(opened.searchParams.has('target'),false);
  const items = (await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'x-doc',sourcePage:source,playerId:'video-1'})).data;
  assert.equal(items.length,1);
  assert.equal(items[0].sourceType,'page');
  assert.equal(items[0].manifest,true);
  assert.equal(items[0].title,'当前帖子视频（请确认）');
  assert.equal(items[0].playerId,'');
  assert.equal((await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'x-doc',sourcePage:source,playerId:'other'})).data.length,0);
});

test('X SSR suggestion rejects other hosts, mismatched posts, forged source and stale document',async () => {
  const source = 'https://x.com/CaptainAmerica/status/719944021058060289';
  for (const [frame,msg] of [
    [{url:'https://example.test/watch',documentId:'x-doc'},{sourcePage:'https://example.test/watch'}],
    [{url:source,documentId:'x-doc'},{suggestedPage:'https://example.test/CaptainAmerica/status/719944021058060289'}],
    [{url:source,documentId:'x-doc'},{suggestedPage:'https://x.com/CaptainAmerica/status/123'}],
    [{url:source,documentId:'old-doc'},{}],
    [{url:source,documentId:'x-doc'},{expectedMediaId:'123'}]
  ]) {
    const bg = background({},frame);
    const reply = await bg.request({type:'PANEL',url:'',suggestedPage:source,expectedMediaId:'719944021058060289',sourcePage:source,playerId:'video-1',...msg},
      {tab:{id:1,title:'X'},frameId:0,documentId:'x-doc',url:source});
    assert.equal(reply.ok,false);
    assert.equal(bg.opened.length,0);
  }
});

test('X page mode accepts only a status permalink from the same platform, preserving single-target scope',async () => {
  const bg = background();
  const reply = await bg.request({type:'PANEL',url:'https://x.com/example/status/12345',sourceType:'page',expectedMediaId:'12345',sourcePage:'https://x.com/home',timeStamp:Date.now()},
    {tab:{id:1,title:'X'},frameId:0,documentId:'x-doc',url:'https://x.com/home'});
  assert.equal(reply.ok,true);
  const target = new URL(bg.opened[0].url).searchParams.get('target');
  assert.equal((await bg.request({type:'LIST',tabId:1,target})).data.length,1);
  assert.equal((await bg.request({type:'PANEL',url:'https://x.com/example/status/54321',sourceType:'page',expectedMediaId:'12345',sourcePage:'https://x.com/home'},
    {tab:{id:1},frameId:0,documentId:'x-doc',url:'https://x.com/home'})).ok,false);
});
