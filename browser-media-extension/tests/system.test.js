import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';
import * as lib from '../lib.js';

const pageURL = 'https://page.test/watch';
const sender = {tab:{id:1,title:'播放器'},frameId:0,documentId:'doc-1',url:pageURL};
function browser(seed = {}) {
  const memory = structuredClone(seed), events = {}, messages = [], starts = [], scripts = [], queries = [];
  let handler, inspectData = {supported:true,title:'测试视频',duration:8,thumbnail:'https://cdn.test/cover.jpg',formats:[{id:'v',ext:'mp4',height:720,vcodec:'h264',acodec:'none'},{id:'a',ext:'m4a',vcodec:'none',acodec:'aac',language:'en'}]};
  const event = key => ({addListener(fn){events[key] = fn;}});
  const chrome = {
    storage:{session:{get:async () => structuredClone(memory),set:async p => Object.assign(memory,structuredClone(p)),remove:async k => {delete memory[k];}},local:{get:async () => ({})}},
    action:{setBadgeText:async()=>{},setBadgeBackgroundColor:async()=>{}},
    tabs:{get:async()=>({id:1,url:pageURL}),onRemoved:event('removed'),create:async()=>{},sendMessage:async(_,m)=>{starts.push(m);return {started:true};}},
    cookies:{getAllCookieStores:async()=>[{id:'store-1',tabIds:[1]}],getPartitionKey:async()=>({topLevelSite:'https://page.test'}),getAll:async q=>{queries.push(q);return []; }},
    webNavigation:{onCommitted:event('commit'),onHistoryStateUpdated:event('spa'),onReferenceFragmentUpdated:event('hash'),getFrame:async()=>({url:pageURL,documentId:'doc-1'})},
    webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(k=>[k,event(k)])),
    scripting:{executeScript:async s=>{scripts.push(s);return [];}},
    runtime:{onMessage:{addListener(fn){handler=fn;}},getURL:p=>'chrome-extension://test/'+p,sendMessage:async()=>{},sendNativeMessage:(_,m,callback)=>{messages.push(m);callback(m.method==='wakeup'?{code:0,data:true}:{code:0,data:{code:0,data:m.params.path.endsWith('/media/inspect')?inspectData:m.params.method==='POST'?'one-task':[]}});}}
  };
  vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},chrome,URL,URLSearchParams});
  return {memory,events,messages,starts,scripts,queries,chrome,setInspection:x=>{inspectData=x;},request:(m,s={})=>new Promise(r=>handler(m,s,r))};
}
const scope = {tabId:1,identify:true,frameId:0,documentId:'doc-1',sourcePage:pageURL,playerId:'clicked'};
async function add(b,items,more={}) { return b.request({type:'ADD_DOM',items,sourcePage:pageURL,timeStamp:Date.now(),...more},sender); }

test('enhanced search admits only this new capture session and requires an explicit single choice',async()=>{
  const b=browser(); await add(b,[{url:'https://cdn.test/previous.m3u8',kind:'video'}]);
  assert.equal((await b.request({type:'LIST',...scope})).data.length,0);
  const started=await b.request({type:'DEEP_SEARCH',...scope}); assert.equal(started.ok,true);
  const searchId=started.data.searchId; assert.ok(searchId);
  assert.equal(b.starts[0].searchId,searchId);
  await add(b,[{url:'https://cdn.test/current.m3u8',kind:'video'}],{searchId});
  await add(b,[{url:'https://cdn.test/foreign.m3u8',kind:'video'}],{searchId:'other-session'});
  const choices=(await b.request({type:'LIST',...scope})).data;
  assert.equal(choices.length,1);assert.equal(choices[0].url,'https://cdn.test/current.m3u8');assert.equal(choices[0].manualFor,'clicked');
  assert.equal((await b.request({type:'CREATE',...scope,playerId:'different',target:choices[0].id,id:choices[0].id,options:{format:'v+a'}})).ok,false);
  assert.equal((await b.request({type:'CREATE',...scope,target:choices[0].id,id:choices[0].id,options:{format:'v+a'}})).ok,true);
  const tasks=b.messages.filter(x=>x.params?.path==='/api/v1/tasks');assert.equal(tasks.length,1);assert.equal(tasks[0].params.data.req.url,choices[0].url);
});

test('a worker restart restores only the still-valid enhanced capture scope',async()=>{
  const first=browser();const searchId=(await first.request({type:'DEEP_SEARCH',...scope})).data.searchId;
  const second=browser(first.memory);await add(second,[{url:'https://cdn.test/restored.m3u8',kind:'video'}],{searchId});
  assert.equal((await second.request({type:'LIST',...scope})).data.length,1);
  second.events.spa({tabId:1,frameId:0,documentId:'doc-1',url:'https://page.test/next',timeStamp:Date.now()+1});await settle();
  const result=await add(second,[{url:'https://cdn.test/stale.m3u8',kind:'video'}],{searchId});assert.equal(result.data.length,0);
});

test('opaque manifest intent remains a media task and keys are context only',async()=>{
  const b=browser();const id=(await add(b,[{url:'https://cdn.test/opaque?sig=keep',kind:'video',manifest:true,protocolHint:'manifest',contextUrls:['https://keys.test/keys/session'],relatedUrls:[]}])).data[0];
  const item=(await b.request({type:'LIST',tabId:1,target:id})).data[0];assert.equal(item.manifest,true);
  const result=await b.request({type:'CREATE',tabId:1,target:id,id,options:{format:'v+a'}});assert.equal(result.ok,true);
  const request=b.messages.find(x=>x.params?.path==='/api/v1/tasks').params.data.req;assert.equal(request.protocol,'media');
  assert.ok(b.queries.some(x=>x.domain==='keys.test'&&x.storeId==='store-1'));
  assert.equal((await b.request({type:'LIST',tabId:1})).data.length,1);
});

test('downstream key cookies include their own domain/path without joining the root Cookie header',async()=>{
  const b=browser();b.chrome.cookies.getAll=async q=>{
    b.queries.push(q);
    if(q.url)return [{name:'root',value:'fixture-root',domain:'cdn.test',path:'/',hostOnly:true}];
    if(q.domain==='keys.test')return [{name:'key',value:'fixture-key',domain:'keys.test',path:'/keys/',hostOnly:true}];
    return [];
  };
  const id=(await add(b,[{url:'https://cdn.test/master.m3u8',kind:'video',contextUrls:['https://keys.test/keys/session']}])).data[0];
  await b.request({type:'CREATE',tabId:1,id,options:{format:'v+a'}});
  const extra=b.messages.find(x=>x.params?.path==='/api/v1/tasks').params.data.req.extra;
  assert.equal(extra.header.Cookie,undefined);assert.ok(extra.cookies.some(x=>x.name==='root'&&x.domain==='cdn.test'));assert.ok(extra.cookies.some(x=>x.domain==='keys.test'&&x.path==='/keys/'));
});

test('observed Authorization is confined to its request scope and never checkpointed in candidates',async()=>{
  const b=browser();const url='https://cdn.test/private.mp4';
  b.events.onBeforeSendHeaders({tabId:1,frameId:0,documentId:'doc-1',url,requestId:'r',timeStamp:Date.now(),requestHeaders:[{name:'Authorization',value:'Bearer fixture-only-secret'}]});
  b.events.onHeadersReceived({tabId:1,frameId:0,documentId:'doc-1',url,requestId:'r',timeStamp:Date.now(),type:'media',responseHeaders:[{name:'Content-Type',value:'video/mp4'}]});
  await settle();await settle();
  const item=(await b.request({type:'LIST',tabId:1})).data[0];assert.equal(item.mediaRequired,true);
  assert.equal(JSON.stringify(b.memory).includes('fixture-only-secret'),false);
  await b.request({type:'INSPECT',tabId:1,id:item.id});
  const extra=b.messages.find(x=>x.params?.path==='/api/v1/media/inspect').params.data.req.extra;
  assert.equal(extra.scopedHeaders[0].url,url);assert.equal(extra.scopedHeaders[0].origin,'https://cdn.test');assert.equal(extra.header.Authorization,undefined);
  const restarted=browser(b.memory);assert.equal((await restarted.request({type:'INSPECT',tabId:1,id:item.id})).ok,false);
});

test('large signed-context bursts stay within checkpoint budget and retain navigation barriers',async()=>{
  const items={};
  for(let i=0;i<1000;i++)items[`item-${i}`]={id:`item-${i}`,tabId:1,frameId:0,kind:'image',url:`https://cdn.test/image/${i}?sig=${'s'.repeat(500)}`,contextUrls:Array.from({length:16},(_,j)=>`https://cdn.test/part/${j}?sig=${'c'.repeat(500)}`),capturedAt:i};
  const memory={mediaStateV1:{items,submitted:{},routes:{'1:0':{at:99,url:pageURL,kind:'commit'}},topCommit:{1:99}}};
  const store=lib.createSessionStore({get:async()=>memory,set:async p=>Object.assign(memory,p),remove:async k=>delete memory[k]});
  await store.read();await store.changed(1,true);
  const checkpoint=memory['mediaTabV2:1'];assert.ok(JSON.stringify(checkpoint).length*2<6*1024*1024);
  assert.equal(checkpoint.routes['1:0'].at,99);assert.equal(checkpoint.topCommit[1],99);assert.ok(Object.keys(checkpoint.items).length<1000);
});
