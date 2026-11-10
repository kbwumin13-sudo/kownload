import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {installDownloadCapture} from '../download-capture.js';
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const hash=value=>createHash('sha256').update(value).digest('hex');
const binding={id:7,startTime:'2026-11-11T00:00:00.000Z',urlHash:hash('https://files.test/a.bin')};
const unknown=op=>({version:1,operationId:op,state:'unknown',taskIds:[]});
function fixture({enabled=true,confirm=false,saved={},localFail=false}={}) {
 const events={},calls=[],rpc=[],journal=structuredClone(saved);
 const event=name=>({addListener:fn=>{(events[name] ||= []).push(fn);}});
 const f={events,calls,rpc,journal,leases:0,wakes:0,settingsReads:0,localFail,settings:{downloadCapture:{enabled,confirm}},item:{id:7,url:'https://files.test/a.bin',finalUrl:'https://files.test/a.bin',filename:'/downloads/a.bin',startTime:binding.startTime,state:'in_progress',paused:true,bytesReceived:0,totalBytes:20},core:null};
 f.browser={permissions:{contains:async()=>true,onAdded:event('permission'),onRemoved:event('removedPermission')},storage:{local:{get:async key=>key==='downloadCapture'?{downloadCapture:{enabled,confirm}}:structuredClone(journal),set:async value=>{if(f.localFail)throw Error('disk');Object.assign(journal,structuredClone(value));}},onChanged:event('storage')},downloads:{onCreated:event('created'),onChanged:event('changed'),onDeterminingFilename:event('download'),pause:async id=>calls.push(['pause',id]),resume:async id=>{calls.push(['resume',id]);if(f.item)f.item.paused=false;},cancel:async id=>{calls.push(['cancel',id]);if(f.cancelError)throw Error('cancel');if(f.onCancel)await f.onCancel();else Object.assign(f.item,{state:'interrupted',error:'USER_CANCELED'});},search:async()=>{if(f.searchError)throw Error('search');return f.item?[structuredClone(f.item)]:[];}},webRequest:{onBeforeSendHeaders:event('headers')},action:{setBadgeText:async()=>{},setTitle:async()=>{}}};
 const record=(state,op,reason)=>({version:1,operationId:op,state,taskId:'task-one',taskIds:['task-one'],download:binding,...(reason?{reason}:{})});
 f.bridge={wake:async()=>{f.wakes++;if(f.wakeError)throw Error('wake');},settings:async()=>{f.settingsReads++;if(f.settingsError)throw Error('settings');return structuredClone(f.settings);},identity:async()=>({clientId:'client-one',browser:'chrome'}),acquire:()=>{f.leases++;let done=false;return()=>{if(!done){done=true;f.leases--;}};},send:async()=>{throw Error('one-phase forbidden');},request:async(action,params={})=>{
  rpc.push({action,...structuredClone(params)});
  if(f.handler){const v=await f.handler(action,params,record);if(v!==undefined)return v;}
  if(action==='handoffCapabilities')return {version:1};
  if(params.pending)return {version:1,items:[]};
  if(action==='handoffPrepare'){f.core=f.settings.downloadCapture.confirm?{version:1,operationId:params.operationId,state:'accepted',taskIds:[]}:record('prepared',params.operationId);return structuredClone(f.core);}
  if(action==='handoffQuery')return structuredClone(f.core||unknown(params.operationId));
  if(action==='handoffAbort'){f.core=f.core?.taskId?record('aborted',params.operationId,params.reason):unknown(params.operationId);return structuredClone(f.core);}
  if(action==='handoffCommit'){f.core=record('committed',params.operationId);return structuredClone(f.core);}
  throw Error('unexpected action');
 }};
 f.controller=installDownloadCapture(f.browser,()=>{throw Error('one-phase forbidden');},{appBridge:f.bridge,pollMs:0});
 f.observe=async(overrides={})=>{for(const fn of events.headers)fn({url:f.item.finalUrl,tabId:1,frameId:0,requestId:'request-one',type:'main_frame',method:'GET',requestHeaders:[{name:'Cookie',value:'synthetic-cookie'},{name:'Authorization',value:'synthetic-token'}],...overrides});await tick();};
 f.capture=async(overrides={},method='GET')=>{await f.observe({method});await f.controller.capture({...f.item,...overrides});};
 f.job=()=>f.controller.jobs.get(7);
 f.prepares=()=>rpc.filter(x=>x.action==='handoffPrepare');
 f.commits=()=>rpc.filter(x=>x.action==='handoffCommit');
 return f;
}

test('automatic capture prepares, freshly verifies cancellation, then commits same task once',async()=>{
 const f=fixture();await f.capture();assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(f.prepares().length,1);assert.equal(f.commits().length,1);assert.equal(f.job().state,'committed');assert.equal(f.leases,0);
 assert.equal(f.commits()[0].taskId,'task-one');assert.equal(f.commits()[0].cancelConfirmed,true);assert.equal(f.commits()[0].browserState.error,'USER_CANCELED');
 assert.equal(f.prepares()[0].req.extra.header.Cookie,'synthetic-cookie');assert.equal(JSON.stringify(f.journal).includes('synthetic-cookie'),false);assert.equal(JSON.stringify(f.journal).includes('https://files.test'),false);
 await f.capture();assert.equal(f.prepares().length,1);
});
test('settings change applies before next actual capture; explicit enabled download wakes first',async()=>{
 const f=fixture({enabled:false});f.settings.downloadCapture={enabled:true,confirm:true};await f.capture();assert.equal(f.prepares().length,1);assert.equal(f.job().state,'accepted');assert.equal(f.settingsReads,1);
 const cold=fixture();let running=false;cold.bridge.wake=async()=>{running=true;cold.wakes++;};const settings=cold.bridge.settings;cold.bridge.settings=async()=>{assert.equal(running,true);return settings();};await cold.observe();assert.equal(cold.wakes,0);await cold.capture();assert.equal(cold.wakes,1);
});
for(const fail of ['disabled','settings','wake','capability'])test('pre-prepare '+fail+' retains browser without old create fallback',async()=>{
 const f=fixture({enabled:fail!=='disabled'});f.settingsError=fail==='settings';f.wakeError=fail==='wake';if(fail==='capability')f.handler=async action=>{if(action==='handoffCapabilities')return {version:0};};await f.capture();assert.equal(f.prepares().length,0);assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.leases,0);
});
test('accepted confirmation is not a task; prepared approval can hand over fully buffered in-progress item',async()=>{
 const f=fixture({confirm:true});await f.capture();assert.equal(f.job().state,'accepted');assert.deepEqual(f.calls,[['pause',7]]);
 f.item.bytesReceived=f.item.totalBytes;f.core={version:1,operationId:f.job().operationId,state:'prepared',taskId:'task-one',taskIds:['task-one'],download:binding};await f.controller.poll(f.job());assert.equal(f.job().state,'committed');assert.equal(f.prepares().length,1);
});
test('genuine complete during accepted confirmation aborts pending App UI and never commits',async()=>{
 const f=fixture({confirm:true});await f.capture();f.item.state='complete';await f.controller.poll(f.job());assert.equal(f.rpc.at(-1).reason,'browser_completed');assert.equal(f.commits().length,0);assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.leases,0);
});
for(const when of ['prepare','cancel'])test('browser completion at '+when+' retains completed file, aborts held task',async()=>{
 const f=fixture();if(when==='prepare')f.handler=async(action)=>{if(action==='handoffPrepare')f.item.state='complete';};else f.onCancel=()=>{f.item.state='complete';};await f.capture();assert.equal(f.commits().length,0);assert.equal(f.core.state,'aborted');assert.equal(f.core.reason,'browser_completed');assert.equal(f.item.state,'complete');
});
for(const state of [{state:'in_progress',paused:true},{state:'interrupted',error:'NETWORK_FAILED'},null])test('cancel callback without fresh USER_CANCELED proof does not commit '+JSON.stringify(state),async()=>{
 const f=fixture();f.onCancel=()=>{f.item=state?{...f.item,...state}:null;};await f.capture();assert.equal(f.commits().length,0);assert.equal(f.job().state,'uncertain');assert.equal(f.calls.some(x=>x[0]==='resume'),false);
});
test('download identity mismatch cannot cancel another item or commit',async()=>{
 const f=fixture();f.handler=async action=>{if(action==='handoffPrepare')f.item.startTime='2026-11-12T00:00:00Z';};await f.capture();assert.equal(f.commits().length,0);assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.core.state,'aborted');
});
test('lost prepare ACK and unavailable query leaves task held while browser resumes; late recovery aborts it',async()=>{
 const f=fixture();f.handler=async(action,params,record)=>{if(action==='handoffPrepare'){f.core=record('prepared',params.operationId);throw Error('lost');}if(action==='handoffQuery'&&!params.pending)throw Error('unavailable');};await f.capture();assert.equal(f.prepares().length,1);assert.equal(f.commits().length,0);assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.core.state,'prepared');assert.equal(f.job().browserRetained,true);
 f.handler=null;await f.controller.poll(f.job());assert.equal(f.core.state,'aborted');assert.equal(f.commits().length,0);assert.equal(f.prepares().length,1);
});
test('lost prepare ACK with query recovery uses same held task',async()=>{
 const f=fixture();f.handler=async(action,params,record)=>{if(action==='handoffPrepare'){f.core=record('prepared',params.operationId);throw Error('lost');}};await f.capture();assert.equal(f.prepares().length,1);assert.equal(f.job().state,'committed');assert.equal(f.commits().length,1);
});
test('lost commit ACK and unavailable query never resume browser or recreate; recovery only queries',async()=>{
 const f=fixture();f.handler=async(action,params,record)=>{if(action==='handoffCommit'){f.core=record('committed',params.operationId);throw Error('lost');}if(action==='handoffQuery'&&!params.pending)throw Error('unavailable');};await f.capture();assert.equal(f.job().state,'uncertain');assert.equal(f.job().cancelConfirmed,true);assert.equal(f.item.state,'interrupted');assert.equal(f.prepares().length,1);assert.equal(f.commits().length,1);assert.equal(f.calls.some(x=>x[0]==='resume'),false);
 f.handler=null;await f.controller.poll(f.job());assert.equal(f.job().state,'committed');assert.equal(f.commits().length,1);
});
test('commit request never delivered stays prepared, then retries only same task with durable cancel proof',async()=>{
 const f=fixture();f.handler=async action=>{if(action==='handoffCommit'||action==='handoffQuery')throw Error('offline');};await f.capture();assert.equal(f.core.state,'prepared');const journal=structuredClone(f.journal);const next=fixture({saved:journal});next.item={...next.item,state:'interrupted',error:'USER_CANCELED'};next.core=f.core;await tick();await tick();assert.equal(next.prepares().length,0);assert.equal(next.commits().length,1);assert.equal(next.commits()[0].taskId,'task-one');assert.equal(next.job().state,'committed');assert.equal(next.wakes,0);
});
test('crash between browser cancel and confirmation journal remains unknown, never infers proof',async()=>{
 const record={version:1,id:7,name:'a.bin',operationId:'old-operation',state:'cancelling',stage:'cancel_intent',posted:true,download:binding,clientId:'client-one',browser:'chrome',taskId:'task-one'};
 const f=fixture({saved:{downloadHandoffPendingV1:{7:record}}});f.item={...f.item,state:'interrupted',error:'USER_CANCELED'};f.core={version:1,operationId:record.operationId,state:'prepared',taskId:'task-one',taskIds:['task-one'],download:binding};await tick();await tick();assert.equal(f.job().state,'uncertain');assert.equal(f.commits().length,0);assert.equal(f.prepares().length,0);assert.equal(f.wakes,0);
});
test('local journal failure prevents cancellation and commit',async()=>{
 const f=fixture();f.handler=async action=>{if(action==='handoffPrepare')f.localFail=true;};await f.capture();assert.equal(f.commits().length,0);assert.deepEqual(f.calls,[['pause',7]]);
});
test('explicit App cancellation is distinct from browser completion and cancels only matching active item',async()=>{
 const f=fixture({confirm:true});await f.capture();f.core={version:1,operationId:f.job().operationId,state:'aborted',taskId:'task-one',taskIds:['task-one'],download:binding,reason:'user_cancelled'};await f.controller.poll(f.job());assert.equal(f.job().state,'aborted');assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(f.commits().length,0);
});
test('invalid receipts cannot trigger browser cancellation',async()=>{
 const f=fixture();f.handler=async(action,params)=>{if(action==='handoffPrepare'||action==='handoffQuery'&&!params.pending)return {version:1,operationId:'other',state:'prepared',taskId:'task-one',taskIds:['task-one'],download:binding};};await f.capture();assert.equal(f.calls.some(x=>x[0]==='cancel'),false);assert.equal(f.commits().length,0);
});
for(const method of ['GET','POST'])test('same-URL ambiguous '+method+'/GET requests keep original browser requests',async()=>{
 const f=fixture();await f.observe({method,requestId:'A',tabId:1});await f.observe({requestId:'B',tabId:2});await f.controller.capture(f.item);assert.deepEqual(f.calls,[]);assert.equal(f.prepares().length,0);
});
test('same request retry keeps final headers; XHR and image traffic cannot displace navigation',async()=>{
 const f=fixture();await f.observe({requestHeaders:[{name:'Authorization',value:'synthetic-A'}]});await f.observe({requestHeaders:[{name:'Authorization',value:'synthetic-B'}]});await f.observe({type:'image',requestId:'other',tabId:2});await f.observe({type:'xmlhttprequest',requestId:'fetch',tabId:2});await f.controller.capture(f.item);assert.equal(f.prepares()[0].req.extra.header.Authorization,'synthetic-B');
});
test('onCreated pauses before filename; filename released exactly once after committed',async()=>{
 const f=fixture();await tick();f.events.created[0](f.item);assert.deepEqual(f.calls,[['pause',7]]);await f.observe();let suggestions=0;f.events.download[0](f.item,()=>suggestions++);for(let i=0;i<30 && !suggestions;i++)await tick();assert.equal(suggestions,1);assert.equal(f.job().state,'committed');
});
test('early-paused POST resumes without any App submission',async()=>{
 const f=fixture();await tick();f.events.created[0](f.item);await f.observe({method:'POST'});let suggestions=0;f.events.download[0](f.item,()=>suggestions++);await tick();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.prepares().length,0);assert.equal(suggestions,1);
});
test('blob, incognito and another extension retain original ownership',async()=>{
 for(const override of [{url:'blob:https://page.test/one',finalUrl:'blob:https://page.test/one'},{incognito:true},{byExtensionId:'other'}]){const f=fixture();await f.capture(override);assert.deepEqual(f.calls,[]);assert.equal(f.prepares().length,0);}
});
test('explicit cancellation of accepted App UI needs no fabricated Core task',async()=>{
 const f=fixture({confirm:true});await f.capture();f.core={version:1,operationId:f.job().operationId,state:'aborted',taskIds:[],reason:'user_cancelled'};await f.controller.poll(f.job());assert.equal(f.job().state,'aborted');assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(f.commits().length,0);
});
test('browser search failure before prepare can resume safely without any submitted operation',async()=>{
 const f=fixture();f.searchError=true;await f.capture();assert.equal(f.prepares().length,0);assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.leases,0);
});
test('legitimate NanoID task starting with minus or underscore can complete handoff',async()=>{
 for(const id of ['-task_one','_task-two']) {
  const f=fixture();f.handler=async(action,params)=>{if(['handoffPrepare','handoffCommit'].includes(action))return {version:1,operationId:params.operationId,state:action==='handoffPrepare'?'prepared':'committed',taskId:id,taskIds:[id],download:binding};};await f.capture();assert.equal(f.job().state,'committed');assert.equal(f.commits()[0].taskId,id);
 }
});
