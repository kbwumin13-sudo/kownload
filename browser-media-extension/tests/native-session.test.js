import test from 'node:test';
import assert from 'node:assert/strict';
import {createNativeSession} from '../native-session.js';

function fixture(timeoutMs=25000) {
  const callbacks={},messages=[];let disconnected=0;
  const port={postMessage:m=>messages.push(m),disconnect:()=>{disconnected++;},onMessage:{addListener:fn=>{callbacks.message=fn;}},onDisconnect:{addListener:fn=>{callbacks.disconnect=fn;}}};
  const session=createNativeSession({runtime:{connectNative:()=>port}},{timeoutMs});
  return {session,messages,callbacks,get disconnected(){return disconnected;}};
}
test('one active Native conversation serializes frames and never retries a submission',async()=>{
  const f=fixture();const a=f.session.send({method:'create',operationId:'one'}),b=f.session.send({method:'browser'});
  assert.equal(f.messages.length,1);f.callbacks.message({code:0,data:'one'});assert.equal((await a).data,'one');
  assert.equal(f.messages.length,2);f.callbacks.message({code:0,data:'two'});assert.equal((await b).data,'two');
  f.session.close();f.session.close();assert.equal(f.disconnected,1);
});
test('a host disconnect rejects the in-flight and unsent frames without reporting user cancellation',async()=>{
  const f=fixture();const a=f.session.send({method:'create'}),b=f.session.send({method:'browser'});
  const results=Promise.allSettled([a,b]);f.callbacks.disconnect();
  for(const result of await results)assert.equal(result.status,'rejected');
  assert.equal(f.messages.length,1);assert.equal(f.session.closed,true);
});
test('a timed-out frame closes the conversation so a late reply cannot satisfy the next request',async()=>{
  const f=fixture(5);const a=f.session.send({method:'create'}),b=f.session.send({method:'browser'});
  const results=await Promise.allSettled([a,b]);assert(results.every(r=>r.status==='rejected'));
  f.callbacks.message({code:0,data:'late'});assert.equal(f.messages.length,1);assert.equal(f.session.closed,true);
});
