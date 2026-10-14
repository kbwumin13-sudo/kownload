import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';
import * as lib from '../lib.js';

class Element {
  constructor(tag='div'){this.tagName=tag.toUpperCase();this.children=[];this.listeners={};this.value='';this.checked=false;this.classList={toggle(){},add(){}};this.style={};}
  append(...items){this.children.push(...items);}
  add(item){this.append(item);}
  replaceChildren(...items){this.children=items;}
  addEventListener(event,fn){this.listeners[event]=fn;}
}
async function popup({formats,kind='video',realBackground=false}={}){
  const elements=new Map(), requests=[];const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
  const info={supported:true,title:'属于这个播放器的标题',duration:65,thumbnail:'https://cdn.test/poster.jpg',formats:[{id:'v',height:720,ext:'mp4',vcodec:'h264',acodec:'none'},{id:'en',ext:'m4a',vcodec:'none',acodec:'aac',language:'en'},{id:'es',ext:'m4a',vcodec:'none',acodec:'aac',language:'es'}],subtitles:[{language:'en',name:'English',automatic:false},{language:'zh-Hans',name:'中文',automatic:true}]};
  if(formats)info.formats=formats;
  const document={body:new Element(),getElementById:get,querySelectorAll:()=>[],createElement:tag=>new Element(tag)};
  const chrome={tabs:{get:async()=>({id:1,url:'https://page.test/watch'})},storage:{local:{get:async()=>({})}},runtime:{onMessage:{addListener(){}},sendMessage:async m=>{requests.push(m);return {ok:true,data:m.type==='LIST'?[{id:'one',kind:'video',url:'https://cdn.test/opaque',mediaRequired:true}]:m.type==='INSPECT'?info:m.type==='CONFIG'?{}:m.type==='CREATE'?{taskId:'one-task'}:false};}}};
  const nativeRequests=[];
  if(realBackground){
    let handler;
    const memory={mediaStateV1:{items:{one:{id:'one',tabId:1,frameId:0,documentId:'doc',sourcePage:'https://page.test/watch',kind,url:'https://cdn.test/opaque',mediaRequired:true,header:{},capturedAt:Date.now()}},submitted:{},routes:{},topCommit:{}}};
    const event={addListener(){}};
    chrome.storage.session={get:async()=>structuredClone(memory),set:async data=>Object.assign(memory,structuredClone(data)),remove:async key=>{delete memory[key];}};
    chrome.action={setBadgeText:async()=>{},setBadgeBackgroundColor:async()=>{}};
    chrome.tabs.onRemoved=event;
    chrome.cookies={getAllCookieStores:async()=>[{id:'fixture-store',tabIds:[1]}],getAll:async()=>[]};
    chrome.webNavigation={onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event};
    chrome.webRequest=Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(name=>[name,event]));
    chrome.runtime.onMessage={addListener(fn){handler=fn;}};
    chrome.runtime.sendMessage=async m=>{requests.push(m);return m.type==='CHANGED'?undefined:new Promise(resolve=>handler(m,{},resolve));};
    chrome.runtime.sendNativeMessage=(_,m,cb)=>{
      nativeRequests.push(m);
      const data=m.params?.path==='/api/v1/media/inspect'?info:m.params?.method==='GET'&&m.params.path.startsWith('/api/v1/tasks?')?[{id:'paused-task',status:'pause',protocol:'media',meta:{req:{extra:{sourcePage:'https://page.test/watch'}}}}]:m.params?.path==='/api/v1/config'?{}:'one-task';
      cb(m.method==='wakeup'?{code:0,data:true}:{code:0,data:{code:0,data}});
    };
    vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,installNetdiskBridge:()=>{},chrome,URL,URLSearchParams});
    // The popup has its own runtime listener; do not replace the worker's handler.
    chrome.runtime.onMessage=event;
  }
  vm.runInNewContext(fs.readFileSync(new URL('../popup.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...lib,document,chrome,URL,URLSearchParams,location:{search:'?tab=1&mode=single&target=one'},Option:class extends Element{constructor(label,value){super('option');this.textContent=label;this.value=value;}}});
  await settle();await settle();return {get,requests,nativeRequests,request:chrome.runtime.sendMessage};
}

test('selected language track and explicit automatic subtitle reach exactly the single task',async()=>{
  const {get,requests}=await popup();const controls=get('formats').children.map(row=>row.children[1]);
  assert.equal(controls.length,3);
  controls[1].value='en';controls[1].listeners.change();
  controls[2].value='zh-Hans|auto';controls[2].listeners.change();
  await get('download').listeners.click();await settle();
  const creates=requests.filter(x=>x.type==='CREATE');assert.equal(creates.length,1);
  assert.equal(creates[0].target,'one');assert.equal(creates[0].options.format,'v+en');
  assert.equal(JSON.stringify(creates[0].options.subtitleLanguages),'["zh-Hans"]');assert.equal(creates[0].options.writeAutomaticSubtitles,true);assert.equal(creates[0].options.embedSubtitles,true);
  const main=get('list').children[0].children[0];assert.equal(main.children[1].children[0].textContent,'属于这个播放器的标题');
});

const unknown={id:'0',ext:'mp4',vcodec:'',acodec:'',height:0,width:0};
const audio={id:'aac',ext:'m4a',vcodec:'none',acodec:'aac'};
function choose(get,id){const select=get('formats').children[0].children[1];select.value=id;select.listeners.change();}
const taskRequests = (nativeRequests,method='POST') => nativeRequests.filter(x=>x.params?.method===method&&x.params.path.startsWith('/api/v1/tasks')).map(x=>x.params.data.req);

test('raw audio HLS with unknown codecs requires a manual format and submits video intent for a video candidate',async()=>{
  const {get,requests,nativeRequests}=await popup({formats:[unknown],realBackground:true});
  const select=get('formats').children[0].children[1];
  assert.equal(select.value,'');
  assert.match(select.children[1].textContent,/轨道未确认/);
  assert.doesNotMatch(get('status').textContent,/默认选择最佳/);
  await get('download').listeners.click();await settle();
  assert.equal(requests.filter(x=>x.type==='CREATE').length,0);
  choose(get,'0');await get('download').listeners.click();await settle();
  assert.equal(requests.find(x=>x.type==='CREATE').options.mediaKind,'video');
  assert.equal(taskRequests(nativeRequests)[0].extra.mediaKind,'video');
  assert.equal(taskRequests(nativeRequests)[0].protocol,'media');
});

test('a video candidate with only confirmed audio requires an explicit audio choice through native CREATE',async()=>{
  const {get,requests,nativeRequests}=await popup({formats:[audio],realBackground:true});
  assert.equal(get('formats').children[0].children[1].value,'');
  assert.match(get('formats').children[0].children[1].children[1].textContent,/仅音频/);
  await get('download').listeners.click();await settle();
  assert.equal(taskRequests(nativeRequests).length,0);
  choose(get,'aac');assert.equal(get('download').textContent,'下载音频');await get('download').listeners.click();await settle();
  assert.equal(requests.find(x=>x.type==='CREATE').options.mediaKind,'audio');
  assert.equal(taskRequests(nativeRequests)[0].extra.mediaKind,'audio');
  assert.equal(taskRequests(nativeRequests)[0].extra.embedSubtitles,false);
});

test('an audio candidate defaults to confirmed audio and submits audio intent',async()=>{
  const {get,requests,nativeRequests}=await popup({formats:[unknown,audio],kind:'audio',realBackground:true});
  assert.equal(get('formats').children[0].children[1].value,'aac');
  assert.doesNotMatch(get('status').textContent,/最佳带音轨/);
  await get('download').listeners.click();await settle();
  assert.equal(requests.find(x=>x.type==='CREATE').options.mediaKind,'audio');
  assert.equal(taskRequests(nativeRequests)[0].extra.mediaKind,'audio');
});
test('an audio candidate manually choosing unknown codecs retains audio intent',async()=>{
  const format={...unknown,vcodec:'unknown',acodec:'unknown'};
  const {get,requests,nativeRequests}=await popup({formats:[format],kind:'audio',realBackground:true});
  assert.equal(get('formats').children[0].children[1].value,'');
  choose(get,'0');
  assert.equal(get('download').textContent,'下载音频');
  await get('download').listeners.click();await settle();
  assert.equal(requests.find(x=>x.type==='CREATE').options.mediaKind,'audio');
  assert.equal(taskRequests(nativeRequests)[0].extra.mediaKind,'audio');
});

test('confirmed video keeps its best paired audio default even beside an unknown high-resolution format',async()=>{
  const {get,requests,nativeRequests}=await popup({formats:[{...unknown,height:4320},{id:'v',ext:'mp4',height:1080,vcodec:'h264',acodec:'none'},audio],realBackground:true});
  assert.equal(get('formats').children[0].children[1].value,'v+aac');
  await get('download').listeners.click();await settle();
  assert.equal(requests.find(x=>x.type==='CREATE').options.mediaKind,'video');
  assert.equal(taskRequests(nativeRequests)[0].extra.mediaKind,'video');
});

for(const [format,mediaKind] of [[unknown,'video'],[audio,'audio']]){
  test(`popup UPDATE forwards explicit ${mediaKind} intent through the native request`,async()=>{
    const {get,requests,nativeRequests}=await popup({formats:[format],realBackground:true});
    await get('findTasks').listeners.click();await settle();
    get('recoverTask').value='paused-task';get('recoverTask').listeners.change();
    await get('update').listeners.click();await settle();
    assert.equal(taskRequests(nativeRequests,'PATCH').length,0);
    choose(get,format.id);await get('update').listeners.click();await settle();
    assert.equal(requests.find(x=>x.type==='UPDATE').options.mediaKind,mediaKind);
    assert.equal(taskRequests(nativeRequests,'PATCH')[0].extra.mediaKind,mediaKind);
    assert.ok(nativeRequests.some(x=>x.params?.method==='PUT'&&x.params.path==='/api/v1/tasks/paused-task/continue'));
  });
}

test('submission deduplication distinguishes video and audio intent for the same format',async()=>{
  const {request,nativeRequests}=await popup({formats:[unknown],realBackground:true});
  for(const mediaKind of ['video','audio','audio'])assert.equal((await request({type:'CREATE',tabId:1,id:'one',options:{format:'0',mediaKind}})).ok,true);
  assert.deepEqual(taskRequests(nativeRequests).map(x=>x.extra.mediaKind),['video','audio']);
});

test('subtitle metadata never enables unwanted subtitle downloading by default',async()=>{
  const {get,requests}=await popup();await get('download').listeners.click();await settle();
  const create=requests.find(x=>x.type==='CREATE');assert.equal(create.options.subtitleLanguages.length,0);assert.equal(create.options.writeAutomaticSubtitles,false);assert.equal(create.options.embedSubtitles,false);
});
