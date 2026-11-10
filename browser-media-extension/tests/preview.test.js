import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import * as lib from '../lib.js';

test('native preview resolves the actual target headers and never inherits video credentials',async()=>{
 let preview,headers;
 const event={addListener(){}};
 const queries=[];
 const chrome={storage:{local:{get:async()=>({})},session:{}},tabs:{onRemoved:event,get:async()=>({url:'https://page.test/watch'})},
  cookies:{getAllCookieStores:async()=>[{id:'profile',tabIds:[1]}],getAll:async query=>{queries.push(query);return query.url?.startsWith('https://poster.test/')?[{name:'preview',value:'B',domain:'poster.test',path:'/'}]:[];}},
  webNavigation:{onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event},
  webRequest:{...Object.fromEntries(['onBeforeRequest','onHeadersReceived','onCompleted','onErrorOccurred'].map(k=>[k,event])),onBeforeSendHeaders:{addListener:fn=>headers=fn}},runtime:{onMessage:event}};
 vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},installDownloadCapture:()=>{},createAppBridge:()=>({}),createAppMediaBridge:(_,__,hooks)=>{preview=hooks.preview;return{};},chrome,URL});
 const item={tabId:1,frameId:0,documentId:'doc',url:'https://video.test/file.mp4',sourcePage:'https://page.test/watch',header:{Cookie:'video=A',Authorization:'Bearer A'},relatedUrls:['https://video.test/keys/secret']};
 headers({tabId:1,frameId:0,documentId:'doc',requestId:'v',url:item.url,requestHeaders:[{name:'Authorization',value:'Bearer A'}]});
 headers({tabId:1,frameId:0,documentId:'doc',requestId:'p',url:'https://poster.test/cover.jpg',requestHeaders:[{name:'Authorization',value:'Bearer B'}]});
 const result=await preview(item,'https://poster.test/cover.jpg');
 assert.equal(result.previewHeaders.Authorization,'Bearer B');assert.equal(result.previewHeaders.Cookie,'preview=B');
 assert.equal(JSON.stringify(result).includes('Bearer A'),false);assert.equal(JSON.stringify(result).includes('video=A'),false);
 assert.equal(queries.some(q=>q.domain==='video.test'),false);
 const publicResult=await preview(item,'https://public.test/cover.jpg');assert.equal(publicResult.previewHeaders.Authorization,undefined);assert.equal(publicResult.previewHeaders.Cookie,undefined);
 assert.equal((await preview({...item,documentId:'new'},'https://poster.test/cover.jpg')).previewHeaders.Authorization,undefined);
});
