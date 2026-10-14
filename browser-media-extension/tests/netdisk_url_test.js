import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {NETDISK_PROVIDERS,detectNetdisk,netdiskSourceIdentity,sanitizeNetdiskCapture,installNetdiskBridge} from '../netdisk.js';
import {readNetdiskPage} from '../netdisk-page.js';

const cases=[
  ['mobile fragment','https://yun.139.com/shareweb/#/w/i/abc','mcloud','abc',''],
  ['mobile alias','https://caiyun.139.com/shareweb/?password=A1b2#/w/i/abc','mcloud','abc','A1b2'],
  ['mobile identity precedence','https://yun.139.com/shareweb/?shareId=wrong&pwd=A1b2#/w/i/abc','mcloud','abc','A1b2'],
  ['mobile encoded identity','https://yun.139.com/shareweb/?pass_code=A1b2#/w/i/%61bc','mcloud','abc','A1b2'],
  ['baidu init','https://pan.baidu.com/share/init?surl=abc','baidu','1abc',''],
  ['baidu alias','https://yun.baidu.com/share/init/?surl=abc&pwd=A1b2','baidu','1abc','A1b2'],
  ['baidu canonical','https://pan.baidu.com/s/1abc#pwd=A1b2','baidu','1abc','A1b2'],
  ['baidu surl starts with 1','https://pan.baidu.com/share/init?surl=1abc','baidu','11abc',''],
  ['baidu init precedence','https://pan.baidu.com/share/init?surl=abc&shareId=wrong&receive_code=A1b2','baidu','1abc','A1b2'],
  ['baidu missing surl','https://pan.baidu.com/share/init','baidu','',''],
  ['baidu existing alias','https://pan.baidu.com/share/init?shareid=1abc','baidu','1abc',''],
  ['tcloud','https://cloud.189.cn/web/share?code=abc&password=A1b2','tcloud','abc','A1b2'],
  ['aliyun','https://www.alipan.com/s/abc?pass_code=A1b2','aliyun','abc','A1b2'],
  ['xunlei','https://pan.xunlei.com/s/abc?receive_code=A1b2','xunlei','abc','A1b2'],
  ['quark','https://pan.quark.cn/s/abc?pwd=A1b2&gopeed_request=public-marker','quark','abc','A1b2'],
  ['uc','https://drive.uc.cn/s/abc?password=A1b2','uc','abc','A1b2'],
  ['123 alias','https://www.123912.com/s/abc?pwd=A1b2','123pan','abc','A1b2'],
  ['guangya','https://www.guangyapan.com/s/abc?pwd=A1b2','guangya','abc','A1b2'],
  ['mobile personal route','https://yun.139.com/drive#/w/i/abc','mcloud','',''],
  ['other provider route','https://pan.quark.cn/shareweb/#/w/i/abc','quark','',''],
  ['other provider init','https://pan.quark.cn/share/init?surl=abc','quark','init','']
];

function readPage(provider,url) {
  const window={};window.top=window;
  const writes=[];
  const result=vm.runInNewContext(`(${readNetdiskPage.toString()})(${JSON.stringify(provider)},${JSON.stringify(url)})`,{
    window,URL,location:{href:url},document:{querySelectorAll:()=>[]},
    localStorage:{length:0,getItem:()=>null,setItem:(...args)=>writes.push(args)},
    fetch:()=>{throw new Error('URL extraction must not request the network');}
  });
  assert.deepEqual(writes,[]);
  return JSON.parse(JSON.stringify(result));
}

for(const [name,url,provider,shareId,password] of cases) test(`${name}: helper, serialized page and sanitizer agree`,()=>{
  assert.deepEqual(netdiskSourceIdentity(url),{provider,shareId,password});
  const page=readPage(provider,url);
  assert.equal(page.share.shareId || '',shareId);
  assert.equal(page.share.password || '',password);
  const context={provider:detectNetdisk(url),url,browser:'chrome',instanceId:'instance',contextId:'context'};
  const capture=sanitizeNetdiskCapture({...page,share:{...page.share,...(shareId?{shareId:'forged-share'}:{}),password:'page-password'}},context);
  assert.equal(capture.share.shareId || '',shareId);
  assert.equal(capture.share.password,password || 'page-password');
});

test('existing query aliases and password precedence remain equivalent',()=>{
  for(const shareKey of ['shareId','shareid','linkID','code']) for(const passwordKey of ['pwd','password','pass_code','receive_code']) {
    const url=`https://pan.quark.cn/web?${shareKey}=abc&${passwordKey}=A1b2`;
    assert.deepEqual(netdiskSourceIdentity(url),{provider:'quark',shareId:'abc',password:'A1b2'});
    assert.deepEqual(readPage('quark',url).share,{shareId:'abc',password:'A1b2'});
  }
  const url='https://pan.quark.cn/s/abc?pwd=A1b2&password=other&shareId=wrong#pwd=third';
  assert.deepEqual(netdiskSourceIdentity(url),{provider:'quark',shareId:'abc',password:'A1b2'});
});

test('all existing page aliases keep detection and protected domains stay rejected',()=>{
  for(const provider of NETDISK_PROVIDERS) for(const host of provider.hosts) assert.equal(netdiskSourceIdentity(`https://${host}/s/abc`)?.provider,provider.id);
  for(const url of [
    'https://yun.139.com.evil.test/shareweb/#/w/i/abc',
    'https://evilyun.139.com/shareweb/#/w/i/abc',
    'https://pan.baidu.com.evil.test/share/init?surl=abc',
    'https://evilpan.baidu.com/share/init?surl=abc',
    'https://evil.test/?next=https://pan.baidu.com/share/init?surl=abc',
    'https://user:password@pan.baidu.com/share/init?surl=abc',
    'https://pan.baidu.com:8443/share/init?surl=abc',
    'file://pan.baidu.com/share/init?surl=abc',
    'https://api.alipan.com/v2/file/list'
  ]) assert.equal(netdiskSourceIdentity(url),null,url);
});

async function claimProbe(expectedURL,currentURL,{shareId=netdiskSourceIdentity(expectedURL)?.shareId}={}) {
  const provider=detectNetdisk(currentURL),messages=[],scripts=[],ports=[];
  let handler,cookieReads=0;
  const event={addListener(){}};
  const frameURL=new URL(currentURL);frameURL.hash='';
  const chrome={
    webNavigation:{onCommitted:event,onHistoryStateUpdated:event,onReferenceFragmentUpdated:event,getFrame:async()=>({url:frameURL.href,documentId:'doc-a',documentLifecycle:'active'})},
    tabs:{onRemoved:event,get:async()=>({id:7,url:currentURL})},
    storage:{local:{get:async()=>({netdiskInstanceId:'url-instance'}),set:async()=>{throw new Error('unexpected persistence');}}},
    cookies:{getAllCookieStores:async()=>[{id:'store-a',tabIds:[7]}],getAll:async()=>{cookieReads++;return [];}},
    scripting:{executeScript:async script=>{scripts.push(script);return [{frameId:0,documentId:'doc-a',result:readPage(provider.id,currentURL)}];}},
    runtime:{id:'url-extension',onMessage:{addListener:fn=>{handler=fn;}},connectNative:()=>{
      let respond;
      const port={closed:false,onMessage:{addListener:fn=>{respond=fn;}},onDisconnect:{addListener(){}},disconnect(){this.closed=true;},postMessage(message){
        messages.push(message);
        let response;
        if(message.method==='ping') response={code:0,data:true};
        else if(message.params.path.endsWith('/claim')) response={code:0,data:{code:0,data:{request:{id:'url-request',source:{provider:provider.id,url:expectedURL,shareId},expiresAt:new Date(Date.now()+60000).toISOString()},nonce:'url-nonce'}}};
        else response={code:0,data:{code:0,data:{sessionId:'url-session',source:{provider:provider.id,url:currentURL}}}};
        queueMicrotask(()=>respond(response));
      }};
      ports.push(port);return port;
    }}
  };
  installNetdiskBridge(chrome);
  const result=await new Promise(resolve=>handler({type:'NETDISK_PROBE'},{id:chrome.runtime.id,frameId:0,documentId:'doc-a',url:frameURL.href,tab:{id:7,incognito:false}},resolve));
  assert(ports.every(p=>p.closed));
  return {result,messages,scripts,cookieReads};
}

for(const [expected,current] of [
  ['https://pan.baidu.com/share/init?surl=abc','https://yun.baidu.com/s/1abc'],
  ['https://pan.baidu.com/s/1abc','https://pan.baidu.com/share/init?surl=abc'],
  ['https://yun.139.com/shareweb/#/w/i/abc','https://caiyun.139.com/shareweb/#/w/i/abc'],
  ['https://cloud.189.cn/web/share?code=abc','https://cloud.189.cn/t/abc'],
  ['https://pan.quark.cn/s/abc','https://pan.quark.cn/s/abc?gopeed_request=public-marker']
]) test(`actual bridge claim accepts equivalent URLs: ${expected}`,async()=>{
  const probe=await claimProbe(expected,current);
  assert.deepEqual(probe.result,{state:'synced',count:0});
  assert.equal(probe.scripts.length,1);
  assert.deepEqual(probe.messages.filter(m=>m.method==='forward').map(m=>m.params.path),['/api/v1/netdisk/browser/claim','/api/v1/netdisk/browser/capture']);
  const capture=probe.messages.at(-1).params.data.capture;
  assert.equal(capture.share.shareId,netdiskSourceIdentity(expected).shareId);
});

test('claim derives expected URL identity when native shareId is empty',async()=>{
  const probe=await claimProbe('https://pan.baidu.com/share/init?surl=abc','https://pan.baidu.com/s/1abc',{shareId:''});
  assert.deepEqual(probe.result,{state:'synced',count:0});
});

test('actual bridge rejects different mobile hashes, Baidu surls and forged expected identities before extraction',async()=>{
  for(const [expected,current,options] of [
    ['https://yun.139.com/shareweb/#/w/i/abc','https://yun.139.com/shareweb/#/w/i/other',{}],
    ['https://yun.139.com/shareweb/#/w/i/abc','https://yun.139.com/shareweb/#/w/i/other',{shareId:''}],
    ['https://pan.baidu.com/share/init?surl=abc','https://pan.baidu.com/share/init?surl=other',{}],
    ['https://pan.baidu.com/share/init?surl=abc','https://pan.baidu.com/s/1other',{shareId:'1other'}]
  ]) {
    const probe=await claimProbe(expected,current,options);
    assert.match(probe.result.error,/来源页面/);
    assert.equal(probe.scripts.length,0);assert.equal(probe.cookieReads,0);
    assert(!probe.messages.some(m=>m.params?.path.endsWith('/capture')));
  }
});
