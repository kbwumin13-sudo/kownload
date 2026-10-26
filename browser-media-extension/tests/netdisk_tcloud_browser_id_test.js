import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {readNetdiskPage} from '../netdisk-page.js';
import {NETDISK_PROVIDERS, sanitizeNetdiskCapture} from '../netdisk.js';

// Public source evidence, checked 2026-10-15:
// /web/main/js/index.9f3753d6.js registers micro-home-main in #frame,
// with entry https://cloud.189.cn/web/subapp/main.html (no Shadow DOM option).
// That entry loads cloud.dlife.cn/web/main/subapp/js/main.29487b28.js:
// bootstrap passes store=a4f5 to Vue; module a8f3 renders .p-web over #app.
// chunk-common.c41232a0.js a4f5 owns state.browserId; interceptor 5724
// initializes it from visitorId and sends it as Browser-Id for listFiles.
// Every value below is synthetic. No browser profile or real session is used.
const url = 'https://cloud.189.cn/web/main/';
const provider = NETDISK_PROVIDERS.find(p => p.id === 'tcloud');
function capture(nodes = {}) {
  const effects = [], reads = [];
  const forbidden = name => () => {effects.push(name); throw new Error(name);};
  const window = {FingerprintJS:{load:forbidden('fingerprint')}, require:forbidden('module execution')};
  window.top = window;
  const document = {querySelectorAll:selector => nodes[selector] || []};
  Object.defineProperty(document, 'cookie', {get:forbidden('cookie read')});
  const result = vm.runInNewContext(`(${readNetdiskPage.toString()})('tcloud',${JSON.stringify(url)})`, {
    window, document, URL, location:{href:url},
    localStorage:{getItem:key => {reads.push(key); return key === 'accessToken' ? '"synthetic-access"' : null;},
      setItem:forbidden('storage write'), removeItem:forbidden('storage remove'), clear:forbidden('storage clear')},
    sessionStorage:{getItem:forbidden('private session read')},
    fetch:forbidden('network'), XMLHttpRequest:forbidden('network'),
    Math:{random:forbidden('generated ID')}, crypto:{randomUUID:forbidden('generated ID')}
  });
  assert.deepEqual(effects, []);
  assert(!reads.includes('browserId'));
  const raw = JSON.parse(JSON.stringify(result));
  const clean = sanitizeNetdiskCapture(raw, {provider, url, browser:'chrome', instanceId:'synthetic-instance', contextId:'synthetic-context'});
  return {raw, clean};
}
const store = id => ({state:{browserId:id, privateValue:'never-export'},
  dispatch:() => {throw new Error('store dispatch');}, commit:() => {throw new Error('store mutation');}});

test('tcloud: mounted .p-web root supplies Browser-Id before #app or file list exists', () => {
  const root = {__vue__:{$store:store('initialized-visitor')}};
  const before = JSON.stringify(root);
  const {raw, clean} = capture({'.p-web':[root]});
  assert.deepEqual(raw.tokens, {browserId:'initialized-visitor', accessToken:'synthetic-access'});
  assert.equal(clean.headers['Browser-Id'], 'initialized-visitor');
  assert.deepEqual(raw.files, []);
  assert.equal(JSON.stringify(root), before);
  assert(!JSON.stringify(raw).includes('never-export'));
});

test('tcloud: document_start and empty initialized store stay empty until the site sets visitorId', () => {
  const root = {__vue__:{$store:store('')}};
  assert.equal(capture().clean.headers['Browser-Id'], undefined);
  assert.equal(capture({'.p-web':[root]}).clean.headers['Browser-Id'], undefined);
  // Simulate the official asynchronous request interceptor completing later.
  root.__vue__.$store.state.browserId = 'site-initialized-later';
  assert.equal(capture({'.p-web':[root]}).clean.headers['Browser-Id'], 'site-initialized-later');
});

for (const [name, node] of [
  ['Vue2 own store', {__vue__:{$store:store('site-visitor')}}],
  ['Vue2 root store', {__vue__:{$root:{$store:store('site-visitor')}}}],
  ['Vue2 parent store', {__vue__:{$parent:{$parent:{$store:store('site-visitor')}}}}],
  ['Vue3 proxy store', {__vueParentComponent:{proxy:{$store:store('site-visitor')}}}],
  ['Vue3 fixed global store', {__vueParentComponent:{appContext:{config:{globalProperties:{$store:store('site-visitor')}}}}}]
]) test(`tcloud: ${name} reuses only its existing browserId`, () => {
  assert.equal(capture({'.p-web':[node]}).clean.headers['Browser-Id'], 'site-visitor');
});

test('tcloud: file selection still comes from explicitly selected rows', () => {
  const node = {__vue__:{$store:store('site-visitor'),
    selectedList:[{id:'chosen',name:'chosen.txt',size:7}],
    fileList:[{id:'unselected',name:'unselected.txt',size:8}]}};
  const {raw} = capture({'.c-file-list':[node], '.p-web':[node]});
  assert.deepEqual(raw.files.map(f => f.id), ['chosen']);
  assert.equal(raw.tokens.browserId, 'site-visitor');
});

test('tcloud: a selected component identity is not overwritten by another root or parent', () => {
  const child = {__vue__:{$store:store('active-visitor'), $parent:{$store:store('other-visitor')}}};
  const {raw} = capture({'.c-file-list':[child], '.p-web':[{__vue__:{$store:store('other-visitor')}}]});
  assert.equal(raw.tokens.browserId, 'active-visitor');
});

test('tcloud: invalid browserId does not hide a valid root ID or become a header', () => {
  for (const value of ['bad\r\nheader', 'bad\x00header', {}, null, '']) {
    const vue = {$store:store(value), $root:{$store:store('valid-visitor')}};
    assert.equal(capture({'.p-web':[{__vue__:vue}]}).clean.headers['Browser-Id'], 'valid-visitor');
    assert.equal(capture({'.p-web':[{__vue__:{$store:store(value)}}]}).clean.headers['Browser-Id'], undefined);
  }
});

test('tcloud: cyclic or uninitialized Vue parents remain bounded without inventing an ID', () => {
  const vue = {$store:store('')};
  vue.$parent = vue;
  assert.equal(capture({'.p-web':[{__vue__:vue}]}).clean.headers['Browser-Id'], undefined);
});
