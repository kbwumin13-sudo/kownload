import test from 'node:test';
import assert from 'node:assert/strict';
import {createNativeSession} from '../native-session.js';
import {NATIVE_HOST} from '../native-identity.js';
const tick=()=>new Promise(resolve=>setImmediate(resolve));

function fixture(timeoutMs=25000) {
  const callbacks={},messages=[],stored={netdiskInstanceId:'session-profile'};let disconnected=0;
  const port={postMessage:m=>{if(m.method==='hostInfo'){queueMicrotask(()=>callbacks.message({code:0,data:{version:1,product:'kownload',nativeProtocol:1,identityProtocol:1,build:{preview:false},supportedHostNames:[NATIVE_HOST],callerOrigin:'chrome-extension://session-helper/',routeScope:'a'.repeat(64),app:{reachable:true,identity:{version:1,product:'kownload',identityProtocol:1,routeScope:'a'.repeat(64),build:{preview:false}}}}}));}else messages.push(m);},disconnect:()=>{disconnected++;},onMessage:{addListener:fn=>{callbacks.message=fn;}},onDisconnect:{addListener:fn=>{callbacks.disconnect=fn;}}};
  const session=createNativeSession({storage:{local:{get:async()=>stored,set:async value=>Object.assign(stored,value)}},runtime:{id:'session-helper',connectNative:host=>{assert.equal(host,NATIVE_HOST);return port;}}},{timeoutMs});
  return {session,messages,callbacks,get disconnected(){return disconnected;}};
}
test('one active Native conversation serializes frames and never retries a submission',async()=>{
  const f=fixture();const a=f.session.send({method:'create',operationId:'one'}),b=f.session.send({method:'browser'});
  await tick();assert.equal(f.messages.length,1);f.callbacks.message({code:0,data:'one'});assert.equal((await a).data,'one');
  assert.equal(f.messages.length,2);f.callbacks.message({code:0,data:'two'});assert.equal((await b).data,'two');
  f.session.close();f.session.close();assert.equal(f.disconnected,1);
});
test('a host disconnect rejects the in-flight and unsent frames without reporting user cancellation',async()=>{
  const f=fixture();const a=f.session.send({method:'create'}),b=f.session.send({method:'browser'});
  const results=Promise.allSettled([a,b]);await tick();f.callbacks.disconnect();
  for(const result of await results)assert.equal(result.status,'rejected');
  assert.equal(f.messages.length,1);assert.equal(f.session.closed,true);
});
test('a timed-out frame closes the conversation so a late reply cannot satisfy the next request',async()=>{
  const f=fixture(5);const a=f.session.send({method:'create'}),b=f.session.send({method:'browser'});
  const results=await Promise.allSettled([a,b]);assert(results.every(r=>r.status==='rejected'));
  f.callbacks.message({code:0,data:'late'});assert.equal(f.messages.length,1);assert.equal(f.session.closed,true);
});
