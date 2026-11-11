import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import * as lib from '../lib.js';
import {contractReply,identityReply} from './fixtures/native.js';
function browser(seed={},host=async m=>contractReply(m,{code:0,data:{code:0,data:'task-one'}})) {
 const memory=structuredClone(seed),local={netdiskInstanceId:'submission-profile'},messages=[],logs=[];let listener;
 const event={addListener(){}};
 const chrome={storage:{session:{get:async()=>structuredClone(memory),set:async p=>Object.assign(memory,structuredClone(p)),remove:async k=>delete memory[k]},local:{get:async()=>structuredClone(local),set:async value=>Object.assign(local,structuredClone(value))}},action:{setBadgeText:async()=>{},setBadgeBackgroundColor:async()=>{}},tabs:{get:async()=>({id:1,url:'https://page.test/watch'}),onRemoved:event},cookies:{getAllCookieStores:async()=>[{id:'fixture',tabIds:[1]}],getAll:async()=>[]},webNavigation:{onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event},webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(k=>[k,event])),runtime:{id:'test',onMessage:{addListener:f=>listener=f},sendMessage:async()=>{},sendNativeMessage:(_,m,cb)=>{const info=identityReply(m);if(info){cb(info);return;}messages.push(m);Promise.resolve(host(m)).then(cb);}}};
 vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,chrome,installNetdiskBridge:()=>{},URL,URLSearchParams,console:{info:(...v)=>logs.push(v)}});
 return {chrome,memory,messages,logs,dispatch:m=>listener(m,{},()=>{}),request:m=>new Promise(resolve=>listener(m,{},resolve))};
}
test('ordinary download messages are delegated without a competing media response',()=>{
 assert.equal(browser().dispatch({type:'DOWNLOAD_CAPTURE',action:'status',id:7}),false);
});
// ADD_DOM requires a sender; use a stored candidate to isolate submission semantics.
const item={id:'one',tabId:1,frameId:0,documentId:'fixture-doc',kind:'image',url:'https://cdn.test/synthetic.jpg?signature=fixture-secret',sourcePage:'https://page.test/watch',capturedAt:Date.now()};
const seed={mediaStateV1:{items:{one:item},submitted:{},routes:{},topCommit:{}}};
const create={type:'CREATE',tabId:1,id:'one',options:{}};
const submitted=b=>Object.values(b.memory).flatMap(p=>Object.values(p?.submitted||{}));
const ids=b=>b.messages.filter(m=>m.method==='create').map(m=>m.operationId);
const failure=(kind,status)=>({code:1,error:kind,message:'fixture-secret',httpStatus:status,businessCode:status===200?1000:null,data:{code:1000,msg:'',data:null}});
test('lost reply across a worker restart: query same operation/binding without another create',async()=>{
 const tasks=new Map();let queryUnavailable=true;
 const host=async message=>{
  if(message.method==='browser') {
   if(queryUnavailable)return failure('request_failed',null);
   const id=tasks.get(message.params.operationId);
   return contractReply(message,{code:0,data:{code:0,data:{state:id?'created':'unknown',taskIds:id?[id]:[]}}});
  }
  if(!tasks.has(message.operationId))tasks.set(message.operationId,'task-'+(tasks.size+1));
  return failure('request_failed',null);
 };
 const first=browser(seed,host);const failed=await first.request(create);assert.equal(failed.ok,false);assert.equal(submitted(first).length,0);
 assert.equal(ids(first).length,1);assert.equal(first.messages.filter(m=>m.method==='browser').length,1);
 const journal=Object.values(first.memory).flatMap(p=>Object.values(p?.operations||{}))[0];
 assert.equal(journal.version,1);assert.equal(journal.bridgeBinding.routeScope,'a'.repeat(64));
 queryUnavailable=false;
 const next=browser(first.memory,host);const result=await next.request(create);assert.equal(result.ok,true);assert.equal(result.data.state,'created');assert.equal(result.data.taskId,'task-1');
 assert.equal(ids(next).length,0);assert.equal(next.messages[0].params.operationId,ids(first)[0]);assert.equal(tasks.size,1);
 const duplicate=await next.request(create);assert.equal(duplicate.data.duplicate,true);assert.equal(next.messages.length,1);
 assert.equal(JSON.stringify([...first.logs,...next.logs]).includes('fixture-secret'),false);
 assert.equal(JSON.stringify([...first.logs,...next.logs]).includes('synthetic.jpg'),false);
 assert.ok(next.logs.some(entry=>entry[1].stage==='assistant_created'&&entry[1].domain==='cdn.test'));
});
test('a reply lost within one click queries created receipt and creates once',async()=>{
 const tasks=new Map();
 const b=browser(seed,async message=>{
  if(message.method==='browser') {
   const id=tasks.get(message.params.operationId);
   return contractReply(message,{code:0,data:{code:0,data:{state:'created',taskIds:[id]}}});
  }
  if(!tasks.has(message.operationId))tasks.set(message.operationId,'task-'+(tasks.size+1));
  return failure('http_error',500);
 });
 const result=await b.request(create);assert.equal(result.ok,true);assert.equal(result.data.taskId,'task-1');
 assert.equal(ids(b).length,1);assert.equal(tasks.size,1);
 assert.equal(b.messages.filter(m=>m.params?.action==='submission').length,1);
 assert.ok(b.logs.some(entry=>entry[1].stage==='assistant_query'));
});
test('after the confirmation was closed, another click reopens it with a new operation',async()=>{
 // Flutter answers a known operation from its receipt and opens the panel only for a new one.
 const receipts=new Set();let panels=0;
 const b=browser(seed,async message=>{
  if(!receipts.has(message.operationId)){receipts.add(message.operationId);panels++;}
  return contractReply(message,{code:0,httpStatus:202,data:{code:0,data:null,submission:{state:'accepted',taskIds:[]}}});
 });
 const a=await b.request(create),again=await b.request(create);
 assert.equal(a.data.state,'accepted');assert.equal(again.data.state,'accepted');assert.equal(submitted(b).length,0);
 assert.notEqual(ids(b)[0],ids(b)[1]);assert.equal(panels,2);
});
test('after a definitive failure is fixed, another click runs again with a new operation',async()=>{
 // REST replays a known operation from its ledger, failures included (reviewer reproduction P2-2).
 const ledger=new Map();let fixed=false,runs=0;
 const b=browser(seed,async message=>{
  if(!ledger.has(message.operationId)){runs++;ledger.set(message.operationId,fixed ? 'created' : 'failed');}
  if(ledger.get(message.operationId)==='failed')return failure('business_error',200);
  return contractReply(message,{code:0,data:{code:0,data:'task-1'}});
 });
 const first=await b.request({...create,options:{path:'/bad'}});assert.equal(first.ok,false);
 fixed=true;
 const second=await b.request({...create,options:{path:'/good'}});assert.equal(second.ok,true);assert.equal(second.data.taskId,'task-1');
 assert.notEqual(ids(b)[0],ids(b)[1]);assert.equal(runs,2);assert.equal(ids(b).length,2);
});
test('definitive and legacy answers never replay create',async()=>{
 for(const [reply,count] of [[failure('http_error',409),1],[failure('business_error',200),1],[failure('wakeup_failed',null),1],[{code:0,data:{code:0,data:'task-one'}},1]]){
  const b=browser(seed,async()=>reply);const result=await b.request(create);
  assert.equal(result.ok,false);assert.equal(ids(b).length,count,JSON.stringify(reply));assert.equal(submitted(b).length,0);
 }
 // A legacy host answer (no httpStatus) is never resent: at most one request per click.
 const legacy=browser(seed,async()=>({code:0,data:{code:0,data:'task-one'}}));
 await legacy.request(create);await legacy.request(create);assert.equal(ids(legacy).length,2);
});
// Review finding R2-3 (reviewer reproduction E3): after a rollback without
// switching back the helper, the build 25 host lets the old app create the
// task and answers {code:0} without the bridge fields.
test('a legacy host answer ends the click as legacy_host with no retained operation',async()=>{
 let created=0;
 const b=browser(seed,async message=>{ if(message.method==='create') created++; return {code:0}; });
 const r1=await b.request(create),r2=await b.request(create);
 assert.equal(r1.ok,false);assert.equal(r2.ok,false);
 assert.equal(r1.error,lib.LEGACY_HOST_MESSAGE);
 assert.match(r1.error,/可能已创建任务/);assert.match(r1.error,/换回旧版助手/);
 assert.equal(created,2,'one request per click, never resent');
 assert.notEqual(ids(b)[0],ids(b)[1],'the next click does not reuse the operation');
 assert.equal(submitted(b).length,0);
 assert.deepEqual(Object.values(b.memory).flatMap(p=>Object.keys(p?.operations||{})),[]);
 assert.equal(lib.submissionOutcome(lib.bridgeFailure('legacy_host')),'final');
});
test('concurrent client clicks coalesce before native create',async()=>{
 const b=browser(seed);const results=await Promise.all([b.request(create),b.request(create)]);assert.ok(results.every(x=>x.ok));assert.equal(b.messages.length,1);assert.match(b.messages[0].operationId,/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/);
});
test('unknown receipt is queried again without creating or clearing the bound operation',async()=>{
 const host=async m=>m.method==='create' ? failure('request_failed',null) : contractReply(m,{code:0,data:{code:0,data:{state:'unknown',taskIds:[]}}});
 const first=browser(seed,host);assert.equal((await first.request(create)).ok,false);
 const pending=Object.values(first.memory).flatMap(p=>Object.values(p?.operations||{}))[0];
 const next=browser(first.memory,host);assert.equal((await next.request(create)).ok,false);
 assert.equal(ids(next).length,0);assert.equal(next.messages.length,1);assert.equal(next.messages[0].params.operationId,pending.operationId);
 assert.deepEqual(Object.values(next.memory).flatMap(p=>Object.values(p?.operations||{}))[0],pending);
});
test('profile binding succeeds but media journal write failure sends zero create or wake',async()=>{
 const b=browser(seed);b.chrome.storage.session.set=async()=>{throw Error('fixture disk full');};
 const result=await b.request(create);assert.equal(result.ok,false);
 assert.deepEqual(b.messages,[]);assert.equal(ids(b).length,0);
});
test('same worker retries successfully after its unsent first journal write failed',async()=>{
 const b=browser(seed),save=b.chrome.storage.session.set;let unavailable=true;
 b.chrome.storage.session.set=async value=>{if(unavailable)throw Error('fixture disk full');return save(value);};
 assert.equal((await b.request(create)).ok,false);assert.deepEqual(b.messages,[]);
 unavailable=false;
 const next=await b.request(create);assert.equal(next.ok,true);assert.equal(next.data.taskId,'task-one');
 assert.equal(ids(b).length,1);assert.equal(b.messages.some(m=>m.params?.action==='submission'),false);
 assert.equal((await b.request(create)).data.duplicate,true);assert.equal(ids(b).length,1);
});
for(const kind of ['legacy','version','route'])test('media '+kind+' pending cannot be rebound or replayed',async()=>{
 const first=browser(seed,async m=>m.method==='create'?failure('request_failed',null):contractReply(m,{code:0,data:{code:0,data:{state:'unknown',taskIds:[]}}}));
 await first.request(create);
 for(const part of Object.values(first.memory))for(const [mark,value] of Object.entries(part?.operations||{})) {
  if(kind==='legacy')part.operations[mark]=value.operationId;
  else if(kind==='version')value.version=2;
  else value.bridgeBinding.routeScope='f'.repeat(64);
 }
 const before=structuredClone(first.memory),next=browser(before);
 assert.equal((await next.request(create)).ok,false);assert.equal(next.messages.length,0);assert.equal(ids(next).length,0);
 assert.deepEqual(next.memory,before);
});
for(const kind of ['http','business','legacy','wrong operation'])test(`${kind} ACK does not mark submitted and redacts error`,async()=>{
 const b=browser(seed,async message=>{
  if(kind==='legacy')return {code:0,data:{code:0,data:'task-one'}};
  const reply=contractReply(message,{code:0,data:{code:0,data:'task-one'}});
  if(kind==='http'){reply.httpStatus=500;reply.message='fixture-secret';}
  if(kind==='business'){reply.code=1;reply.error='business_error';reply.message='fixture-secret';}
  if(kind==='wrong operation')reply.operationId='other-operation';
  return reply;
 });
 const result=await b.request(create);assert.equal(result.ok,false);assert.equal(result.error.includes('fixture-secret'),false);assert.equal(submitted(b).length,0);
});
