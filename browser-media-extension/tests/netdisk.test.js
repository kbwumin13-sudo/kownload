import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import fs from 'node:fs';
import {setImmediate as settle} from 'node:timers/promises';
import {NETDISK_PROVIDERS,detectNetdisk,cleanNetdiskFiles,sanitizeNetdiskCapture,installNetdiskBridge} from '../netdisk.js';
import {readNetdiskPage} from '../netdisk-page.js';
import * as mediaLib from '../lib.js';

const file={id:'selected',name:'selected.txt',size:42,isDir:false};
const page='https://pan.quark.cn/s/share-a';
function extract(provider,{nodes={},storage={},url,globals={}}={}) {
  const reads=[],writes=[];
  const document={querySelectorAll:s=>nodes[s] || []};
  const localStorage={length:Object.keys(storage).length,key:i=>Object.keys(storage)[i],getItem:k=>{reads.push(k);return storage[k] ?? null;},setItem:(...args)=>writes.push(args)};
  const window={...globals};window.top=window;
  const result=vm.runInNewContext(`(${readNetdiskPage.toString()})(${JSON.stringify(provider)},${JSON.stringify(url || 'https://'+NETDISK_PROVIDERS.find(p=>p.id===provider).hosts[0]+'/s/share-a')})`,{window,document,localStorage,URL,location:{href:url || 'https://'+NETDISK_PROVIDERS.find(p=>p.id===provider).hosts[0]+'/s/share-a'}});
  return {result:result && JSON.parse(JSON.stringify(result)),reads,writes};
}
const fiber=props=>({__reactFiber$test:{return:{memoizedProps:props}}});
const fixtures=[
  ['baidu','.file-list',{__vue__:{allFileList:[{fs_id:10,server_filename:'selected.txt',size:42,isdir:0,selected:true},{fs_id:11,server_filename:'other.txt',size:7,isdir:0,selected:false}]}},'10'],
  ['aliyun','[class^="node-list-table-view--"]',fiber({dataSource:[{fileId:'selected',name:'selected.txt',size:42,type:'file',driveId:'drive-a'},{fileId:'other',name:'other.txt',size:7,type:'file'}],selectedKeys:'selected'}),'selected'],
  ['mcloud','.main_file_list',{__vue__:{selectList:[{item:{contentID:'selected',contentName:'selected.txt',contentSize:42}}],allFileList:[{contentID:'other',contentName:'other.txt'}]}},'selected'],
  ['tcloud','.c-file-list',{__vue__:{selectedList:[{id:'selected',name:'selected.txt',size:42,isFolder:false}],fileList:[{id:'other',name:'other.txt',size:7}]}},'selected'],
  ['xunlei','[class*="SourceListItem__item--"]',{__vue__:{info:{id:'selected',name:'selected.txt',size:'42',kind:'drive#file'},selected:['selected']}},'selected'],
  ['quark','.file-list',fiber({list:[{fid:'selected',file_name:'selected.txt',size:42,dir:false,share_fid_token:'per-file-secret'},{fid:'other',file_name:'other.txt',size:7}],selectedRowKeys:['selected'],stoken:'share-secret'}),'selected'],
  ['uc','.file-list',{__reactProps$test:{list:[{fid:'selected',file_name:'selected.txt',size:42,dir:false},{fid:'other',file_name:'other.txt',size:7}],selectedRowKeys:['selected'],stoken:'share-secret'}},'selected'],
  ['123pan','.ant-table-wrapper',fiber({dataSource:[{FileId:123,FileName:'selected.txt',Size:42,Type:0,Etag:'hash',S3KeyFlag:'key-a',StorageNode:'node-a'},{FileId:124,FileName:'other.txt',Size:7,Type:0}],rowSelection:{selectedRowKeys:[123]}}),'123'],
  ['guangya','.swangpan-file-list__root[data-ui="file-list"]',fiber({dataSource:[{fileId:'selected',fileName:'selected.txt',fileSize:42,fileType:0},{fileId:'other',fileName:'other.txt',fileSize:7}],selectedItems:['selected']}),'selected']
];
for(const [id,selector,node,expected] of fixtures) test(`${id}: actual selection layout maps only selected resources`,()=>{
  const {result,writes}=extract(id,{nodes:{[selector]:[node]}});
  assert.equal(result.files.length,1);assert.equal(result.files[0].id,expected);
  assert.equal(result.files[0].size,42);assert.equal(result.files[0].name,'selected.txt');assert.equal(result.files[0].isDir,false);
  assert.deepEqual(writes,[]);
});
test('all nine provider IDs and supported domain aliases are exact; spoofed domains rejected',()=>{
  assert.deepEqual(NETDISK_PROVIDERS.map(p=>p.id),['baidu','aliyun','mcloud','tcloud','xunlei','quark','uc','123pan','guangya']);
  for(const provider of NETDISK_PROVIDERS) for(const host of provider.hosts) assert.equal(detectNetdisk('https://'+host+'/')?.id,provider.id);
  for(const url of ['https://pan.quark.cn.attacker.test/','https://attacker.test/?site=pan.baidu.com','https://www.alipan.com:8443/','https://evil@pan.baidu.com/','file:///pan.baidu.com','https://api.guangyapan.com/']) assert.equal(detectNetdisk(url),null);
  assert.equal(detectNetdisk('https://www.123912.com/')?.id,'123pan');
});
test('unselected single-file sharing view never becomes an automatic selection',()=>{
  const {result}=extract('123pan',{nodes:{'.ant-table-wrapper':[fiber({files:[{FileId:1,FileName:'other.txt',Size:7}],file:{FileId:1,FileName:'other.txt',Size:7,S3KeyFlag:'key'}})]}});
  assert.deepEqual(result.files,[]);
});
test('DOM fallback requires selection; Vue3 proxy and mobile file/directory indices are supported',()=>{
  const selector='[data-file-id][aria-selected="true"], [data-fid][aria-selected="true"], [data-file-id].selected, [data-fid].selected, [data-file-id]:has(input[type="checkbox"]:checked)';
  assert.equal(extract('uc',{nodes:{[selector]:[{dataset:{fid:'selected',fileName:'selected.txt',size:'42'}}]}}).result.files[0].id,'selected');
  const result=extract('mcloud',{nodes:{'.home-page':[{__vueParentComponent:{proxy:{_computedWatchers:{fileList:{value:[{contentID:'other',contentName:'other.txt',contentSize:7},{contentID:'selected',contentName:'selected.txt',contentSize:42}]},dirList:{value:[{catalogID:'dir',catalogName:'目录'}]}},selectedFile:[1],selectedDir:[0]}}}]}}).result;
  assert.deepEqual(result.files.map(f=>[f.id,f.isDir]),[['selected',false],['dir',true]]);
});
test('MAIN reads only known auth fields, no refresh token, cookie access or unrelated storage',()=>{
  for(const id of ['xunlei','guangya']) {
    const {result,reads,writes}=extract(id,{storage:{credentials_client:JSON.stringify({access_token:'auth-secret',token_type:'Bearer',refresh_token:'refresh-secret',user_id:'user-a',unrelated:'noise'}),captcha_client:JSON.stringify({captcha_token:'captcha-secret',huge:'noise'}),deviceid:JSON.stringify('0123456789abcdef0123456789abcdef,web'),unrelated:'unrelated-secret'}});
    assert.equal(result.tokens.access_token,'auth-secret');assert.equal(result.tokens.captcha_token,'captcha-secret');
    assert.equal(result.tokens.deviceid,'0123456789abcdef0123456789abcdef');assert(!reads.includes('unrelated'));assert.deepEqual(writes,[]);
    assert(!JSON.stringify(result).includes('refresh-secret'));assert(!JSON.stringify(result).includes('noise'));
  }
  const {result}=extract('aliyun',{storage:{token:JSON.stringify({access_token:'ali-secret',token_type:'Bearer',resource_drive_id:'drive-a',refresh_token:'refresh-secret'})}});
  assert.equal(result.tokens.access_token,'ali-secret');assert.equal(result.tokens.resource_drive_id,'drive-a');assert(!JSON.stringify(result).includes('refresh-secret'));
  assert.equal(extract('tcloud',{storage:{accessToken:'"t-secret"'}}).result.tokens.accessToken,'t-secret');
  assert.equal(extract('123pan',{storage:{authorToken:'"123-secret"',LoginUuid:'"uuid"'}}).result.tokens.authorToken,'123-secret');
});
test('sanitizer drops URLs, auth headers, cookies and extra fields; per-file share token stays session-only',()=>{
  const context={provider:detectNetdisk(page),url:page,browser:'chrome',instanceId:'instance',contextId:'context'};
  const raw={provider:'quark',sourcePage:page,files:[{...file,url:'https://secret.test/',meta:{share_fid_token:'per-file-secret',Authorization:'secret',url:'https://secret.test/',driveId:'drive-a'}}],tokens:{stoken:'share-secret',arbitrary:'bad'},cookies:[{value:'bad-cookie'}],headers:{Authorization:'bad-auth'},share:{shareId:'attacker-share',password:'abcd'}};
  const capture=sanitizeNetdiskCapture(raw,context);
  assert.equal(capture.share.shareId,'share-a');assert.deepEqual(capture.headers,{Referer:page});assert.equal(capture.cookies,undefined);
  assert.equal(capture.files[0].meta.share_fid_token,'per-file-secret');
  assert(!JSON.stringify(cleanNetdiskFiles(capture.files)).includes('per-file-secret'));
  assert(!JSON.stringify(capture).includes('bad-auth'));assert(!JSON.stringify(capture).includes('bad-cookie'));assert(!JSON.stringify(capture).includes('secret.test'));
});

function browser({running=true,claimed=true,files=[file],error,hook,timeoutMs=2000,install=true,frameUrl}={}) {
  const events={},handlers=[],messages=[],queries=[],scripts=[],ports=[],memory={},logs=[];
  let current={id:7,url:page,documentId:'doc-a'},reads=0;
  const event=key=>({addListener(fn){(events[key] ||= []).push(fn);}});
  const sender={id:'extension-id',url:page,tab:{id:7,url:page},frameId:0,documentId:'doc-a'};
  const emit=(key,arg)=>{for(const fn of events[key] || []) fn(arg);};
  const chrome={
    storage:{local:{get:async()=>({...memory}),set:async value=>Object.assign(memory,value)}},
    tabs:{get:async()=>({...current}),onRemoved:event('removed')},
    webNavigation:{getFrame:async()=>({url:frameUrl || current.url,documentId:current.documentId}),onCommitted:event('commit'),onHistoryStateUpdated:event('spa'),onReferenceFragmentUpdated:event('hash')},
    cookies:{getAllCookieStores:async()=>[{id:'store-a',tabIds:[7]},{id:'store-other',tabIds:[9]}],getPartitionKey:async()=>({partitionKey:{topLevelSite:'https://quark.cn',hasCrossSiteAncestor:false}}),getAll:async query=>{
      reads++;queries.push(query);
      return [{name:'SID',value:'cookie-secret',domain:'.quark.cn',path:'/api',secure:true,storeId:'store-a',expirationDate:2000000000,...(query.partitionKey?{partitionKey:query.partitionKey}:{})},{name:'BAD',value:'other-cookie',domain:'.attacker.test',path:'/',storeId:'store-a'},{name:'OTHER',value:'other-store-secret',domain:'.quark.cn',path:'/',storeId:'store-other'}];
    }},
    scripting:{executeScript:async args=>{scripts.push(args);if(hook) await hook('extract',api);return [{frameId:0,documentId:'doc-a',result:{provider:'quark',sourcePage:args.args[1],files,tokens:{stoken:'token-secret'},share:{shareId:'share-a'}}}];}},
    runtime:{id:'extension-id',onMessage:{addListener(fn){handlers.push(fn);}},connectNative:()=>{
      const callbacks=[],disconnects=[];
      const port={closed:false,disconnect(){this.closed=true;},onMessage:{addListener:fn=>callbacks.push(fn)},onDisconnect:{addListener:fn=>disconnects.push(fn)},postMessage(message){
        messages.push(structuredClone(message));
        if(hook?.(message.method==='forward'?message.params.path:message.method,api)==='hang') return;
        queueMicrotask(()=>{
          if(this.closed) return;
          let response;
          if(error) response={code:1,message:error};
          else if(message.method==='ping') response={code:0,data:running};
          else if(message.method==='forward') response={code:0,data:{code:0,data:message.params.path.endsWith('/claim') ? claimed? typeof claimed==='object'?claimed:{request:{id:'request-a',source:{provider:'quark',url:page,shareId:'share-a'},expiresAt:new Date(Date.now()+60000).toISOString()},nonce:'claim-secret'}:null : {sessionId:'session-a',source:{provider:'quark',url:page},files:[file,{id:'unselected',name:'extra.txt',size:7,isDir:false}]}}};
          else response={code:0};
          for(const callback of callbacks) callback(response);
        });
      }};
      ports.push(port);return port;
    }}
  };
  if(install) installNetdiskBridge(chrome,{timeoutMs});
  const api={sender,chrome,events,messages,queries,scripts,ports,memory,logs,get reads(){return reads;},navigate(url=page+'/changed',kind='spa',documentId='doc-b'){
    current={...current,url,documentId};emit(kind,{tabId:7,frameId:0,url,documentId});
  },send(type='NETDISK_PROBE',customSender=sender,extra={}){
    return new Promise(resolve=>{for(const handler of handlers) handler({type,...extra},customSender,resolve);});
  }};
  return api;
}
test('passive ping while App stopped does not wake, claim, collect secrets or leave a native port',async()=>{
  const b=browser({running:false});assert.deepEqual(await b.send(),{state:'idle'});
  assert.deepEqual(b.messages.map(m=>m.method),['ping']);assert.equal(b.reads,0);assert.equal(b.scripts.length,0);assert(b.ports.every(p=>p.closed));
  assert.deepEqual(b.memory,{});
  const stopped=browser({error:'dial unix socket: not running'});assert.deepEqual(await stopped.send(),{state:'idle'});assert.equal(stopped.scripts.length,0);assert.equal(stopped.reads,0);
});
test('no pending claim does not wake or extract; claimed session is captured without native create',async()=>{
  const idle=browser({claimed:false});assert.deepEqual(await idle.send(),{state:'idle'});assert.equal(idle.reads,0);assert.equal(idle.scripts.length,0);
  const b=browser();assert.deepEqual(await b.send(),{state:'synced',count:1});
  assert.deepEqual(b.messages.map(m=>m.method),['ping','forward','forward']);
  assert.deepEqual(b.messages.filter(m=>m.method==='forward').map(m=>m.params.path),['/api/v1/netdisk/browser/claim','/api/v1/netdisk/browser/capture']);
  assert.equal(b.messages[2].params.data.nonce,'claim-secret');assert.equal(b.messages[2].params.data.requestId,'request-a');
  assert.equal(b.scripts[0].world,'MAIN');assert.deepEqual(b.scripts[0].target,{tabId:7,documentIds:['doc-a']});
  assert.deepEqual(b.scripts[0].args,['quark',page]);assert(!JSON.stringify(b.scripts[0]).includes('claim-secret'));
  assert(b.ports.every(p=>p.closed));
});
test('explicit click wakes and submits selected-only session to non-silent Flutter Native create',async()=>{
  const b=browser();assert.deepEqual(await b.send('NETDISK_CLICK'),{state:'confirm',count:1});
  assert.deepEqual(b.messages.map(m=>m.method),['wakeup','forward','create']);
  const capture=b.messages[1].params.data.capture,create=b.messages[2];
  assert.equal(capture.tokens.stoken,'token-secret');assert.equal(capture.cookies[0].value,'cookie-secret');assert.equal(capture.cookies[0].expires,2000000000);
  assert.equal(create.meta.silent,false);assert.equal(create.params.req.protocol,'netdisk');assert.equal(create.params.req.extra.sessionId,'session-a');
  assert.deepEqual(create.params.req.extra.files,[file]);assert.equal(create.params.req.url,page);assert.deepEqual(create.params.opts,{});
  assert(!JSON.stringify(create).includes('token-secret'));assert(!JSON.stringify(create).includes('cookie-secret'));
  assert(b.queries.every(q=>q.storeId==='store-a'));assert.equal(b.queries.filter(q=>q.partitionKey).length,1);
  assert(!JSON.stringify(b.memory).includes('secret'));assert(b.ports.every(p=>p.closed));
});
test('empty selection blocks capture and create; passive auth sync can have zero selected files',async()=>{
  const click=browser({files:[]});assert.match((await click.send('NETDISK_CLICK')).error,/勾选/);assert.equal(click.reads,0);assert.equal(click.messages.length,1);
  const passive=browser({files:[]});assert.equal((await passive.send()).state,'synced');
});
test('sender identity, top frame, current tab URL, document and site must all match',async()=>{
  for(const change of [{id:'other-extension'},{frameId:1},{documentLifecycle:'cached'},{documentId:'stale-doc'},{url:'https://attacker.test/'},{url:'https://pan.quark.cn/s/other-share'},{tab:{id:99}}]) {
    const b=browser();const result=await b.send('NETDISK_CLICK',{...b.sender,...change});
    assert.match(result.error,/来源页面|登录状态/);assert.equal(b.messages.length,0);assert.equal(b.reads,0);
  }
});
test('messages cannot proxy arbitrary API paths, cookies, scripts, tabs or secret captures',async()=>{
  for(const extra of [{path:'/tasks'},{cookies:[]},{tabId:9},{sourcePage:'https://other.test/'},{capture:{}},{nonce:'injected'}]) {
    const b=browser();assert.match((await b.send('NETDISK_PROBE',b.sender,extra)).error,/无效/);assert.equal(b.messages.length,0);
  }
  const b=browser();assert.match((await b.send('NETDISK_FORWARD')).error,/无效/);
});
test('SPA, fragment and full navigation during extraction invalidate session and close native port',async()=>{
  for(const kind of ['spa','hash','commit']) {
    const b=browser({hook:(stage,api)=>{if(stage==='extract') api.navigate(page+'/other',kind);}});
    assert.match((await b.send()).error,/来源页面/);assert.equal(b.reads,0);assert.equal(b.messages.filter(m=>m.params?.path==='/api/v1/netdisk/browser/capture').length,0);assert(b.ports.every(p=>p.closed));
  }
});
test('navigation after capture never submits Native create for a stale page',async()=>{
  const b=browser({hook:(stage,api)=>{if(stage==='/api/v1/netdisk/browser/capture') api.navigate();}});
  assert.match((await b.send('NETDISK_CLICK')).error,/来源页面/);assert(!b.messages.some(m=>m.method==='create'));assert(b.ports.every(p=>p.closed));
});
test('native timeout and raw Native error are bounded, close ports and expose no secrets',async()=>{
  const b=browser({hook:stage=>stage==='ping'?'hang':undefined,timeoutMs:10});
  assert.match((await b.send()).error,/超时/);assert(b.ports.every(p=>p.closed));
  const failed=browser({error:'Authorization: Bearer token-secret Cookie: cookie-secret URL https://secret.test/'});
  const output=JSON.stringify(await failed.send('NETDISK_CLICK'));
  assert(!output.includes('secret'));assert(!JSON.stringify(failed.memory).includes('secret'));assert(failed.ports.every(p=>p.closed));
});
test('claim from a different provider, browser instance, share or expired request is rejected before extraction',async()=>{
  const valid={id:'request-a',source:{provider:'quark',url:page,shareId:'share-a'},expiresAt:new Date(Date.now()+60000).toISOString()};
  for(const changed of [{source:{...valid.source,provider:'uc'}},{source:{...valid.source,shareId:'share-b'}},{source:{provider:'quark',url:'https://pan.quark.cn/different'}},{expiresAt:'invalid'},{expiresAt:'2000-01-01T00:00:00Z'},{instanceId:'different-instance'}]) {
    const b=browser({claimed:{request:{...valid,...changed},nonce:'claim-secret'}});
    assert.match((await b.send()).error,/来源页面/);assert.equal(b.reads,0);assert.equal(b.scripts.length,0);assert(b.ports.every(p=>p.closed));
  }
});
test('missing store or partition never falls back to another profile; errors are secret-free',async()=>{
  for(const mutate of [b=>{b.chrome.cookies.getAllCookieStores=async()=>[{id:'other',tabIds:[8]}];},b=>{b.chrome.cookies.getPartitionKey=async()=>{throw new Error('token-secret');};}]) {
    const b=browser();mutate(b);const output=await b.send('NETDISK_CLICK');
    assert(output.error);assert(!JSON.stringify(output).includes('token-secret'));assert.equal(b.reads,0);
    assert(!b.messages.some(m=>m.params?.path==='/api/v1/netdisk/browser/capture'));assert(b.ports.every(p=>p.closed));
  }
});
test('timeout during MAIN extraction settles task immediately and late result cannot collect cookies',async()=>{
  let release;
  const b=browser({timeoutMs:10,hook:stage=>stage==='extract'?new Promise(resolve=>{release=resolve;}):undefined});
  assert.match((await b.send()).error,/超时/);assert.equal(b.reads,0);assert(b.ports.every(p=>p.closed));
  release();await settle();assert.equal(b.reads,0);assert(!b.messages.some(m=>m.params?.path==='/api/v1/netdisk/browser/capture'));
});
test('overlapping probes do not open multiple native collection ports',async()=>{
  let release;
  const b=browser({hook:stage=>stage==='extract'?new Promise(resolve=>{release=resolve;}):undefined});
  const request=b.send();
  while(!release) await settle();
  assert.deepEqual(await b.send(),{state:'busy'});assert.equal(b.ports.length,1);
  release();assert.equal((await request).state,'synced');
});
test('cookie read timeout cancels subsequent partition queries and never captures late credentials',async()=>{
  let release;
  const b=browser({timeoutMs:10});
  b.chrome.cookies.getAll=query=>{b.queries.push(query);return new Promise(resolve=>{release=resolve;});};
  assert.match((await b.send('NETDISK_CLICK')).error,/超时/);assert.equal(b.queries.length,1);assert(b.ports.every(p=>p.closed));
  release([{name:'SID',value:'cookie-secret',domain:'.quark.cn',path:'/'}]);await settle();
  assert.equal(b.queries.length,1);assert(!b.messages.some(m=>m.params?.path==='/api/v1/netdisk/browser/capture'));
});
test('real background module shares listeners with media without handling netdisk messages twice',async()=>{
  const b=browser({install:false});
  b.chrome.storage.session={get:async()=>({}),set:async()=>{},remove:async()=>{}};
  b.chrome.action={setBadgeBackgroundColor:async()=>{},setBadgeText:async()=>{}};
  b.chrome.webRequest=Object.fromEntries(['onBeforeRequest','onBeforeSendHeaders','onHeadersReceived','onCompleted','onErrorOccurred'].map(key=>[key,{addListener(){}}]));
  vm.runInNewContext(fs.readFileSync(new URL('../background.js',import.meta.url),'utf8').replace(/^import .*;\n/gm,''),{...mediaLib,installNetdiskBridge,chrome:b.chrome,URL,URLSearchParams});
  assert.equal((await b.send('NETDISK_CLICK')).state,'confirm');
  assert.deepEqual(b.messages.map(m=>m.method),['wakeup','forward','create']);assert.equal(b.ports.length,1);assert(b.ports[0].closed);
});
test('committed React selection wins over pending props, unselected rows and unused globals',()=>{
  const node={__reactFiber$test:{return:{memoizedProps:{list:[{fid:'selected',file_name:'selected.txt',size:42},{fid:'other',file_name:'other.txt',size:7,checked:true}],selectedRowKeys:['selected']},pendingProps:{list:[{fid:'old',file_name:'old.txt',size:7}],selectedRowKeys:['old']}}}};
  assert.deepEqual(extract('quark',{nodes:{'.file-list':[node]}}).result.files.map(f=>f.id),['selected']);
  let calls=0;
  const {result}=extract('baidu',{globals:{require:key=>{assert.equal(key,'system-core:context/context.js');calls++;return {instanceForSystem:{list:{getSelected:()=>[{fs_id:1,server_filename:'selected.txt',size:42}]}}};}}});
  assert.equal(calls,1);assert.equal(result.files[0].id,'1');
});
test('isolated bootstrap rejects synthetic clicks, forwards only fixed messages and restores from bfcache',async()=>{
  const nodes=[],messages=[],windowEvents={},documentEvents={},timers=new Set();
  class Element {
    constructor(tag){this.tag=tag;this.style={};this.events={};this.children=[];nodes.push(this);}
    setAttribute(){}
    append(...children){this.children.push(...children);children.forEach(c=>{c.isConnected=true;});}
    attachShadow(){return new Element('shadow');}
    addEventListener(type,fn){this.events[type]=fn;}
  }
  const body=new Element('body');
  const document={body,visibilityState:'visible',createElement:tag=>new Element(tag),addEventListener:(k,fn)=>{documentEvents[k]=fn;}};
  const window={addEventListener:(k,fn)=>{windowEvents[k]=fn;}};window.top=window;
  vm.runInNewContext(fs.readFileSync(new URL('../netdisk-content.js',import.meta.url),'utf8'),{window,document,chrome:{runtime:{sendMessage:async msg=>{messages.push(msg);return {state:'confirm',count:1};}}},MutationObserver:class{observe(){}disconnect(){}},setInterval:fn=>{timers.add(fn);return fn;},clearInterval:fn=>timers.delete(fn)});
  await settle();assert.deepEqual(JSON.parse(JSON.stringify(messages)),[{type:'NETDISK_PROBE'}]);
  const button=nodes.find(n=>n.tag==='button');
  await button.events.click({isTrusted:false});assert.equal(messages.length,1);
  await button.events.click({isTrusted:true});assert.deepEqual(JSON.parse(JSON.stringify(messages[1])),{type:'NETDISK_CLICK'});
  assert(!windowEvents.message);assert(!documentEvents.message);assert.equal(timers.size,0);
  windowEvents.pagehide();assert.equal(timers.size,0);
  windowEvents.pageshow({persisted:true});await settle();assert.equal(timers.size,0);
  windowEvents.pagehide();assert.equal(timers.size,0);
});
test('manifest fixes identity/version, top document_start isolated bootstrap; media capture unchanged',()=>{
  const manifest=JSON.parse(fs.readFileSync(new URL('../manifest.json',import.meta.url),'utf8'));
  assert.equal(manifest.version,'0.4.0');assert.equal(manifest.name,'kownload 下载助手');assert(manifest.key.startsWith('MIIBIjAN'));
  const script=manifest.content_scripts.find(s=>s.js.includes('netdisk-content.js'));
  assert.equal(script.all_frames,false);assert.equal(script.run_at,'document_start');assert.equal(script.world,'ISOLATED');
  assert.equal(manifest.content_scripts.find(s=>s.js.includes('content.js')).all_frames,true);
});

test('committed sender/frame URL may omit the current hash route without changing the document',async()=>{
 const b=browser({frameUrl:page});
 b.navigate(page+'#/list/share','hash','doc-a');
 assert.deepEqual(await b.send(),{state:'synced',count:1});
 assert.equal(b.scripts[0].args[1],page+'#/list/share');
 assert.equal(b.messages.find(m=>m.params?.path==='/api/v1/netdisk/browser/capture').params.data.capture.sourcePage,page+'#/list/share');
});
