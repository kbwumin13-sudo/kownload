import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import * as lib from '../lib.js';

function browser(seed = {}) {
  let handler;
  const memory = structuredClone(seed),counts = {reads:0,writes:0,bytes:0,tabs:0,notifications:0,native:0};
  const event = {addListener(){}};
  let nativeReply = async message => message.method === 'wakeup' ? {code:0,data:true} : {code:0,data:{code:0,data:'task-one'}};
  let cookiesQuery;
  const navigation = {};
  let local = {};

  const chrome = {storage:{session:{get:async () => {counts.reads++;return structuredClone(memory);},set:async data => {counts.writes++;counts.bytes += Buffer.byteLength(JSON.stringify(data));Object.assign(memory,structuredClone(data));},remove:async key => {delete memory[key];}},local:{get:async () => structuredClone(local),set:async data => {Object.assign(local,structuredClone(data));}}},
    tabs:{get:async () => {counts.tabs++;return {id:1,url:'https://example.test/watch'};},onRemoved:event,create:async () => {}},
    cookies:{getAllCookieStores:async () => [{id:'store-private',tabIds:[1]}],getPartitionKey:async () => ({partitionKey:{topLevelSite:'https://example.test',hasCrossSiteAncestor:true}}),getAll:async query => {cookiesQuery = query;return [];}},
    action:{setBadgeText:async () => {},setBadgeBackgroundColor:async () => {}},webNavigation:{getAllFrames:async () => [],onCommitted:{addListener:fn => {navigation.commit = fn;}},onHistoryStateUpdated:{addListener:fn => {navigation.spa = fn;}},onReferenceFragmentUpdated:event},
    webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(k => [k,event])),
    runtime:{onMessage:{addListener:fn => {handler = fn;}},sendMessage:async () => {counts.notifications++;},getURL:path => 'chrome-extension://test/'+path,sendNativeMessage:(host,msg,cb) => {counts.native++;nativeReply(msg).then(cb);}}};
  vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},chrome,URL,URLSearchParams});
  return {chrome,memory,counts,navigate:async data => {navigation.spa(data);await new Promise(resolve => setImmediate(resolve));},setNative:fn => {nativeReply = fn;},cookieQuery:() => cookiesQuery,request:(msg,sender = {}) => new Promise(resolve => handler(msg,sender,resolve))};
}
const sender = {tab:{id:1,title:'test'},frameId:0,documentId:'doc-1',url:'https://example.test/watch'};
async function add(bg,count = 1) {
  return bg.request({type:'ADD_DOM',timeStamp:Date.now(),sourcePage:sender.url,items:Array.from({length:count},(_,i) => ({url:`https://cdn.test/${i}.jpg`,kind:'image',width:320,height:180}))},sender);
}
test('200 candidates checkpoint once, restore after worker restart, and do not repeatedly query tabs',async () => {
  const bg = browser();
  assert.equal((await add(bg,200)).data.length,200);
  assert.equal(bg.counts.reads,1);
  assert.equal(bg.counts.writes,1);
  assert.equal(bg.counts.tabs,1);
  assert.equal(bg.counts.notifications,1);
  const restored = browser(bg.memory);
  assert.equal((await restored.request({type:'LIST',tabId:1})).data.length,200);
  assert.equal(restored.counts.reads,1);
});
test('tab checkpoint migration removes the old global snapshot and preserves other tabs',async () => {
  const bg = browser({mediaStateV1:{items:{a:{id:'a',tabId:2,frameId:0,url:'https://cdn.test/a.mp4'}},submitted:{},routes:{},topCommit:{}}});
  await add(bg);
  assert.equal(bg.memory.mediaStateV1,undefined);
  assert.ok(bg.memory['mediaTabV2:2']);
  assert.equal((await browser(bg.memory).request({type:'LIST',tabId:2})).data[0].id,'a');
});
test('slow task creation never blocks candidate discovery and concurrent submissions deduplicate',async () => {
  const bg = browser();const id = (await add(bg)).data[0];
  let release;
  bg.setNative(async message => message.method === 'wakeup' ? {code:0,data:true} : await new Promise(resolve => {release = () => resolve({code:0,data:{code:0,data:'task-one'}});}));
  const first = bg.request({type:'CREATE',tabId:1,id,options:{}}),second = bg.request({type:'CREATE',tabId:1,id,options:{}});
  while (!release) await new Promise(resolve => setImmediate(resolve));
  assert.equal((await add(bg,2)).data.length,2);
  release();
  assert.equal((await first).data.taskId,'task-one');
  assert.equal((await second).data.taskId,'task-one');
  assert.equal(bg.counts.native,2); // wakeup + one create
});
test('Cookie queries use the clicked tab store and frame partition',async () => {
  const bg = browser();const id = (await add(bg)).data[0];
  await bg.request({type:'CREATE',tabId:1,id,options:{}});
  assert.equal(bg.cookieQuery().storeId,'store-private');
  assert.equal(bg.cookieQuery().partitionKey?.topLevelSite,'https://example.test');
});
test('identify candidates require explicit player binding as well as exact document/frame',async () => {
  const items = Object.fromEntries([
    ['a',{id:'a',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,playerId:'player-1',kind:'video',url:'https://cdn.test/a.m3u8'}],
    ['b',{id:'b',tabId:1,frameId:7,documentId:'doc-2',sourcePage:sender.url,playerId:'player-1',kind:'video',url:'https://cdn.test/b.m3u8'}],
    ['c',{id:'c',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,kind:'image',url:'https://cdn.test/c.jpg'}],
    ['d',{id:'d',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,playerId:'different-player',kind:'video',url:'https://cdn.test/d.mp4'}],
    ['e',{id:'e',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,kind:'video',url:'https://cdn.test/unbound.m3u8'}]
  ]);
  const bg = browser({mediaTabV1:{},mediaStateV1:{items,submitted:{},routes:{},topCommit:{}}});
  const result = await bg.request({type:'LIST',tabId:1,identify:true,frameId:0,documentId:'doc-1',sourcePage:sender.url,playerId:'player-1'});
  assert.equal(result.data.map(x => x.id).join(','),'a');
});
test('manifest children are grouped only by explicit metadata, preserving another player',() => {
  const base = {tabId:1,frameId:0,documentId:'d',sourcePage:'https://a.test/page'};
  const items = [{...base,id:'root',url:'https://a.test/master.m3u8?sig=x',playerId:'player-a',relatedUrls:['https://a.test/high.m3u8?sig=y']},
    {...base,id:'child',url:'https://a.test/high.m3u8?sig=y'},
    {...base,id:'other',url:'https://a.test/high.m3u8?sig=y',playerId:'player-b'}];
  assert.deepEqual(lib.groupMedia(items).map(x => x.id),['root','other']);
  assert.deepEqual(lib.mediaRelatedUrls({formats:[{manifestUrl:'high.m3u8?sig=y'}]},items[0].url),['https://a.test/high.m3u8?sig=y']);
});
test('standalone TS and disposition-only media survive while known fragments stay suppressed',() => {
  assert.deepEqual(lib.classify('https://a.test/movie.ts','video/mp2t'),{kind:'video',manifest:false});
  assert.equal(lib.classify('https://a.test/segment_001.ts','video/mp2t'),null);
  assert.deepEqual(lib.classify('https://a.test/resource','application/octet-stream','','attachment; filename="movie.mp4"'),{kind:'video',manifest:false});
});

test('checkpoints stay bounded without deleting another tab navigation barrier',async () => {
  const memory = {},storage = {get:async () => memory,set:async parts => Object.assign(memory,structuredClone(parts))};
  const store = lib.createSessionStore(storage,{maxItems:3,maxPerTab:2});
  const state = await store.read();
  lib.applyNavigation(state,{tabId:9,frameId:0,timeStamp:10,url:'https://a.test/new',documentId:'new',kind:'commit'});
  await store.changed(9,true);
  for (let i = 0; i < 5; i++) lib.applyCandidate(state,{tabId:1,frameId:0,documentId:'doc',sourcePage:'https://a.test/',url:`https://a.test/${i}.jpg`,kind:'image',capturedAt:i+20});
  await store.changed(1,true);
  assert.equal(Object.values(state.items).length,2);
  assert.equal(state.topCommit[9],10);
  const restored = await lib.createSessionStore(storage).read();
  assert.equal(lib.applyCandidate(restored,{tabId:9,frameId:0,documentId:'old',sourcePage:'https://a.test/old',url:'https://a.test/old.mp4',capturedAt:20,network:true}),null);
});
test('one inspection serves concurrent requests and survives reopening without another native call',async () => {
  const item = {id:'root',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,kind:'video',manifest:true,url:'https://cdn.test/master.m3u8?sig=keep'};
  const bg = browser({mediaStateV1:{items:{root:item},submitted:{},routes:{},topCommit:{}}});
  bg.setNative(async message => message.method === 'wakeup' ? {code:0,data:true} : {code:0,data:{code:0,data:{supported:true,formats:[{id:'best',manifestUrl:'https://cdn.test/high.m3u8?sig=keep'}]}}});
  const replies = await Promise.all(Array.from({length:5},() => bg.request({type:'INSPECT',tabId:1,id:'root'})));
  assert.ok(replies.every(x => x.ok));
  assert.equal(bg.counts.native,2);
  assert.equal((await bg.request({type:'INSPECT',tabId:1,id:'root'})).ok,true);
  assert.equal(bg.counts.native,2);
});

test('missing cookie store fails without reading another profile or submitting',async () => {
  const bg = browser();const id = (await add(bg)).data[0];
  bg.chrome.cookies.getAllCookieStores = async () => [];
  let queries = 0; bg.chrome.cookies.getAll = async () => {queries++;return [{name:'secret',value:'wrong-profile'}];};
  assert.equal((await bg.request({type:'CREATE',tabId:1,id,options:{}})).ok,false);
  assert.equal(queries,0); assert.equal(bg.counts.native,0);
});
test('SPA round trip creates a new epoch so a delayed old task cannot mark new resources submitted',async () => {
  const bg = browser(),old = (await add(bg)).data[0];
  let release,creates = 0;
  bg.setNative(async msg => msg.method === 'wakeup' ? {code:0,data:true} : ++creates === 1 ? await new Promise(resolve => {release = () => resolve({code:0,data:{code:0,data:'old-task'}});}) : {code:0,data:{code:0,data:'new-task'}});
  const pending = bg.request({type:'CREATE',tabId:1,id:old,options:{}});
  while (!release) await new Promise(resolve => setImmediate(resolve));
  const at = Date.now();
  await bg.navigate({tabId:1,frameId:0,timeStamp:at,url:'https://example.test/away',documentId:'doc-1'});
  await bg.navigate({tabId:1,frameId:0,timeStamp:at+1,url:sender.url,documentId:'doc-1'});
  const fresh = (await bg.request({type:'ADD_DOM',timeStamp:at+2,sourcePage:sender.url,items:[{url:'https://cdn.test/0.jpg',kind:'image'}]},sender)).data[0];
  assert.notEqual(fresh,old);
  release(); await pending;
  assert.equal((await bg.request({type:'CREATE',tabId:1,id:fresh,options:{}})).data.taskId,'new-task');
  assert.equal(creates,2);
});
test('cyclic manifest metadata keeps one representative visible',() => {
  const base = {tabId:1,frameId:0,documentId:'d',sourcePage:'https://a.test/page'};
  const items = [{...base,id:'a',url:'https://a.test/a.m3u8',relatedUrls:['https://a.test/b.m3u8']},
    {...base,id:'b',url:'https://a.test/b.m3u8',relatedUrls:['https://a.test/a.m3u8']}];
  assert.deepEqual(lib.groupMedia(items).map(x => x.id),['a']);
});
test('disabling a site wins over a capture that read the earlier enabled state',async () => {
  const bg = browser();let release,gets = 0,setting = {};
  bg.chrome.storage.local.get = async () => { if (++gets === 1) return await new Promise(resolve => {release = () => resolve({});}); return structuredClone(setting); };
  bg.chrome.storage.local.set = async data => {Object.assign(setting,data);};
  const capture = add(bg);
  while (!release) await new Promise(resolve => setImmediate(resolve));
  const off = bg.request({type:'SITE',tabId:1,host:'example.test',set:true});
  await new Promise(resolve => setImmediate(resolve));
  release();await capture;assert.equal((await off).ok,true);
  assert.equal((await bg.request({type:'LIST',tabId:1})).data.length,0);
});

test('a delayed inspection cannot attach metadata to a resource recaptured after an SPA round trip',async () => {
  const root = {id:'root',tabId:1,frameId:0,documentId:'doc-1',sourcePage:sender.url,kind:'video',manifest:true,url:'https://cdn.test/master.m3u8'};
  const bg = browser({mediaStateV1:{items:{root},submitted:{},routes:{},topCommit:{}}});
  let release,inspects = 0;
  bg.setNative(async msg => msg.method === 'wakeup' ? {code:0,data:true} : ++inspects === 1 ? await new Promise(resolve => {release = () => resolve({code:0,data:{code:0,data:{supported:true,mediaId:'old',formats:[{id:'old'}]}}});}) : {code:0,data:{code:0,data:{supported:true,mediaId:'fresh',formats:[{id:'fresh'}]}}});
  const pending = bg.request({type:'INSPECT',tabId:1,id:'root'});
  while (!release) await new Promise(resolve => setImmediate(resolve));
  const at = Date.now();
  await bg.navigate({tabId:1,frameId:0,timeStamp:at,url:'https://example.test/away',documentId:'doc-1'});
  await bg.navigate({tabId:1,frameId:0,timeStamp:at+1,url:sender.url,documentId:'doc-1'});
  const id = (await bg.request({type:'ADD_DOM',timeStamp:at+2,sourcePage:sender.url,items:[{url:root.url,kind:'video'}]},sender)).data[0];
  release();assert.equal((await pending).ok,false);
  assert.equal((await bg.request({type:'INSPECT',tabId:1,id})).data.mediaId,'fresh');
  assert.equal(inspects,2);
});
