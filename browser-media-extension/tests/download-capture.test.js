import test from 'node:test';
import assert from 'node:assert/strict';
import {installDownloadCapture} from '../download-capture.js';

function fixture({enabled=true,confirm=false,ping={code:0,data:true},reply,tabFails=false,searchFails=false,session={}}={}) {
  const events={},calls=[],messages=[],saved=structuredClone(session);
  const event = name => ({addListener:fn => { (events[name] ||= []).push(fn); }});
  const browser={
    permissions:{contains:async()=>true,onAdded:event('permission')},
    storage:{local:{get:async()=>({downloadCapture:{enabled,confirm}})},session:{set:async v=>Object.assign(saved,structuredClone(v)),get:async()=>structuredClone(saved)},onChanged:event('storage')},
    downloads:{onDeterminingFilename:event('download'),pause:async id=>calls.push(['pause',id]),resume:async id=>calls.push(['resume',id]),cancel:async id=>calls.push(['cancel',id]),search:async()=>{if(searchFails)throw Error('search');return[{id:7,state:'in_progress'}];}},
    tabs:{create:async()=>{if(tabFails)throw Error('tab');return{id:18};},onRemoved:event('removed')},
    action:{setBadgeText:async()=>{},setTitle:async()=>{}},
    webNavigation:{onCommitted:event('navigation')},webRequest:{onBeforeSendHeaders:event('headers')},runtime:{getURL:p=>'chrome-extension://ours/'+p,onMessage:event('message')}
  };
  const send=async message=>{
    messages.push(message);
    if(message.method==='ping'){if(ping instanceof Error)throw ping;return ping;}
    if(reply instanceof Error)throw reply;
    if(typeof reply==='function')return reply(message);
    return reply || {code:0,httpStatus:200,businessCode:0,operationId:message.operationId,data:{code:0,operationId:message.operationId,data:'task-one',submission:{state:'created',taskIds:['task-one']}}};
  };
  const controller=installDownloadCapture(browser,send);
  const item={id:7,url:'https://files.test/a.bin',finalUrl:'https://files.test/a.bin',filename:'/downloads/a.bin'};
  async function capture(overrides={},method='GET') {
    for(const fn of events.headers)fn({url:item.finalUrl,tabId:1,requestId:'request-one',method,requestHeaders:[{name:'Cookie',value:'private-cookie'},{name:'Authorization',value:'private-token'},{name:'Referer',value:'https://page.test/'}]});
    await Promise.resolve();await controller.capture({...item,...overrides});
  }
  return {browser,controller,capture,item,calls,messages,saved,events};
}
test('created ACK cancels the browser only after the task exists, without a second submission',async()=>{
  const f=fixture();await f.capture();assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);assert.equal(f.messages.filter(m=>m.method==='forward').length,1);assert.equal(f.controller.jobs.get(7).state,'created');
  await f.capture();assert.equal(f.messages.length,2);
  assert.equal(JSON.stringify(f.saved).includes('private-cookie'),false);assert.equal(JSON.stringify(f.saved).includes('private-token'),false);
  assert.equal(f.messages[1].params.data.req.extra.header.Cookie,'private-cookie');
  assert.equal(f.controller.jobs.get(7).header,undefined);assert.equal(f.controller.jobs.get(7).url,undefined);
});
test('explicit host rejection resumes the original browser request',async()=>{
  const f=fixture({reply:{code:1,error:'ipc_unavailable',httpStatus:null,businessCode:null}});await f.capture();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.controller.jobs.get(7).state,'browser');
});
test('filename completion stays pending throughout confirmation and releases on explicit cancellation',async()=>{
  const f=fixture({confirm:true});await new Promise(resolve=>setImmediate(resolve));
  let releasePause,suggested=false;
  f.browser.downloads.pause=async id=>{f.calls.push(['pause',id]);await new Promise(resolve=>{releasePause=resolve;});};
  f.events.headers[0]({url:f.item.finalUrl,tabId:1,requestId:'request-one',method:'GET',requestHeaders:[]});await Promise.resolve();
  assert.equal(f.events.download[0](f.item,()=>{suggested=true;}),true);
  await new Promise(resolve=>setImmediate(resolve));assert.equal(suggested,false);
  releasePause();await new Promise(resolve=>setImmediate(resolve));
  assert.equal(suggested,false);assert.equal(f.controller.jobs.get(7).state,'waiting');
  await f.controller.finish(f.controller.jobs.get(7),'cancel');assert.equal(suggested,true);
});
test('a fully buffered fast file cannot finish before confirmation and create a browser duplicate',async()=>{
  const f=fixture({confirm:true});await new Promise(resolve=>setImmediate(resolve));
  let suggested=false;
  f.browser.downloads.search=async()=>[{id:7,state:suggested ? 'complete' : 'in_progress'}];
  f.events.headers[0]({url:f.item.finalUrl,tabId:1,requestId:'request-one',method:'GET',requestHeaders:[]});await Promise.resolve();
  assert.equal(f.events.download[0](f.item,()=>{suggested=true;}),true);
  await new Promise(resolve=>setImmediate(resolve));assert.equal(suggested,false);
  await f.controller.finish(f.controller.jobs.get(7),'submit');
  assert.equal(f.controller.jobs.get(7).state,'created');assert.equal(suggested,true);
  assert.equal(f.messages.filter(m=>m.method==='forward').length,1);
  assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);
});
test('a browser file already completed at confirmation is not reported as user cancellation',async()=>{
  const f=fixture({confirm:true});await f.capture();
  f.browser.downloads.search=async()=>[{id:7,state:'complete'}];
  await f.controller.finish(f.controller.jobs.get(7),'submit');
  assert.equal(f.controller.jobs.get(7).state,'browser');
  assert.equal(f.messages.filter(m=>m.method==='forward').length,0);
});
for(const ping of [{code:0,data:false},new Error('host disconnected')])test('preflight unavailable/disconnect cannot have submitted a task and resumes the browser',async()=>{
  const f=fixture({ping});await f.capture();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,1);
});
for(const reply of [new Error('reply lost after commit'),{code:0},message=>({code:0,httpStatus:200,businessCode:0,operationId:message.operationId,data:{code:0,operationId:'different',data:'task-one',submission:{state:'created',taskIds:['task-one']}}})])test('unknown receipt never resumes or cancels a possibly accepted download',async()=>{
  const f=fixture({reply});await f.capture();assert.deepEqual(f.calls,[['pause',7]]);assert.equal(f.controller.jobs.get(7).state,'uncertain');assert.equal(f.saved.downloadCapturePending[7].state,'uncertain');
});
test('accepted is waiting, never confused with a created task',async()=>{
  const f=fixture({reply:m=>({code:0,httpStatus:202,businessCode:0,operationId:m.operationId,data:{code:0,operationId:m.operationId,data:null,submission:{state:'accepted',taskIds:[]}}})});await f.capture();assert.equal(f.controller.jobs.get(7).state,'accepted');assert.deepEqual(f.calls,[['pause',7]]);
});
test('explicit user cancellation never calls the native host',async()=>{
  const f=fixture({confirm:true});await f.capture();assert.equal(f.controller.jobs.get(7).state,'waiting');await f.controller.finish(f.controller.jobs.get(7),'cancel');assert.equal(f.messages.length,0);assert.deepEqual(f.calls,[['pause',7],['cancel',7]]);
});
test('closing confirmation restores the browser before any app submission',async()=>{
  const f=fixture({confirm:true});await f.capture();f.events.removed[0](18);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,0);
});
test('duplicate confirmation and unknown actions cannot submit twice',async()=>{
  const f=fixture({confirm:true});await f.capture();const job=f.controller.jobs.get(7);await f.controller.finish(job,'unexpected');assert.equal(f.messages.length,0);await Promise.all([f.controller.finish(job,'submit'),f.controller.finish(job,'submit')]);assert.equal(f.messages.filter(m=>m.method==='forward').length,1);
});
test('confirmation UI failure restores the browser instead of stranding a paused file',async()=>{
  const f=fixture({confirm:true,tabFails:true});await f.capture();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,0);
});
test('a browser status query failure before confirmation restores the original request',async()=>{
  const f=fixture({searchFails:true});await f.capture();assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,0);
});
test('a browser status query failure after preflight cannot strand an unsubmitted request',async()=>{
  const f=fixture({confirm:true});await f.capture();f.browser.downloads.search=async()=>{throw Error('search');};await f.controller.finish(f.controller.jobs.get(7),'submit');assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,1);
});
test('a worker restart lets an unsubmitted confirmation resume without losing credentials or submitting',async()=>{
  const original=fixture({confirm:true});await original.capture();
  const restarted=fixture({session:original.saved});
  const request=action=>new Promise(resolve=>restarted.events.message[0]({type:'DOWNLOAD_CAPTURE',action,id:7},{url:'chrome-extension://ours/download-confirm.html?id=7',tab:{id:18}},resolve));
  assert.equal((await request('status')).job.state,'expired');
  assert.equal((await request('submit')).job.state,'expired');assert.equal(restarted.messages.length,0);
  assert.equal((await request('resume')).job.state,'browser');assert.deepEqual(restarted.calls,[['resume',7]]);
});
test('closing an unsubmitted confirmation after a worker restart restores the browser',async()=>{
  const original=fixture({confirm:true});await original.capture();const restarted=fixture({session:original.saved});restarted.events.removed[0](18);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(restarted.calls,[['resume',7]]);assert.equal(restarted.messages.length,0);
});
test('leaving an unsubmitted confirmation restores the browser, but its initial navigation does not',async()=>{
  const f=fixture({confirm:true});await f.capture();f.events.navigation[0]({tabId:18,frameId:0,url:'chrome-extension://ours/download-confirm.html?id=7'});await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(f.calls,[['pause',7]]);f.events.navigation[0]({tabId:18,frameId:0,url:'https://page.test/'});await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(f.calls,[['pause',7],['resume',7]]);assert.equal(f.messages.length,0);
});
test('default off, blob/data, POST, incognito and extension requests stay in the browser',async()=>{
  for(const [options,item,method] of [[{enabled:false},{},'GET'],[{}, {url:'blob:https://page.test/one',finalUrl:'blob:https://page.test/one'},'GET'],[{}, {},'POST'],[{}, {incognito:true},'GET'],[{}, {byExtensionId:'other'},'GET']]) {
    const f=fixture(options);await f.capture(item,method);assert.deepEqual(f.calls,[]);assert.equal(f.messages.length,0);
  }
});
for (const firstMethod of ['GET','POST']) test(`same-URL ${firstMethod}/GET requests from different tabs cannot borrow the later headers`,async()=>{
  const f=fixture();
  f.events.headers[0]({url:f.item.finalUrl,tabId:1,frameId:0,requestId:'request-A',method:firstMethod,requestHeaders:[{name:'Authorization',value:'synthetic-A'}]});
  f.events.headers[0]({url:f.item.finalUrl,tabId:2,frameId:0,requestId:'request-B',method:'GET',requestHeaders:[{name:'Authorization',value:'synthetic-B'}]});
  await Promise.resolve();await f.controller.capture(f.item);
  assert.equal(f.messages.length,0);assert.deepEqual(f.calls,[]);
});
test('a retry of the same identified request retains its final headers',async()=>{
  const f=fixture();
  for(const token of ['synthetic-A','synthetic-B']) f.events.headers[0]({url:f.item.finalUrl,tabId:1,frameId:0,requestId:'same-request',method:'GET',requestHeaders:[{name:'Authorization',value:token}]});
  await Promise.resolve();await f.controller.capture(f.item);
  assert.equal(f.messages.find(m=>m.method==='forward').params.data.req.extra.header.Authorization,'synthetic-B');
});
test('cache overflow cannot erase ambiguity and then attach a different request',async()=>{
  const f=fixture();
  for(let i=0;i<201;i++) f.events.headers[0]({url:`https://files.test/${i}`,tabId:1,requestId:`request-${i}`,method:'GET',requestHeaders:[]});
  await Promise.resolve();await f.capture();assert.deepEqual(f.calls,[]);assert.equal(f.messages.length,0);
});
