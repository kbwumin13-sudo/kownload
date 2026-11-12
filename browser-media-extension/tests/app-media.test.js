import test from 'node:test';
import assert from 'node:assert/strict';
import {setTimeout as delay} from 'node:timers/promises';
import {createAppMediaBridge} from '../app-media.js';

const page='https://page.test/watch';
const image={id:'a',kind:'image',url:'https://cdn.test/a.jpg?signature=fixture',variants:['https://cdn.test/original.jpg'],width:640,height:480};
const video={id:'a',kind:'video',url:'https://cdn.test/master.m3u8',manifest:true,mediaRequired:true};
const formats=[{id:'v',ext:'mp4',vcodec:'h264',acodec:'none',height:1080},{id:'en',ext:'m4a',vcodec:'none',acodec:'aac',language:'en'},{id:'es',ext:'m4a',vcodec:'none',acodec:'aac',language:'es'}];
async function until(predicate){for(let i=0;i<100;i++){if(predicate())return;await delay(2);}assert.fail('condition timed out');}
async function fixture(t,{mode='all',items=[structuredClone(image)],info={supported:true,formats,subtitles:[{language:'zh',name:'中文',automatic:true}]},previewFails=false,callOverride}={}){
  const f={items,commands:[],calls:[],requests:[],results:[],leases:0,releases:0,wakes:0,frame:{documentId:'doc',url:page},settings:{sniffing:{enabled:true,disabledSites:{}}},disconnect:false};
  const browser={webNavigation:{getFrame:async()=>f.frame}};
  const appBridge={wake:async()=>{f.wakes++;},acquire:()=>{f.leases++;let released=false;return()=>{assert.equal(released,false);released=true;f.releases++;};},identity:async()=>({clientId:'instance',browser:'chrome'}),settings:async()=>({settings:f.settings,revision:1}),request:async(action,params)=>{
    f.requests.push({action,params});if(f.disconnect)throw Error('disconnected');
    if(action==='mediaPoll')return{command:f.commands.shift()||null};
    if(action==='mediaResult')f.results.push(params);
    return{opened:true};
  }};
  const call=async msg=>{f.calls.push(structuredClone(msg));if(callOverride){const result=await callOverride(msg,f);if(result!==undefined)return result;}
    if(msg.type==='LIST')return f.items;
    if(msg.type==='INSPECT')return info;
    if(msg.type==='CREATE')return{state:'created',taskIds:['task-one'],taskId:'task-one'};
    if(msg.type==='UPDATE')return{taskId:msg.options.taskId};
    if(msg.type==='RECOVERABLE')return[{id:'paused',name:'paused',status:'pause'}];
    return{started:true};
  };
  f.bridge=createAppMediaBridge(browser,appBridge,{call,preview:async(item,url)=>{if(previewFails)throw Error('preview unavailable');return{previewUrl:url,previewHeaders:{Referer:page,Cookie:'preview-fixture-only'}};},pollMs:1});
  t.after(()=>f.bridge.invalidate(1));
  f.scope={tabId:1,mode,frameId:0,documentId:'doc',sourcePage:page,...(mode==='single'?{target:'a'}:{}),...(mode==='identify'?{playerId:'clicked-player'}:{})};
  f.open=()=>f.bridge.open(f.scope);
  f.send=async command=>{const id=`command-${f.commands.length}-${f.results.length}`;f.commands.push({id,...command});await until(()=>f.results.some(r=>r.commandId===id)||f.releases>0);return f.results.find(r=>r.commandId===id);};
  return f;
}

test('explicit open presents complete candidates and private previews without a second settings source',async t=>{
 const f=await fixture(t);assert.equal(f.leases,0);assert.equal(f.requests.length,0);await f.open();
 const open=f.requests.find(r=>r.action==='mediaOpen').params;
 assert.equal(open.clientId,'instance');assert.equal(open.browser,'chrome');assert.equal(open.view,'all');
 assert.equal(open.items[0].displayUrl,image.url);assert.equal(open.items[0].previewHeaders.Cookie,'preview-fixture-only');
 assert.equal(open.items[0].header,undefined);assert.equal(f.wakes,1);assert.equal(f.leases,1);
 await f.bridge.invalidate(1);await f.bridge.invalidate(1);assert.equal(f.releases,1);
});
test('preview failures preserve the complete download list',async t=>{
 const f=await fixture(t,{previewFails:true,items:[structuredClone(image),{...image,id:'b'}]});await f.open();
 const items=f.requests.find(r=>r.action==='mediaOpen').params.items;assert.equal(items.length,2);assert.equal(items[0].previewUrl,undefined);
});
test('App command scope and request-header forgeries cannot override the frozen browser binding',async t=>{
 const f=await fixture(t,{mode:'single'});await f.open();
 const result=await f.send({type:'CREATE',itemId:'a',tabId:999,target:'other',documentId:'forged',options:{url:'https://evil.test',imageUrl:'https://evil.test',header:{Cookie:'bad'},name:'kept'}});
 assert.equal(result.ok,true);const create=f.calls.find(c=>c.type==='CREATE');
 assert.equal(create.tabId,1);assert.equal(create.target,'a');assert.equal(create.id,'a');assert.equal(create.options.imageUrl,image.url);
 assert.equal(create.options.header,undefined);assert.equal(create.options.url,undefined);assert.equal(create.options.name,'kept');
 const wrong=await f.send({type:'CREATE',itemId:'b',options:{}});assert.equal(wrong.ok,false);assert.equal(f.calls.filter(c=>c.type==='CREATE').length,1);
});
test('path creation carries only the live App command and keeps original track selection',async t=>{
 const f=await fixture(t,{items:[video]});await f.open();
 assert.equal(f.requests.find(r=>r.action==='mediaOpen').params.pathFreezeVersion,1);
 const inspected=await f.send({type:'INSPECT',itemId:'a'});
 const reply=await f.send({type:'CREATE',itemId:'a',pathFreeze:true,
   options:{format:inspected.data.bestId,audioId:'en',path:'/owned',name:'chosen.mp4',pathIntent:{origin:'rule'},pathCreate:{sessionId:'forged',commandId:'forged'}}});
 assert.equal(reply.ok,true);
 const create=f.calls.find(c=>c.type==='CREATE');
 const open=f.requests.find(r=>r.action==='mediaOpen').params;
 assert.deepEqual(create.options.pathCreate,{sessionId:open.sessionId,commandId:reply.commandId});
 assert.equal(create.options.pathIntent,undefined);
 assert.equal(create.options.format,'v+en');
 assert.equal(create.options.path,'/owned');
 assert.equal(create.options.name,'chosen.mp4');
});
test('identify keeps player/document scope through inspect, create and enhanced search',async t=>{
 const f=await fixture(t,{mode:'identify',items:[video]});await f.open();
 assert.equal((await f.send({type:'INSPECT',itemId:'a'})).ok,true);
 const inspect=f.calls.find(c=>c.type==='INSPECT');assert.equal(inspect.identify,true);assert.equal(inspect.playerId,'clicked-player');assert.equal(inspect.documentId,'doc');assert.equal(inspect.sourcePage,page);assert.equal(inspect.target,'a');
 assert.equal((await f.send({type:'DEEP_SEARCH'})).ok,true);
 const deep=f.calls.find(c=>c.type==='DEEP_SEARCH');assert.equal(deep.playerId,'clicked-player');
});
test('navigation or document replacement closes the viewer and releases its lease without submitting',async t=>{
 const f=await fixture(t);await f.open();f.frame={documentId:'other',url:page};f.commands.push({id:'stale',type:'CREATE',itemId:'a',options:{}});
 await until(()=>f.releases===1);assert.equal(f.calls.filter(c=>c.type==='CREATE').length,0);assert.ok(f.requests.some(r=>r.action==='mediaResult'&&r.params.closed));
});
test('canonical sniffing changes apply to the next actual command without reloading',async t=>{
 const f=await fixture(t);await f.open();f.settings.sniffing.enabled=false;
 assert.equal((await f.send({type:'CREATE',itemId:'a',options:{}})).ok,false);assert.equal(f.calls.filter(c=>c.type==='CREATE').length,0);
});
test('known media options retain audio language and explicit automatic subtitles',async t=>{
 const f=await fixture(t,{items:[video]});await f.open();const inspect=await f.send({type:'INSPECT',itemId:'a'});
 assert.equal(inspect.data.bestId,'v+es');assert.equal(inspect.data.audioTracks.length,2);
 const result=await f.send({type:'CREATE',itemId:'a',options:{format:inspect.data.bestId,audioId:'en',subtitleLanguages:['zh'],writeAutomaticSubtitles:true,embedSubtitles:true}});
 assert.equal(result.ok,true);const options=f.calls.find(c=>c.type==='CREATE').options;
 assert.equal(options.format,'v+en');assert.equal(options.mediaKind,'video');assert.deepEqual(options.subtitleLanguages,['zh']);assert.equal(options.embedSubtitles,true);
});
test('unknown tracks are never chosen implicitly and explicit audio keeps audio intent',async t=>{
 const audio={id:'audio',ext:'m4a',vcodec:'none',acodec:'aac'};
 const f=await fixture(t,{items:[video],info:{supported:true,formats:[{id:'unknown',ext:'mp4',vcodec:'unknown',acodec:'unknown'},audio]}});await f.open();
 const inspected=await f.send({type:'INSPECT',itemId:'a'});assert.equal(inspected.data.bestId,'');
 assert.equal((await f.send({type:'CREATE',itemId:'a',options:{format:''}})).ok,false);
 assert.equal((await f.send({type:'CREATE',itemId:'a',options:{format:'audio',subtitleLanguages:['forged'],embedSubtitles:true}})).ok,true);
 const options=f.calls.find(c=>c.type==='CREATE').options;assert.equal(options.mediaKind,'audio');assert.equal(options.embedSubtitles,false);assert.deepEqual(options.subtitleLanguages,[]);
});
test('image variants are opaque, previewable and revalidated against the current candidate',async t=>{
 const f=await fixture(t);await f.open();const inspected=await f.send({type:'INSPECT',itemId:'a'});
 assert.equal(inspected.data.variants[1].id,'image-1');assert.equal(inspected.data.variants[1].previewUrl,image.variants[0]);
 assert.equal((await f.send({type:'CREATE',itemId:'a',options:{imageVariant:'image-1'}})).ok,true);
 assert.equal(f.calls.find(c=>c.type==='CREATE').options.imageUrl,image.variants[0]);
 f.items[0].variants=[];assert.equal((await f.send({type:'CREATE',itemId:'a',options:{imageVariant:'image-1'}})).ok,false);
});
test('duplicate App commands reuse the result without resubmitting and transport loss never replays CREATE',async t=>{
 const f=await fixture(t);await f.open();const command={id:'same',type:'CREATE',itemId:'a',options:{}};
 f.commands.push(command);await until(()=>f.results.some(r=>r.commandId==='same'));f.commands.push(command);
 await until(()=>f.results.filter(r=>r.commandId==='same').length===2);assert.equal(f.calls.filter(c=>c.type==='CREATE').length,1);
 f.disconnect=true;await until(()=>f.releases===1);assert.equal(f.calls.filter(c=>c.type==='CREATE').length,1);
});
test('CLOSE and explicit invalidation end the only active polling lease',async t=>{
 const f=await fixture(t);await f.open();f.commands.push({id:'close',type:'CLOSE'});await until(()=>f.releases===1);
 const count=f.requests.length;await delay(10);assert.equal(f.requests.length,count);
});
test('invalidation during an opening LIST cannot present a stale viewer or create a task',async t=>{
 let release,started=false;
 const f=await fixture(t,{callOverride:async(msg)=>{if(msg.type==='LIST'){started=true;return new Promise(resolve=>release=()=>resolve([structuredClone(image)]));}}});
 const opening=f.open();await until(()=>started);await f.bridge.invalidate(1);release();await assert.rejects(opening);
 assert.equal(f.releases,1);assert.equal(f.requests.some(r=>r.action==='mediaOpen'),false);
});

test('embedded players retain the top-site preference and settings label',async t=>{
 const f=await fixture(t);f.scope.site='outer.test';await f.open();
 assert.equal(f.requests.find(r=>r.action==='mediaOpen').params.site,'outer.test');
 f.settings.sniffing.disabledSites['outer.test']=true;
 assert.equal((await f.send({type:'CREATE',itemId:'a',options:{}})).ok,false);
 assert.equal(f.calls.filter(c=>c.type==='CREATE').length,0);
});
