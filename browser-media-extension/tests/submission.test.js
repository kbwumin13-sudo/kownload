import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import * as lib from '../lib.js';
import {contractReply} from './fixtures/native.js';
function browser(seed={},host=async m=>contractReply(m,{code:0,data:{code:0,data:'task-one'}})) {
 const memory=structuredClone(seed),messages=[],logs=[];let listener;
 const event={addListener(){}};
 const chrome={storage:{session:{get:async()=>structuredClone(memory),set:async p=>Object.assign(memory,structuredClone(p)),remove:async k=>delete memory[k]},local:{get:async()=>({})}},action:{setBadgeText:async()=>{},setBadgeBackgroundColor:async()=>{}},tabs:{get:async()=>({id:1,url:'https://page.test/watch'}),onRemoved:event},cookies:{getAllCookieStores:async()=>[{id:'fixture',tabIds:[1]}],getAll:async()=>[]},webNavigation:{onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event},webRequest:Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(k=>[k,event])),runtime:{onMessage:{addListener:f=>listener=f},sendMessage:async()=>{},sendNativeMessage:(_,m,cb)=>{messages.push(m);Promise.resolve(host(m)).then(cb);}}};
 vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,chrome,installNetdiskBridge:()=>{},URL,URLSearchParams,console:{info:(...v)=>logs.push(v)}});
 return {memory,messages,logs,request:m=>new Promise(resolve=>listener(m,{},resolve))};
}
// ADD_DOM requires a sender; use a stored candidate to isolate submission semantics.
const item={id:'one',tabId:1,frameId:0,documentId:'fixture-doc',kind:'image',url:'https://cdn.test/synthetic.jpg?signature=fixture-secret',sourcePage:'https://page.test/watch',capturedAt:Date.now()};
const seed={mediaStateV1:{items:{one:item},submitted:{},routes:{},topCommit:{}}};
const create={type:'CREATE',tabId:1,id:'one',options:{}};
const submitted=b=>Object.values(b.memory).flatMap(p=>Object.values(p?.submitted||{}));
test('lost creation response retries the same operation across worker restart',async()=>{
 const tasks=new Map();let lose=true;
 const host=async message=>{
  if(!tasks.has(message.operationId))tasks.set(message.operationId,'task-'+(tasks.size+1));
  if(lose){lose=false;return {code:1,error:'request_failed',message:'fixture-secret'};}
  return contractReply(message,{code:0,data:{code:0,data:tasks.get(message.operationId)}});
 };
 const first=browser(seed,host);const failed=await first.request(create);assert.equal(failed.ok,false);assert.equal(submitted(first).length,0);
 const next=browser(first.memory,host);const result=await next.request(create);assert.equal(result.ok,true);assert.equal(result.data.state,'created');assert.equal(result.data.taskId,'task-1');
 assert.equal(first.messages[0].operationId,next.messages[0].operationId);assert.equal(tasks.size,1);
 const duplicate=await next.request(create);assert.equal(duplicate.data.duplicate,true);assert.equal(next.messages.length,1);
 assert.equal(JSON.stringify([...first.logs,...next.logs]).includes('fixture-secret'),false);
 assert.equal(JSON.stringify([...first.logs,...next.logs]).includes('synthetic.jpg'),false);
 assert.ok(next.logs.some(entry=>entry[1].stage==='assistant_created'&&entry[1].domain==='cdn.test'));
});
test('accepted remains waiting and never becomes submitted',async()=>{
 const b=browser(seed,async message=>contractReply(message,{code:0,httpStatus:202,data:{code:0,data:null,submission:{state:'accepted',taskIds:[]}}}));
 const a=await b.request(create),again=await b.request(create);assert.equal(a.data.state,'accepted');assert.equal(again.data.state,'accepted');assert.equal(submitted(b).length,0);assert.equal(b.messages[0].operationId,b.messages[1].operationId);
});
test('concurrent client clicks coalesce before native create',async()=>{
 const b=browser(seed);const results=await Promise.all([b.request(create),b.request(create)]);assert.ok(results.every(x=>x.ok));assert.equal(b.messages.length,1);assert.match(b.messages[0].operationId,/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/);
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
