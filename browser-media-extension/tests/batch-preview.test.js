import test from 'node:test';
import assert from 'node:assert/strict';
import {importLinkSelection, readLinkSelection} from '../batch-preview.js';

const frame = {url:'https://fixture.invalid/page?private=1',documentId:'doc-A',documentLifecycle:'active'};
const same = 'https://fixture.invalid/F%2file?B=2&a=Ab%2f';
const distinct = 'https://fixture.invalid/F%2file?a=Ab%2F&B=2';
const cross = 'https://cdn.fixture.invalid/x?token=example';

function fixture(links = [same, distinct, cross, same]) {
  let pendingFrame = {...frame};
  const calls = [];
  const browser = {
    runtime:{id:'fixture-ext',getURL:path => 'chrome-extension://fixture-ext/'+path},
    tabs:{query:async query => {calls.push(['tabs',query]);return [{id:1}];}},
    webNavigation:{getFrame:async target => {calls.push(['frame',target]);return pendingFrame;}},
    scripting:{executeScript:async args => {
      calls.push(['execute',args]);
      return [{frameId:0,documentId:frame.documentId,result:{links}}];
    }},
  };
  const sender = {id:browser.runtime.id,url:browser.runtime.getURL('popup.html')};
  const send = async message => calls.push(['send',message]);
  const captured = target => {
    calls.push(['context',target]);
    if (target.url === same) return {Authorization:'fixture-auth-a',Cookie:'session=fixture-a'};
    return undefined;
  };
  return {browser,sender,send,captured,calls,changeFrame:next => {pendingFrame=next;}};
}

test('explicit popup reads one current document and sends only a preview draft', async () => {
  const f = fixture();
  assert.equal(await importLinkSelection(f.browser,f.sender,f.captured,f.send),'selectionOpened');
  assert.deepEqual(f.calls.filter(([key]) => key==='execute').map(([,args]) => args), [{
    target:{tabId:1,documentIds:[frame.documentId]},func:readLinkSelection,world:'ISOLATED',
  }]);
  const contexts = f.calls.filter(([key]) => key==='context').map(([,target]) => target.url);
  assert.deepEqual(contexts,[same,distinct,same]);
  const matched = {url:same,header:{Authorization:'fixture-auth-a',Cookie:'session=fixture-a'},context:'doc-A'};
  assert.deepEqual(f.calls.filter(([key]) => key==='send'), [['send',{
    method:'batchPreview',params:{version:1,items:[matched,{url:distinct},{url:cross},matched]},
  }]]);
  assert.equal(JSON.stringify(f.calls.at(-1)).includes('private=1'),false);
});

test('page/content sender, empty range, stale document and non-HTTP frame never send', async () => {
  const forged = fixture();
  assert.equal(await importLinkSelection(forged.browser,{...forged.sender,tab:{id:1}},forged.captured,forged.send),null);
  assert.deepEqual(forged.calls,[]);

  const empty = fixture([]);
  assert.equal(await importLinkSelection(empty.browser,empty.sender,empty.captured,empty.send),'selectionEmpty');
  assert.equal(empty.calls.some(([kind]) => kind==='send'),false);

  for (const scenario of [{documentId:'doc-B'},{url:'https://fixture.invalid/another'},{documentLifecycle:'cached'}]) {
    const f = fixture();
    const execute = f.browser.scripting.executeScript;
    f.browser.scripting.executeScript = async args => {
      f.changeFrame({...frame,...scenario});
      return execute(args);
    };
    assert.equal(await importLinkSelection(f.browser,f.sender,f.captured,f.send),'selectionUnavailable');
    assert.equal(f.calls.some(([kind]) => ['send','context'].includes(kind)),false);
  }

  const internal = fixture();
  internal.changeFrame({...frame,url:'chrome://extensions/'});
  assert.equal(await importLinkSelection(internal.browser,internal.sender,internal.captured,internal.send),'selectionUnavailable');
  assert.equal(internal.calls.some(([kind]) => kind==='execute'),false);
});

test('count and byte bounds reject the draft before native dispatch', async () => {
  for (const links of [Array(201).fill(same),Array(10).fill('https://fixture.invalid/?sig='+'X'.repeat(8190))]) {
    const f = fixture(links);
    assert.equal(await importLinkSelection(f.browser,f.sender,f.captured,f.send),'selectionTooLarge');
    assert.equal(f.calls.some(([kind]) => kind==='send'),false);
  }
});
