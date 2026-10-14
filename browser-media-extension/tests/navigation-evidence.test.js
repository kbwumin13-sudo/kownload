import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';
import * as lib from '../lib.js';

const page = 'https://page.test/watch#first';
const resource = 'https://cdn.test/opaque?sig=keep';
function browser() {
  const events = {}, messages = [], memory = {}, frames = new Map([[0,{url:page,documentId:'doc-1'}]]);
  let handler;
  const event = name => ({addListener(fn) { events[name] = fn; }});
  const chrome = {
    storage:{session:{get:async () => structuredClone(memory),set:async data => Object.assign(memory,structuredClone(data))},local:{get:async () => ({})}},
    action:{setBadgeText:async () => {},setBadgeBackgroundColor:async () => {}},
    tabs:{get:async () => ({id:1,url:frames.get(0).url}),onRemoved:event('removed'),sendMessage:async () => ({started:true})},
    cookies:{getAllCookieStores:async () => [{id:'fixture-store',tabIds:[1]}],getAll:async () => []},
    scripting:{executeScript:async () => []},
    webNavigation:{onCommitted:event('commit'),onHistoryStateUpdated:event('spa'),onReferenceFragmentUpdated:event('hash'),getFrame:async ({frameId}) => frames.get(frameId)},
    webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(name => [name,event(name)])),
    runtime:{onMessage:{addListener(fn) { handler = fn; }},sendMessage:async () => {},sendNativeMessage:(_,message,callback) => {
      messages.push(message);
      const data = message.params?.path === '/api/v1/media/inspect' ? {supported:true,formats:[{id:'best'}]} : 'fixture-task';
      callback(message.method === 'wakeup' ? {code:0,data:true} : {code:0,data:{code:0,data}});
    }}
  };
  vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},chrome,URL,URLSearchParams});
  const request = (msg,sender = {}) => new Promise(resolve => handler(msg,sender,resolve));
  return {events,messages,memory,request,
    async navigate(kind,url,timeStamp,frameId = 0,documentId = 'doc-1',tabId = 1) {
      frames.set(frameId,{url,documentId});
      events[kind]?.({tabId,frameId,documentId,url,timeStamp});
      await settle();
    },
    async dom(items,{sourcePage = page,timeStamp = 110,frameId = 0,documentId = 'doc-1',tabId = 1,searchId} = {}) {
      return request({type:'ADD_DOM',sourcePage,timeStamp,items,searchId},{tab:{id:tabId,title:'fixture'},frameId,documentId,url:sourcePage});
    },
    async network(mime,{url = resource,timeStamp = 110,frameId = 0,documentId = 'doc-1'} = {}) {
      events.onBeforeRequest({tabId:1,frameId,documentId,url,requestId:url,timeStamp});
      events.onHeadersReceived({tabId:1,frameId,documentId,url,requestId:url,timeStamp:timeStamp+1,type:'media',responseHeaders:[{name:'Content-Type',value:mime}]});
      await settle();
    }
  };
}
const scope = (sourcePage = page,more = {}) => ({tabId:1,identify:true,frameId:0,documentId:'doc-1',sourcePage,playerId:'clicked',...more});
const list = async (b,more = {}) => (await b.request({type:'LIST',tabId:1,...more})).data;

test('hash navigation admits the new DOM route and expires old player, document and search scopes',async () => {
  const b = browser();
  await b.navigate('commit',page,100);
  const oldId = (await b.dom([{url:resource,kind:'video',playerId:'clicked'}])).data[0];
  const searchId = (await b.request({type:'DEEP_SEARCH',...scope()})).data.searchId;
  const nextPage = 'https://page.test/watch#second';
  await b.navigate('hash',nextPage,200);
  const fresh = await b.dom([{url:resource,kind:'video',playerId:'clicked'}],{sourcePage:nextPage,timeStamp:210});
  assert.equal(fresh.data.length,1,'ADD_DOM must follow the browser hash navigation event');
  assert.notEqual(fresh.data[0],oldId);
  assert.equal((await b.request({type:'CREATE',tabId:1,id:oldId,options:{}})).ok,false);
  assert.equal((await list(b,scope(nextPage))).length,1);
  for (const mismatch of [{sourcePage:page},{documentId:'old-doc'},{frameId:7},{playerId:'other'}]) {
    assert.equal((await list(b,scope(nextPage,mismatch))).length,0);
    assert.equal((await b.request({type:'CREATE',...scope(nextPage,mismatch),id:fresh.data[0],options:{}})).ok,false);
  }
  for (const stale of [{sourcePage:page},{documentId:'old-doc'},{timeStamp:190},{searchId}]) {
    assert.equal((await b.dom([{url:'https://cdn.test/stale',kind:'video'}],{sourcePage:nextPage,timeStamp:220,...stale})).data.length,0);
  }
  await b.network('application/vnd.apple.mpegurl',{url:'https://cdn.test/old-doc',timeStamp:230,documentId:'old-doc'});
  assert.equal((await list(b)).length,1,'late old-document responses cannot enter a same-document route');
});

test('iframe hash navigation preserves siblings and other tabs while retaining its own boundary',async () => {
  const b = browser(), framePage = 'https://frame.test/watch#first', nextPage = 'https://frame.test/watch#next';
  await b.navigate('commit',page,100);
  await b.navigate('commit',framePage,105,7,'frame-doc');
  await b.dom([{url:'https://cdn.test/top.mp4',kind:'video',playerId:'clicked'}]);
  await b.dom([{url:resource,kind:'video'}],{frameId:7,documentId:'frame-doc',sourcePage:framePage,timeStamp:110});
  await b.dom([{url:'https://cdn.test/other.mp4',kind:'video'}],{tabId:2});
  await b.navigate('hash',nextPage,200,7,'frame-doc');
  assert.equal((await b.dom([{url:resource,kind:'video'}],{frameId:7,documentId:'frame-doc',sourcePage:nextPage,timeStamp:210})).data.length,1);
  assert.equal((await b.dom([{url:resource,kind:'video'}],{frameId:7,documentId:'wrong-doc',sourcePage:nextPage,timeStamp:220})).data.length,0);
  assert.equal((await list(b)).length,2);
  assert.equal((await list(b,{tabId:2})).length,1);
});

test('manifest evidence is confined to the document and navigation epoch, including a hash round trip',async () => {
  const b = browser();
  await b.navigate('commit',page,100);
  await b.network('application/vnd.apple.mpegurl');
  const old = (await list(b))[0];
  await b.navigate('hash','https://page.test/watch#away',200);
  await b.navigate('hash',page,300);
  const id = (await b.dom([{url:resource,kind:'video'}],{timeStamp:310})).data[0];
  const fresh = (await list(b,{target:id}))[0];
  assert.ok(fresh);
  assert.notEqual(id,old.id);
  assert.equal(fresh.manifest,false);
  assert.equal(fresh.mediaRequired,false);
  await b.navigate('commit',page,400,0,'doc-2');
  const nextId = (await b.dom([{url:resource,kind:'audio'}],{documentId:'doc-2',timeStamp:410})).data[0];
  assert.equal((await list(b,{target:nextId}))[0].kind,'audio');
  assert.equal((await list(b,{target:nextId}))[0].manifest,false);
});

for (const mime of ['application/vnd.apple.mpegurl','application/dash+xml']) {
  for (const networkFirst of [true,false]) {
    test(`${mime} network evidence survives weak DOM (${networkFirst ? 'network first' : 'DOM first'}) through INSPECT and CREATE`,async () => {
      const b = browser();
      await b.navigate('commit',page,100);
      if (networkFirst) await b.network(mime);
      const id = (await b.dom([{url:resource,kind:'video',playerId:'clicked'}],{timeStamp:120})).data[0];
      if (!networkFirst) await b.network(mime,{timeStamp:130});
      const item = (await list(b,{target:id}))[0];
      assert.equal((await b.request({type:'INSPECT',...scope(),id})).ok,true);
      assert.equal((await b.request({type:'CREATE',...scope(),id,options:{format:'best'}})).ok,true);
      const req = b.messages.find(x => x.params?.path === '/api/v1/tasks').params.data.req;
      assert.equal(req.protocol,'media');
      assert.equal(req.url,resource);
      assert.equal(req.extra.format,'best');
      assert.equal(req.extra.sourcePage,page);
      assert.equal(item.manifest,true);
      assert.equal(item.kind,'video');
      assert.equal(item.mediaRequired,true);
      assert.equal(item.playerId,'clicked');
    });
  }
}

for (const [mime,kind] of [['image/webp','image'],['audio/mpeg','audio']]) {
  test(`${mime} network classification cannot be replaced by a weak DOM video hint`,async () => {
    const b = browser();
    await b.navigate('commit',page,100);
    await b.network(mime);
    const id = (await b.dom([{url:resource,kind:'video',playerId:'clicked'}],{timeStamp:120})).data[0];
    const item = (await list(b,{target:id}))[0];
    assert.equal(item.kind,kind);
    assert.equal(item.manifest,false);
    assert.equal(item.mediaRequired,false);
    assert.equal((await list(b,scope())).length,0);
    assert.equal((await b.request({type:'CREATE',tabId:1,id,options:{}})).ok,true);
    assert.equal(b.messages.find(x => x.params?.path === '/api/v1/tasks').params.data.req.protocol,undefined);
  });
}
