import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const script=fs.readFileSync(new URL('../download-confirm.js',import.meta.url),'utf8');
const flush=()=>new Promise(resolve=>setImmediate(resolve));
function page(reply) {
  const timers=new Map(),events={},messages=[],nodes={};let nextTimer=0;
  for(const id of ['name','status','submit','resume','cancel']) nodes[id]={id,disabled:false,textContent:'',addEventListener:(event,fn)=>{events[id+':'+event]=fn;}};
  const f={nodes,timers,messages,events,reply:reply || {ok:true,job:{id:7,name:'file.bin',state:'waiting'},browser:{state:'in_progress',paused:true}}};
  vm.runInNewContext(script,{
    URLSearchParams,location:{search:'?id=7'},
    document:{getElementById:id=>nodes[id],querySelectorAll:()=>['submit','resume','cancel'].map(id=>nodes[id])},
    chrome:{runtime:{sendMessage:async message=>{messages.push(message);return typeof f.reply === 'function' ? f.reply(message) : structuredClone(f.reply);}}},
    addEventListener:(event,fn)=>{events[event]=fn;},
    setTimeout:(fn,ms)=>{const id=++nextTimer;timers.set(id,{fn,ms});return id;},clearTimeout:id=>timers.delete(id)
  });
  f.tick=async()=>{const [id,timer]=timers.entries().next().value;timers.delete(id);timer.fn();await flush();};
  return f;
}
test('an open pending confirmation sends bounded status messages and stops after creation',async()=>{
  const f=page();await flush();assert.equal(f.timers.size,1);assert.equal([...f.timers.values()][0].ms,10000);
  await f.tick();assert.equal(f.messages.length,2);assert.equal(f.messages[1].action,'status');
  f.reply.job.state='created';f.reply.browser.state='interrupted';await f.tick();
  assert.equal(f.timers.size,0);assert.equal(f.nodes.submit.disabled,true);assert.match(f.nodes.status.textContent,/已在 kownload 创建任务/);
});
test('completed browser state overrides a stale waiting label without offering another submission',async()=>{
  const f=page({ok:true,job:{id:7,state:'waiting'},browser:{state:'complete',paused:true}});await flush();
  assert.match(f.nodes.status.textContent,/浏览器已完成下载/);assert.equal(f.nodes.submit.disabled,true);assert.equal(f.timers.size,0);
});
test('closing a confirmation clears its timer and cannot restart communication from a late reply',async()=>{
  const f=page();await flush();let reply;
  f.reply=()=>new Promise(resolve=>{reply=resolve;});const ticking=f.tick();
  f.events.pagehide();reply({ok:true,job:{id:7,state:'waiting'},browser:{state:'in_progress',paused:true}});await ticking;
  assert.equal(f.timers.size,0);
});
test('a slow status response cannot cause a duplicate submit while controls are disabled',async()=>{
  const f=page();await flush();let reply;
  f.reply=()=>new Promise(resolve=>{reply=resolve;});const ticking=f.tick();
  const before=f.messages.length;f.events['submit:click']();assert.equal(f.messages.length,before);
  reply({ok:true,job:{id:7,state:'waiting'},browser:{state:'in_progress',paused:true}});await ticking;
  f.events.pagehide();
});
