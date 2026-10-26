import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {setImmediate as settle} from 'node:timers/promises';

const script=fs.readFileSync(new URL('../netdisk-content.js',import.meta.url),'utf8');
async function bootstrap(url,{state='capturing',hidden=false}={}) {
  let now=1000000,response={state};
  const timers=new Set(),messages=[],events={},location={href:url};
  const window={addEventListener:(key,fn)=>{events[key]=fn;}};window.top=window;
  const document={body:null,visibilityState:hidden?'hidden':'visible',addEventListener(){}};
  vm.runInNewContext(script,{
    window,document,location,URL,URLSearchParams,Date:{now:()=>now},
    MutationObserver:class{observe(){}disconnect(){}},
    chrome:{runtime:{sendMessage:async message=>{messages.push(message);return response;}}},
    setInterval:(fn,ms)=>{assert.equal(ms,15000);timers.add(fn);return fn;},
    clearInterval:fn=>timers.delete(fn)
  });
  await settle();
  return {messages,timers,events,location,document,
    respond:state=>{response={state};},
    async tick(ms=15000){now+=ms;for(const fn of [...timers]) fn();await settle();}
  };
}

for(const url of [
  'https://pan.quark.cn/list?gopeed_request=id#/list/all',
  'https://drive.uc.cn/list?gopeed_request=id#/list/all',
  'https://alipan.com/drive/file/all?gopeed_request=id',
  'https://yun.139.com/w/#/index?gopeed_request=id',
  'https://caiyun.139.com/w/#/index?tab=all&gopeed_request=id'
]) test(`App handoff retries are bounded for ${url}`,async()=>{
  const page=await bootstrap(url);
  assert.equal(page.messages.length,1);assert.equal(page.timers.size,1);
  await page.tick();assert.equal(page.messages.length,2);
  assert(page.messages.every(message=>message.type==='NETDISK_PROBE'));
  await page.tick(600001);
  assert.equal(page.timers.size,0);assert.equal(page.messages.length,2);
  await page.tick();assert.equal(page.messages.length,2);
});

test('ordinary browsing and unrelated hash fields never start polling',async()=>{
  for(const url of [
    'https://pan.quark.cn/',
    'https://yun.139.com/w/#/index',
    'https://yun.139.com/w/#/index?gopeed_request=',
    'https://yun.139.com/w/#/index?requestId=unrelated',
    'https://yun.139.com/unknown#/index?gopeed_request=id',
    'https://yun.139.com/shareweb/#/w/i/abc?gopeed_request=id',
    'https://pan.quark.cn/w/#/index?gopeed_request=id',
    'https://yun.139.com.evil.test/w/#/index?gopeed_request=id',
    'https://user@yun.139.com/w/#/index?gopeed_request=id',
    'https://yun.139.com:8443/w/#/index?gopeed_request=id',
    'file://yun.139.com/w/#/index?gopeed_request=id'
  ]) {
    const page=await bootstrap(url);
    assert.equal(page.messages.length,1,url);assert.equal(page.timers.size,0,url);
    await page.tick(700000);assert.equal(page.messages.length,1,url);
    page.events.pagehide();page.events.pageshow({persisted:true});await settle();
    assert.equal(page.messages.length,2,url);assert.equal(page.timers.size,0,url);
  }
});

test('mobile handoff polling stops on synced and across pagehide',async()=>{
  for(const state of ['synced']) {
    const page=await bootstrap('https://yun.139.com/w/#/index?gopeed_request=id');
    page.respond(state);await page.tick();
    assert.equal(page.timers.size,0);assert.equal(page.messages.length,2);
    await page.tick();assert.equal(page.messages.length,2);
  }
  const page=await bootstrap('https://yun.139.com/w/#/index?gopeed_request=id');
  page.events.pagehide();assert.equal(page.timers.size,0);
  await page.tick();assert.equal(page.messages.length,1);
  page.events.pageshow({persisted:true});await settle();
  assert.equal(page.timers.size,1);assert.equal(page.messages.length,2);
  await page.tick(600001);assert.equal(page.timers.size,0);
});

test('an idle probe during a personal-home redirect does not end the bounded handoff',async()=>{
  const page=await bootstrap('https://yun.139.com/w/#/index?gopeed_request=id',{state:'idle'});
  assert.equal(page.timers.size,1);
  await page.tick();assert.equal(page.messages.length,2);
  page.respond('synced');await page.tick();assert.equal(page.timers.size,0);
});

test('a mobile SPA can attach its correlated handoff after document_start',async()=>{
  const page=await bootstrap('https://yun.139.com/w/',{state:'idle'});
  assert.equal(page.timers.size,0);
  page.location.href='https://yun.139.com/w/#/index?gopeed_request=id';
  page.respond('capturing');page.events.hashchange();await settle();
  assert.equal(page.timers.size,1);
  await page.tick();assert.equal(page.messages.length,2);
  await page.tick(600001);assert.equal(page.timers.size,0);
  page.events.hashchange();await settle();assert.equal(page.timers.size,0);
});

test('an intermediate mobile hash retains the handoff until the router settles',async()=>{
  const page=await bootstrap('https://yun.139.com/w/#?gopeed_request=id',{state:'idle'});
  assert.equal(page.timers.size,1);
  page.location.href='https://yun.139.com/w/#/index?gopeed_request=id';
  page.respond('capturing');await page.tick();assert.equal(page.messages.length,2);
  await page.tick(600001);assert.equal(page.timers.size,0);
});

test('hidden mobile handoffs respect visibility and still expire',async()=>{
  const page=await bootstrap('https://yun.139.com/w/#/index?gopeed_request=id',{hidden:true});
  assert.equal(page.messages.length,0);assert.equal(page.timers.size,1);
  await page.tick();assert.equal(page.messages.length,0);
  page.document.visibilityState='visible';await page.tick();assert.equal(page.messages.length,1);
  page.document.visibilityState='hidden';await page.tick(600001);
  assert.equal(page.timers.size,0);assert.equal(page.messages.length,1);
});
