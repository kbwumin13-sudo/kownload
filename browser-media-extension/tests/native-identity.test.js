import test from 'node:test';
import assert from 'node:assert/strict';
import {checkNativeConnection,createBoundTransport,nativeBinding,sendNativeBound,NATIVE_HOST,NATIVE_BINDING_KEY} from '../native-identity.js';

function fixture(saved={}) {
  const f={stored:{netdiskInstanceId:'profile-one',...structuredClone(saved)},frames:[],hosts:[],writes:0,storageWrites:[],scope:'a'.repeat(64),running:true,appIdentity:true};
  f.info=()=>({version:1,product:'kownload',nativeProtocol:1,identityProtocol:1,
    build:{sourceCommit:'b'.repeat(40),releaseVersion:'fixture',preview:false},
    supportedHostNames:[NATIVE_HOST,'com.gopeed.gopeed'],callerOrigin:'chrome-extension://helper-id/',routeScope:f.scope,
    app:{reachable:f.running,identity:f.running && f.appIdentity ? {version:1,product:'kownload',identityProtocol:1,routeScope:f.scope,build:{preview:false}} : null},
    core:{reachable:null,handoffVersion:null}});
  f.raw=async message=>{
    f.frames.push(structuredClone(message));
    if(message.method==='hostInfo')return {code:0,data:f.info()};
    assert.equal(message.identityProtocol,1);
    assert.equal(message.bridgeBinding.hostName,NATIVE_HOST);
    if(message.bridgeBinding.routeScope!==f.scope)return {code:1,error:'route_mismatch'};
    f.writes++;
    assert.deepEqual(f.stored[NATIVE_BINDING_KEY],message.bridgeBinding,'binding must be durably saved before every modifying frame');
    if(message.method==='wakeup')f.running=true;
    if(f.disconnect)throw Error('disconnected');
    return {code:0,data:{code:0,data:{state:'unknown',taskIds:[]}}};
  };
  f.browser={storage:{local:{get:async()=>structuredClone(f.stored),set:async value=>{
    f.storageWrites.push(structuredClone(value));if(f.storageFail)throw Error('storage');Object.assign(f.stored,structuredClone(value));
  }}},runtime:{id:'helper-id',sendNativeMessage:(host,message,callback)=>{f.hosts.push(host);f.raw(message).then(callback);}}};
  f.transport=()=>createBoundTransport(f.browser,f.raw);
  f.modifying=()=>f.frames.filter(m=>m.method!=='hostInfo');
  return f;
}
const binding=scope=>({version:1,hostName:NATIVE_HOST,routeScope:scope.repeat(64)});

test('first binding is saved before modifying frame; three-entry route uses only new host',async()=>{
  const f=fixture();await sendNativeBound(f.browser,{method:'browser',params:{action:'sync'}});
  assert.equal(f.writes,1);assert.deepEqual(f.hosts,[NATIVE_HOST,NATIVE_HOST]);
  assert.deepEqual(f.storageWrites,[{[NATIVE_BINDING_KEY]:binding('a')}]);
  assert(!JSON.stringify(f.stored).includes('params'));
});
test('persistence failure sends zero wake/create/forward and keeps previous storage',async()=>{
  for(const method of ['wakeup','create','forward','browser']) {
    const f=fixture();f.storageFail=true;
    await assert.rejects(f.transport().send({method}),/storage/);
    assert.equal(f.writes,0);assert.deepEqual(f.modifying(),[]);
    assert.equal(f.stored[NATIVE_BINDING_KEY],undefined);
  }
});
test('worker restart with same journal route queries same operation, never selects legacy',async()=>{
  const a=fixture();await a.transport().send({method:'browser',params:{action:'handoffPrepare',operationId:'same-op'}});
  const b=fixture(a.stored);await sendNativeBound(b.browser,{method:'browser',params:{action:'handoffQuery',operationId:'same-op'}});
  assert.deepEqual(b.modifying().map(m=>m.params.action),['handoffQuery']);
  assert(b.hosts.every(h=>h===NATIVE_HOST));assert.equal(b.storageWrites.length,0);
});
test('changed route after worker restart preserves original binding with zero writes',async()=>{
  const f=fixture({[NATIVE_BINDING_KEY]:binding('a')});f.scope='c'.repeat(64);
  for(let i=0;i<2;i++)await assert.rejects(f.transport().send({method:'create'}),{category:'route_mismatch'});
  assert.equal(f.writes,0);assert.deepEqual(f.stored[NATIVE_BINDING_KEY],binding('a'));
  assert.equal(f.storageWrites.length,0);
});
test('different short-lived writing process must receive the expected binding',async()=>{
  const f=fixture();const raw=async message=>{
    if(message.method==='hostInfo'){const reply=await f.raw(message);f.scope='d'.repeat(64);return reply;}
    return f.raw(message);
  };
  const reply=await createBoundTransport(f.browser,raw).send({method:'create',operationId:'once'});
  assert.equal(reply.error,'route_mismatch');assert.equal(f.writes,0);
  assert.equal(f.modifying().length,1);assert.equal(f.modifying()[0].bridgeBinding.routeScope,'a'.repeat(64));
});
test('unsupported or unknown saved binding is not silently replaced',async()=>{
  for(const saved of [{version:2,hostName:NATIVE_HOST,routeScope:'a'.repeat(64)}, {...binding('a'),routeScope:['a'.repeat(64)]}, {...binding('a'),extra:'unknown'}, binding('a')]) {
    const f=fixture({[NATIVE_BINDING_KEY]:saved});
    if(saved.version===1)f.scope='e'.repeat(64);
    await assert.rejects(f.transport().send({method:'forward'}));
    assert.deepEqual(f.stored[NATIVE_BINDING_KEY],saved);assert.equal(f.writes,0);
  }
});
test('concurrent first probes cannot persist two different profile routes',async()=>{
  const f=fixture();
  const first=createBoundTransport(f.browser,async message=>({code:0,data:f.info()}));
  const other=createBoundTransport(f.browser,async message=>{const info=f.info();info.routeScope='b'.repeat(64);info.app.identity.routeScope=info.routeScope;return {code:0,data:info};});
  const results=await Promise.allSettled([first.binding(),other.binding()]);
  assert.equal(results[0].status,'fulfilled');assert.equal(results[1].status,'rejected');
  assert.deepEqual(f.stored[NATIVE_BINDING_KEY],binding('a'));assert.equal(f.storageWrites.length,1);assert.equal(f.writes,0);
  await assert.rejects(other.send({method:'create'}),{category:'route_mismatch'});
  assert.equal(f.storageWrites.length,1);
});
test('cold explicit create wakes only after durable binding then verifies App before create',async()=>{
  const f=fixture();f.running=false;
  await f.transport().send({method:'create',operationId:'cold'});
  assert.deepEqual(f.modifying().map(m=>m.method),['wakeup','create']);
  assert.equal(f.frames.filter(m=>m.method==='hostInfo').length,2);
  const passive=fixture();passive.running=false;
  await assert.rejects(passive.transport().send({method:'browser',params:{action:'sync'}}),{category:'ipc_unavailable'});
  assert.equal(passive.writes,0);
});
test('old App reachable without identity is not a supported new write route',async()=>{
  const f=fixture();f.appIdentity=false;
  await assert.rejects(f.transport().send({method:'create'}),{category:'identity_unsupported'});
  assert.equal(f.writes,0);
});
test('caller origin and App route/preview mismatches block writes',async()=>{
  for(const patch of [info=>{info.callerOrigin='chrome-extension://other/';},info=>{info.app.identity.routeScope='f'.repeat(64);},info=>{info.app.identity.build.preview=true;}]) {
    const f=fixture(),original=f.info;f.info=()=>{const value=original();patch(value);return value;};
    await assert.rejects(f.transport().send({method:'browser'}));assert.equal(f.writes,0);
  }
});
test('disconnect does not retry or switch hosts; subsequent explicit query keeps binding',async()=>{
  const f=fixture();f.disconnect=true;
  await assert.rejects(f.transport().send({method:'create',operationId:'uncertain'}));
  assert.equal(f.writes,1);f.disconnect=false;
  await f.transport().send({method:'browser',params:{action:'submission',operationId:'uncertain'}});
  assert.equal(f.modifying().filter(m=>m.method==='create').length,1);
  assert.deepEqual(f.stored[NATIVE_BINDING_KEY],binding('a'));
});
test('captured pending expected route is checked before journal or Native writes',async()=>{
  const f=fixture();await assert.rejects(nativeBinding(f.browser,binding('z')),{category:'route_mismatch'});
  assert.equal(f.writes,0);assert.deepEqual(f.storageWrites,[]);
});

test('explicit connection checks separate disconnected layers and read history without binding or syncing',async t=>{
  t.mock.timers.enable({apis:['setTimeout']});
  const syncedAt=Date.UTC(2024,0,1);
  const cases=[
    ['matched','ready','reachable','matched'],
    ['cold','ready','unreachable','unknown'],
    ['older App','ready','reachable','unsupported'],
    ['wrong App','ready','reachable','mismatch'],
    ['wrong profile binding','ready','reachable','binding'],
    ['storage unavailable','ready','reachable','bindingUnknown'],
    ['older Host','unsupported','unknown','unknown'],
    ['missing','missing','unknown','unknown'],
    ['forbidden','forbidden','unknown','unknown'],
    ['failed','failed','unknown','unknown'],
    ['timeout','timeout','unknown','unknown'],
  ];
  for(const [label,host,app,identity] of cases) {
    const f=fixture({lastSettingsSyncAt:syncedAt});
    delete f.stored.netdiskInstanceId;
    if(label==='cold') f.running=false;
    if(label==='older App') f.appIdentity=false;
    if(label==='wrong profile binding') f.stored[NATIVE_BINDING_KEY]=binding('b');
    if(label==='storage unavailable') f.browser.storage.local.get=async()=>{throw Error('private storage');};
    const nativeErrors={
      missing:'Specified native messaging host not found.',
      forbidden:'Access to the specified native messaging host is forbidden.',
      failed:'https://private.invalid/?token=SYNTHETIC_COOKIE',
    };
    const calls=[];
    f.browser.runtime.sendNativeMessage=(hostName,message,reply)=>{
      calls.push([hostName,message]);
      if(label==='timeout') return;
      if(nativeErrors[label]) {
        f.browser.runtime.lastError={message:nativeErrors[label]};
        reply();
        delete f.browser.runtime.lastError;
        return;
      }
      const info=f.info();
      if(label==='wrong App') info.app.identity.routeScope='c'.repeat(64);
      reply(label==='older Host' ? {code:1} : {code:0,data:info});
    };
    const pending=checkNativeConnection(f.browser);
    // Let the storage read settle before advancing the native-response deadline.
    await Promise.resolve();
    await Promise.resolve();
    if(label==='timeout') t.mock.timers.tick(3500);
    const syncKnown=label!=='storage unavailable';
    assert.deepEqual(await pending,{host,app,identity,syncKnown,syncedAt:syncKnown ? syncedAt : null},label);
    assert.deepEqual(calls.map(([name,message])=>[name,message.method,message.params.clientId]),
      [[NATIVE_HOST,'hostInfo','connection-check']],label);
    assert.deepEqual(f.storageWrites,[],label);
    assert.equal(f.stored.netdiskInstanceId,undefined,label);
  }
});
