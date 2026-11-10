import test from 'node:test';
import assert from 'node:assert/strict';
import {createAppBridge} from '../app-bridge.js';
import {browserClientId} from '../app-identity.js';

function fixture() {
  const stored={netdiskInstanceId:'stable-client',downloadCapture:{enabled:true,confirm:true},showAll:true,disabledSites:{'one.test':true}},writes=[],messages=[];
  const f={stored,writes,messages,data:{revision:1,settings:{downloadCapture:{enabled:true,confirm:false},sniffing:{enabled:true,showAll:false,disabledSites:{'two.test':true}}}}};
  f.browser={storage:{local:{get:async()=>structuredClone(stored),set:async value=>{writes.push(structuredClone(value));Object.assign(stored,structuredClone(value));}}},permissions:{contains:async()=>true},runtime:{getManifest:()=>({version:'0.5.0'})}};
  f.send=async message=>{
    messages.push(structuredClone(message));
    if(message.method==='wakeup')return {code:0,data:true};
    if(f.failure)return {code:1,error:'ipc_unavailable'};
    return {code:0,data:{code:0,data:structuredClone(f.data)}};
  };
  f.bridge=createAppBridge(f.browser,f.send,{browserName:'edge'});
  return f;
}
test('sync migrates only known legacy preferences and caches App canonical values under the same client',async()=>{
  const f=fixture();f.stored.unrelated='keep';f.stored.token='synthetic-do-not-export';
  const settings=await f.bridge.settings();
  assert.equal(f.messages.length,1);const params=f.messages[0].params;
  assert.equal(params.action,'sync');assert.equal(params.clientId,'stable-client');assert.equal(params.browser,'edge');
  assert.deepEqual(params.legacySettings,{downloadCapture:{enabled:true,confirm:true},showAll:true,disabledSites:{'one.test':true}});
  assert.equal(JSON.stringify(params).includes('synthetic-do-not-export'),false);
  assert.deepEqual(f.stored.downloadCapture,settings.downloadCapture);assert.deepEqual(f.stored.disabledSites,{'two.test':true});assert.equal(f.stored.unrelated,'keep');
  assert.equal(f.messages.some(m=>m.method==='wakeup'),false);
});
test('each actual settings read gets the current App value without extension reload',async()=>{
  const f=fixture();await f.bridge.settings();f.data.revision=2;f.data.settings.downloadCapture.enabled=false;
  assert.equal((await f.bridge.settings()).downloadCapture.enabled,false);assert.equal(f.messages.length,2);assert.equal(f.stored.downloadCapture.enabled,false);
});
test('invalid or unavailable App responses never install independent fallback preferences',async()=>{
  for(const invalid of [true,false]) {
    const f=fixture();if(invalid)f.data.settings.downloadCapture.enabled='yes';else f.failure=true;
    await assert.rejects(f.bridge.settings());assert.equal(f.writes.length,0);assert.equal(f.stored.downloadCapture.enabled,true);
  }
});
test('old revisions cannot overwrite a newer canonical cache',async()=>{
  const f=fixture();f.data.revision=4;await f.bridge.settings();f.data.revision=3;f.data.settings.downloadCapture.enabled=false;
  assert.equal((await f.bridge.settings()).downloadCapture.enabled,true);assert.equal(f.stored.browserSettingsCache.revision,4);
});
test('explicit settings actions wake and edit through the App only',async()=>{
  const f=fixture();await f.bridge.updateSettings({sniffing:{showAll:true}});
  assert.deepEqual(f.messages.map(m=>m.method==='browser'?m.params.action:m.method),['wakeup','sync','updateSettings']);
  assert.deepEqual(f.messages[2].params.patch,{sniffing:{showAll:true}});
  assert.equal(f.stored.showAll,false);
  f.messages.length=0;await f.bridge.openSettings();assert.deepEqual(f.messages.map(m=>m.method==='browser'?m.params.action:m.method),['wakeup','sync','openSettings']);
});
test('caller parameters cannot replace the browser client identity',async()=>{
  const f=fixture();await f.bridge.request('submission',{clientId:'other-client',browser:'chrome',operationId:'op-1'});
  assert.equal(f.messages[0].params.clientId,'stable-client');assert.equal(f.messages[0].params.browser,'edge');
  assert.equal(await browserClientId(f.browser),'stable-client');
});
