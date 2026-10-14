import test from 'node:test';
import assert from 'node:assert/strict';
import {classify,candidateKey,mergeCandidate,isSmall,nativeData,inspectData,taskId,safeHeader,formatChoice,formatOptions,recoverableTasks,nativeCookies,nativeForwardMessage,applyCandidate,applyNavigation} from './lib.js';

test('MIME 分类支持无后缀资源与清单',() => {
  assert.deepEqual(classify('https://a.test/stream?id=1','video/mp4'),{kind:'video',manifest:false});
  assert.deepEqual(classify('https://a.test/resource','application/vnd.apple.mpegurl'),{kind:'video',manifest:true});
  assert.deepEqual(classify('https://a.test/asset','image/webp'),{kind:'image',manifest:false});
  assert.deepEqual(classify('https://a.test/sound','audio/mpeg'),{kind:'audio',manifest:false});
});
test('分片被抑制，签名查询参数保留',() => {
  assert.equal(classify('https://a.test/video/segment_001.ts','video/mp2t'),null);
  assert.equal(classify('https://a.test/a/init.mp4','video/mp4'),null);
  assert.deepEqual(classify('https://a.test/movie.m3u8?sig=abc',''),{kind:'video',manifest:true});
  assert.notEqual(candidateKey({tabId:1,frameId:0,url:'https://a.test/a.mp4?sig=x'}),candidateKey({tabId:1,frameId:0,url:'https://a.test/a.mp4?sig=y'}));
});
test('同文档同 URL 去重补全，frame 与 document 独立',() => {
  const a = {tabId:2,frameId:1,documentId:'docA',url:'https://a.test/a.mp4'};
  assert.equal(candidateKey(a),candidateKey({...a}));
  assert.notEqual(candidateKey(a),candidateKey({...a,frameId:2,documentId:'docB'}));
  assert.notEqual(candidateKey(a),candidateKey({...a,documentId:'docC'}));
  assert.deepEqual(mergeCandidate({...a,title:'原名',width:1920,header:{Referer:'x'}},{...a,title:'',height:1080,header:{Origin:'y'}}).header,{Referer:'x',Origin:'y'});
});
test('仅隐藏已知宽高都小于 128 的图片',() => {
  assert.equal(isSmall({kind:'image',width:127,height:127}),true);
  assert.equal(isSmall({kind:'image',width:127,height:128}),false);
  assert.equal(isSmall({kind:'image',width:0,height:30}),false);
  assert.equal(isSmall({kind:'video',width:20,height:20}),false);
});
test('Native 双层结果与 inspect/任务错误如实抛出',() => {
  assert.deepEqual(nativeData({code:0,data:{code:0,data:{downloadDir:'/tmp'}}}),{downloadDir:'/tmp'});
  assert.throws(() => nativeData({code:2,msg:'主机失败'}),/主机失败/);
  assert.throws(() => nativeData({code:0,data:{code:3,msg:'REST 失败'}}),/REST 失败/);
  assert.throws(() => inspectData({supported:false,reason:'不支持',formats:[]}),/不支持/);
  assert.throws(() => taskId({}),/任务 ID/);
});
test('请求头只保留必要上下文，不转发授权头',() => {
  assert.deepEqual(safeHeader([{name:'Authorization',value:'secret'},{name:'Referer',value:'https://a.test/'},{name:'Origin',value:'https://a.test'},{name:'User-Agent',value:'UA'}]),{'Referer':'https://a.test/','Origin':'https://a.test','User-Agent':'UA'});
});
test('每档单视频轨配音频，MP4 优先 M4A，默认最高带音轨画质',() => {
  const formats = [
    {id:'v720',ext:'webm',height:720,vcodec:'vp9',acodec:'none'},
    {id:'a1',ext:'m4a',vcodec:'none',acodec:'aac'},
    {id:'v1080',ext:'mp4',height:1080,vcodec:'h264',acodec:'none'},
    {id:'a2',ext:'webm',vcodec:'none',acodec:'opus'},
    {id:'mux',ext:'mp4',height:480,vcodec:'h264',acodec:'aac'}
  ];
  assert.deepEqual(formatChoice(formats[2],formats),{id:'v1080+a1',container:'mp4'});
  assert.deepEqual(formatChoice(formats[0],formats),{id:'v720+a2',container:'mkv'});
  const result = formatOptions(formats);
  assert.equal(result.bestId,'v1080+a1');
  assert.match(result.options.find(x => x.id === 'v1080+a1').label,/1080p · mp4/);
  assert.match(result.options.find(x => x.id === 'a1').label,/仅音频/);
  assert.equal(result.options.find(x => x.id === 'mux').id,'mux');
  assert.throws(() => formatOptions([{id:'silent',ext:'mp4',vcodec:'h264',acodec:'none'}]),/没有可下载/);
});
test('恢复任务只取同页同协议的暂停或错误任务',() => {
  const task = (id,status,protocol,sourcePage) => ({id,status,protocol,meta:{req:{extra:{sourcePage}}}});
  const tasks = [task('a','pause','media','https://a.test/watch'),task('b','error','media','https://a.test/watch'),task('c','pause','http','https://a.test/watch'),task('d','running','media','https://a.test/watch'),task('e','error','media','https://a.test/other')];
  assert.deepEqual(recoverableTasks(tasks,'https://a.test/watch','media').map(x => x.id),['a','b']);
});
test('未知轨道保留手选并明确标记，不按扩展名或尺寸自动当作视频',() => {
  for (const format of [{id:'0',ext:'mp4',vcodec:'',acodec:'',height:0,width:0},{id:'high',ext:'mp4',height:4320,vcodec:'',acodec:''},{id:'partial',ext:'mp4',vcodec:'',acodec:'aac'}]) {
    const result = formatOptions([format]);
    assert.equal(result.bestId,'');
    assert.match(result.options[0].label,/轨道未确认/);
    assert.equal(result.options[0].audioOnly,false);
  }
});
test('只有已确认音频时，视频候选要求手选，音频候选可默认音频',() => {
  const audio = {id:'aac',ext:'m4a',vcodec:'none',acodec:'aac'};
  assert.equal(formatOptions([audio],'video').bestId,'');
  assert.equal(formatOptions([audio],'audio').bestId,'aac');
  assert.equal(formatOptions([{id:'0',ext:'mp4',vcodec:'',acodec:''}],'audio').bestId,'');
});
test('显式 unknown codec 不作为已确认视频或音频默认，仍能手动选择',() => {
  for (const format of [
    {id:'both',ext:'mp4',vcodec:'unknown',acodec:'unknown'},
    {id:'video',ext:'mp4',vcodec:'unknown',acodec:'none'},
    {id:'audio',ext:'mp4',vcodec:'none',acodec:'unknown'},
  ]) {
    for (const kind of ['video','audio']) {
      const result = formatOptions([format],kind);
      assert.equal(result.bestId,'');
      assert.equal(result.options.length,1);
      assert.match(result.options[0].label,/轨道未确认/);
      assert.equal(result.options[0].audioOnly,false);
    }
  }
});
test('Native Cookie 保留 hostOnly 和 httpOnly',() => {
  assert.deepEqual(nativeCookies([{name:'sid',value:'x',domain:'a.test',path:'/',secure:true,hostOnly:true,httpOnly:true,expirationDate:123}]),[{name:'sid',value:'x',domain:'a.test',path:'/',secure:true,hostOnly:true,httpOnly:true,expirationDate:123}]);
});
test('Native forward 统一发送 /api/v1 路径',() => {
  assert.equal(nativeForwardMessage('GET','/config').params.path,'/api/v1/config');
  assert.equal(nativeForwardMessage('POST','/media/inspect',{req:{}}).params.path,'/api/v1/media/inspect');
  assert.equal(nativeForwardMessage('GET','/tasks?status=pause').params.path,'/api/v1/tasks?status=pause');
  assert.equal(nativeForwardMessage('GET','/api/v1/info').params.path,'/api/v1/info');
  assert.deepEqual(nativeForwardMessage('POST','/tasks',{req:{}}).params.data,{req:{}});
});
test('SPA 清理只删除边界之前资源，晚入队的新路由网络清单保留',() => {
  const state = {items:{},submitted:{},routes:{},topCommit:{}};
  applyNavigation(state,{tabId:1,frameId:0,timeStamp:100,url:'https://a.test/old',documentId:'doc',kind:'commit'});
  const old = {tabId:1,frameId:0,documentId:'doc',url:'https://a.test/old.m3u8',sourcePage:'https://a.test/old',network:true,capturedAt:110};
  const fresh = {tabId:1,frameId:0,documentId:'doc',url:'https://a.test/new.m3u8',sourcePage:'https://a.test/old',network:true,capturedAt:210};
  assert.ok(applyCandidate(state,old));
  assert.ok(applyCandidate(state,fresh)); // 新响应先完成，导航回调后入队。
  assert.ok(applyCandidate(state,{...fresh,network:false,url:'https://a.test/stale-dom.mp4'})); // 旧页 DOM 晚到，时间已越过边界。
  applyNavigation(state,{tabId:1,frameId:0,timeStamp:200,url:'https://a.test/new',documentId:'doc',kind:'spa'});
  assert.deepEqual(Object.values(state.items).map(x => x.url),['https://a.test/new.m3u8']);
  assert.equal(Object.values(state.items)[0].sourcePage,'https://a.test/new');
  assert.equal(applyCandidate(state,{...old,url:'https://a.test/late-old.m3u8',sourcePage:'https://a.test/new'}),null); // 旧响应晚到。
  assert.ok(applyCandidate(state,{...fresh,url:'https://a.test/later-new.m3u8',sourcePage:'https://a.test/new'}));
  assert.equal(applyCandidate(state,{...old,frameId:2,url:'https://a.test/old-frame.m3u8'}),null); // 顶层 SPA 也挡住旧子 frame。
  assert.ok(applyCandidate(state,{...fresh,frameId:2,url:'https://a.test/new-frame.m3u8',sourcePage:'https://a.test/frame'}));
});
test('普通导航挡住旧 document 的晚到请求并保留新 document 请求',() => {
  const state = {items:{},submitted:{},routes:{},topCommit:{}};
  const old = {tabId:2,frameId:0,documentId:'old',url:'https://a.test/old.m3u8',sourcePage:'https://a.test/old',network:true,capturedAt:120};
  const fresh = {tabId:2,frameId:0,documentId:'new',url:'https://a.test/new.m3u8',sourcePage:'https://a.test/new',network:true,capturedAt:220};
  applyCandidate(state,old);
  applyCandidate(state,fresh);
  applyNavigation(state,{tabId:2,frameId:0,timeStamp:200,url:'https://a.test/new',documentId:'new',kind:'commit'});
  assert.deepEqual(Object.values(state.items).map(x => x.url),['https://a.test/new.m3u8']);
  assert.equal(applyCandidate(state,{...old,url:'https://a.test/late.m3u8',capturedAt:230}),null); // 时间虽新，但属于旧文档。
});
