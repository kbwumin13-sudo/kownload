import test from 'node:test';
import assert from 'node:assert/strict';
import {installDownloadCapture} from '../download-capture.js';

const tick=()=>new Promise(resolve=>setImmediate(resolve));
function fixture({enabled=true,confirm=false,ping={code:0,data:true},reply,receipt={state:'unknown',taskIds:[]},session={}}={}) {
  const events={},calls=[],messages=[],rpc=[],saved=structuredClone(session);
  const event=name=>({addListener:fn=>{(events[name] ||= []).push(fn);}});
  const f={events,calls,messages,rpc,saved,settings:{downloadCapture:{enabled,confirm}},receipt,leases:0,wakes:0,settingsReads:0,itemState:{id:7,state:'in_progress',paused:true,bytesReceived:0,totalBytes:20}};
  f.browser={
    permissions:{contains:async()=>true,onAdded:event('permission'),onRemoved:event('removedPermission')},
    storage:{local:{get:async()=>({downloadCapture:{enabled,confirm}})},session:{get:async()=>structuredClone(saved),set:async value=>Object.assign(saved,structuredClone(value))},onChanged:event('storage')},
    downloads:{onCreated:event('created'),onChanged:event('changed'),onDeterminingFilename:event('download'),pause:async id=>calls.push(['pause',id]),resume:async id=>calls.push(['resume',id]),cancel:async id=>calls.push(['cancel',id]),search:async()=>{if(f.searchError)throw Error('query');return f.itemState ? [structuredClone(f.itemState)] : [];}},
    webRequest:{onBeforeSendHeaders:event('headers')},action:{setBadgeText:async()=>{},setTitle:async()=>{}}
  };
  f.send=async message=>{
    messages.push(message);
    if(message.method==='ping'){if(ping instanceof Error)throw ping;return ping;}
    if(reply instanceof Error)throw reply;
    if(typeof reply==='function')return reply(message);
    return reply || {code:0,httpStatus:200,businessCode:0,operationId:message.operationId,data:{code:0,operationId:message.operationId,data:'task-one',submission:{state:'created',taskIds:['task-one']}}};
  };
  f.bridge={
    wake:async()=>{f.wakes++;if(f.wakeError)throw Error('unavailable');},
    settings:async()=>{f.settingsReads++;if(f.settingsError)throw Error('unavailable');return structuredClone(f.settings);},
    identity:async()=>({clientId:'client-one',browser:'chrome'}),
    send:f.send,acquire:()=>{f.leases++;let released=false;return()=>{if(!released){released=true;f.leases--;}};},
    request:async(action,params)=>{rpc.push({action,...structuredClone(params)});if(f.receipt instanceof Error)throw f.receipt;return typeof f.receipt==='function' ? f.receipt(params) : structuredClone(f.receipt);}
  };
  f.controller=installDownloadCapture(f.browser,f.send,{appBridge:f.bridge,pollMs:0});
  f.item={id:7,url:'https://files.test/a.bin',finalUrl:'https://files.test/a.bin',filename:'/downloads/a.bin'};
  f.observe=async(overrides={})=>{
    for(const fn of events.headers)fn({url:f.item.finalUrl,tabId:1,frameId:0,requestId:'request-one',type:'main_frame',method:'GET',requestHeaders:[{name:'Cookie',value:'synthetic-cookie'},{name:'Authorization',value:'synthetic-token'},{name:'Referer',value:'https://page.test/'}],...overrides});
    await tick();
  };
  f.capture=async(overrides={},method='GET')=>{await f.observe({method});await f.controller.capture({...f.item,...overrides});};
  return f;
}
const accepted=message=>({code:0,httpStatus:202,businessCode:0,operationId:message.operationId,data:{code:0,operationId:message.operationId,data:null,submission:{state:'accepted',taskIds:[]}}});
const submissions=f=>f.messages.filter(message=>message.method==='create');

test('automatic capture uses current App settings and cancels only after a valid created receipt',async()=>{
  const f=fixture();await f.capture();assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(submissions(f).length,1);
  const message=submissions(f)[0];assert.equal(message.meta.source,'browser-capture');assert.equal(message.meta.silent,true);assert.equal(message.meta.clientId,'client-one');
  assert.equal(message.params.req.extra.header.Cookie,'synthetic-cookie');assert.equal(message.params.req.extra.header.Authorization,'synthetic-token');
  assert.equal(f.controller.jobs.get(7).state,'created');assert.equal(f.leases,0);
  assert.equal(JSON.stringify(f.saved).includes('synthetic-cookie'),false);assert.equal(JSON.stringify(f.saved).includes('https://files.test'),false);
  await f.capture();assert.equal(submissions(f).length,1);
});
test('App mode changes apply to the next capture even while the extension cache says disabled',async()=>{
  const f=fixture({enabled:false});f.settings.downloadCapture={enabled:true,confirm:true};f.messages.length=0;
  await f.capture();assert.equal(submissions(f).length,1);assert.equal(submissions(f)[0].meta.silent,false);assert.equal(f.settingsReads,1);
});
test('disabled or unavailable App settings leave the original browser request intact',async()=>{
  for(const fail of [false,true]) {
    const f=fixture({enabled:false});f.settingsError=fail;await f.capture();
    assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(submissions(f).length,0);assert.equal(f.leases,0);
  }
});
test('an accepted App confirmation is pending, not a created task or browser cancellation',async()=>{
  const f=fixture({confirm:true,reply:accepted});await f.capture();
  assert.equal(f.controller.jobs.get(7).state,'accepted');assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.leases,1);assert.equal(submissions(f).length,1);
  f.receipt={state:'cancelled',taskIds:[]};await f.controller.poll(f.controller.jobs.get(7));assert.equal(f.leases,0);
});
test('App approval is gated by the current download and does not issue another create',async()=>{
  const f=fixture({confirm:true,reply:accepted});await f.capture();const job=f.controller.jobs.get(7);
  f.itemState.bytesReceived=f.itemState.totalBytes;
  f.receipt=params=>params.decision==='commit' ? {state:'accepted',taskIds:[],commitStarted:true} : {state:'accepted',taskIds:[],commitRequested:true,confirmToken:'token-one'};
  await f.controller.poll(job);
  assert.equal(f.rpc.at(-1).decision,'commit');assert.equal(job.state,'authorizing');assert.equal(submissions(f).length,1);
  f.receipt={state:'created',taskIds:['task-one']};await f.controller.poll(job);assert.equal(job.state,'created');assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);
});
test('a real browser completion during confirmation is reported separately and never re-created',async()=>{
  const f=fixture({confirm:true,reply:accepted});await f.capture();f.itemState.state='complete';f.itemState.bytesReceived=20;
  f.receipt={state:'browser_completed',taskIds:[]};await f.controller.poll(f.controller.jobs.get(7));
  assert.equal(f.rpc[0].decision,undefined);assert.equal(f.rpc[0].browserState.state,'complete');assert.equal(submissions(f).length,1);assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.leases,0);
});
test('only an explicit App cancellation cancels an in-progress browser download',async()=>{
  const f=fixture({confirm:true,reply:accepted});await f.capture();f.receipt={state:'cancelled',taskIds:[]};
  await f.controller.poll(f.controller.jobs.get(7));assert.equal(f.controller.jobs.get(7).state,'cancelled');assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);
});
for(const ping of [{code:0,data:false},new Error('host disconnected')])test('pre-submission host unavailability safely resumes the browser',async()=>{
  const f=fixture({ping});await f.capture();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(submissions(f).length,0);assert.equal(f.leases,0);
});
test('explicit Native rejection safely resumes the original browser request',async()=>{
  const f=fixture({reply:{code:1,error:'ipc_unavailable'}});await f.capture();
  assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.controller.jobs.get(7).state,'browser');
});
test('a lost create reply is queried once by operationId, never replayed or blindly resumed',async()=>{
  const f=fixture({reply:new Error('lost'),receipt:{state:'unknown',taskIds:[]}});await f.capture();
  assert.equal(submissions(f).length,1);assert.equal(f.rpc.length,1);assert.equal(f.rpc[0].operationId,submissions(f)[0].operationId);
  assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.controller.jobs.get(7).state,'uncertain');assert.equal(f.leases,0);
});
test('the existing created result can be recovered after its Native reply is lost',async()=>{
  const f=fixture({reply:new Error('lost'),receipt:{state:'created',taskIds:['real-task']}});await f.capture();
  assert.equal(submissions(f).length,1);assert.equal(f.controller.jobs.get(7).state,'created');assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);
});
test('a browser cancellation failure preserves the created fact and task IDs for recovery',async()=>{
  const f=fixture();f.browser.downloads.cancel=async()=>{throw Error('permission changed');};await f.capture();
  const job=f.controller.jobs.get(7);assert.equal(job.state,'created');assert.deepEqual(job.taskIds,['task-one']);assert.equal(f.saved.downloadCapturePending[7].state,'created');assert.equal(f.leases,0);
});
test('a completed file is not deleted or downgraded when an App-created result arrives',async()=>{
  const f=fixture({reply:message=>{f.itemState.state='complete';return {code:0,httpStatus:200,businessCode:0,operationId:message.operationId,data:{code:0,operationId:message.operationId,data:'one',submission:{state:'created',taskIds:['one']}}};}});
  await f.capture();assert.equal(f.controller.jobs.get(7).state,'created');assert.deepEqual(f.calls,[['pause',7]]);
});
test('malformed or mismatched creation receipts cannot cause browser cancellation',async()=>{
  const f=fixture({reply:message=>({code:0,httpStatus:200,businessCode:0,operationId:'other',data:{code:0,operationId:message.operationId,submission:{state:'created',taskIds:['one']}}})});await f.capture();
  assert.equal(f.controller.jobs.get(7).state,'uncertain');assert.deepEqual(f.calls,[['pause',7]]);
});
test('worker recovery queries an already submitted operation without retaining credentials or sending create',async()=>{
  const f=fixture({session:{downloadCapturePending:{7:{id:7,name:'a.bin',operationId:'persisted-operation',state:'accepted',posted:true}}},receipt:{state:'created',taskIds:['existing']}});
  await tick();assert.equal(submissions(f).length,0);assert.equal(f.rpc[0].operationId,'persisted-operation');assert.deepEqual(f.calls,[['cancel',7]]);assert.equal(f.leases,0);
});
test('worker recovery resumes a definitely unsubmitted request without reconstructing its lost headers',async()=>{
  const f=fixture({session:{downloadCapturePending:{7:{id:7,name:'a.bin',operationId:'old-operation',state:'waiting',posted:false}}}});
  await tick();assert.deepEqual(f.calls,[['resume',7]]);assert.equal(f.messages.length,0);
});
test('the first created event pauses immediately before filename determination',async()=>{
  const f=fixture();await tick();f.events.created[0](f.item);assert.deepEqual(f.calls,[['pause',7]]);
  await f.observe();let suggestions=0;f.events.download[0](f.item,()=>{suggestions++;});await tick();
  assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(suggestions,1);
});
test('a held filename is released once on safe fallback and not before an App decision',async()=>{
  const f=fixture({reply:accepted});await f.observe();let suggestions=0;
  assert.equal(f.events.download[0](f.item,()=>{suggestions++;}),true);await tick();assert.equal(suggestions,0);
  f.receipt={state:'cancelled',taskIds:[]};await f.controller.poll(f.controller.jobs.get(7));assert.equal(suggestions,1);
});
test('an early-paused POST resumes the same browser request without Native submission',async()=>{
  const f=fixture();await tick();f.events.created[0](f.item);await f.observe({method:'POST'});let suggestions=0;
  f.events.download[0](f.item,()=>{suggestions++;});await tick();
  assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,0);assert.equal(suggestions,1);
});
test('blob, incognito and other-extension downloads stay with their original owner',async()=>{
  for(const override of [{url:'blob:https://page.test/one',finalUrl:'blob:https://page.test/one'},{incognito:true},{byExtensionId:'other'}]) {
    const f=fixture();await f.capture(override);assert.deepEqual(f.calls,[]);assert.equal(f.messages.length,0);
  }
});
for(const method of ['GET','POST'])test('ambiguous same-URL '+method+'/GET navigation requests cannot borrow another tab headers',async()=>{
  const f=fixture();await f.observe({method,requestId:'request-A',tabId:1});await f.observe({requestId:'request-B',tabId:2});
  await f.controller.capture(f.item);assert.deepEqual(f.calls,[]);assert.equal(f.messages.length,0);
});
test('a retry of the same request identity retains its final headers',async()=>{
  const f=fixture();await f.observe({requestHeaders:[{name:'Authorization',value:'synthetic-A'}]});await f.observe({requestHeaders:[{name:'Authorization',value:'synthetic-B'}]});
  await f.controller.capture(f.item);assert.equal(submissions(f)[0].params.req.extra.header.Authorization,'synthetic-B');
});
test('ordinary association is not displaced by image or XHR resources',async()=>{
  const f=fixture();await f.observe();await f.observe({type:'image',requestId:'image-other',tabId:2});
  await f.controller.capture(f.item);assert.equal(submissions(f).length,1);assert.equal(submissions(f)[0].params.req.extra.header.Cookie,'synthetic-cookie');
});
test('browser query failure before posting cannot strand an original request',async()=>{
  const f=fixture();f.searchError=true;await f.capture();assert.equal(submissions(f).length,0);assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.leases,0);
});

test('completion while waiting sends status before any token-bound decision',async()=>{
 const f=fixture({confirm:true,reply:accepted});await f.capture();f.itemState.state='complete';
 f.receipt=params=>{if(params.decision){assert.equal(params.confirmToken,'token-one');assert.equal(params.decision,'browser_completed');return {state:'browser_completed',taskIds:[]};}return {state:'accepted',taskIds:[],commitRequested:true,confirmToken:'token-one'};};
 await f.controller.poll(f.controller.jobs.get(7));assert.equal(f.rpc[0].decision,undefined);assert.equal(f.rpc[1].decision,'browser_completed');assert.equal(f.controller.jobs.get(7).resolved,true);assert.equal(submissions(f).length,1);
});

test('enabled download wakes the App before canonical settings; passive/off capture does not',async()=>{
 const f=fixture();let running=false;f.bridge.wake=async()=>{f.wakes++;running=true;};
 const read=f.bridge.settings;f.bridge.settings=async()=>{assert.equal(running,true);return read();};
 assert.equal(f.wakes,0);await f.observe();assert.equal(f.wakes,0);await f.capture();assert.equal(f.wakes,1);assert.equal(submissions(f).length,1);
 const off=fixture({enabled:false});await off.capture();assert.equal(off.wakes,0);assert.equal(submissions(off).length,0);
 const failure=fixture();failure.wakeError=true;await failure.capture();assert.equal(submissions(failure).length,0);assert.deepEqual(failure.calls,[['pause',7],['resume',7]]);
});
