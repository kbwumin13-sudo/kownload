import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {readNetdiskPage} from '../netdisk-page.js';

// Synthetic values with the storage shapes from the fixed
// artifacts/linkswift-2026-10-14/upstream/LinkSwift-dev.user.js:
// Xunlei getToken: credentials_<client>, captcha_<client>={token,expires_at}.
// Guangya official public bundles (read 2026-10-15):
// https://www.guangyapan.com/static/js/index.b1e7c5bb.js declares the client;
// https://www.guangyapan.com/static/js/394.c71f4afa.js LocalCredentials and
// LocalCaptchaTokenInfoManager define the keys, sub, and captcha_token shape.
const webClient = 'aMe-8VSlkrbQXpUR';
const now = Date.parse('2026-10-15T12:00:00Z');
const at = seconds => new Date(now + seconds * 1000).toISOString();
const credentials = (access, seconds, extra = {}) => ({
  access_token:access, token_type:'Bearer', user_id:`user-${access}`,
  expires_in:43200, expires_at:at(seconds), ...extra
});
const captcha = (provider, token, seconds = 3600) => provider === 'xunlei'
  ? {token, expires_at:new Date(now + seconds * 1000).toString()}
  : {action:'GET:/drive/v1/about', meta:{}, captcha_token:token, expires_in:3600, expires_at:at(seconds)};

function capture(provider, entries) {
  const storage = new Map(entries.map(([key,value])=>[key,typeof value === 'string' ? value : JSON.stringify(value)]));
  const before = [...storage], reads = [], effects = [];
  const forbidden = effect => (...args)=>{effects.push([effect,...args]);throw new Error(effect);};
  const url = provider === 'xunlei' ? 'https://pan.xunlei.com/drive/all' : 'https://www.guangyapan.com/drive';
  const window = {};window.top = window;
  const document = {querySelectorAll:()=>[]};
  Object.defineProperty(document,'cookie',{get:forbidden('cookie read')});
  // Execute the production function exactly as chrome.scripting serializes it.
  // No copied selector or auth implementation lives in this test.
  const result = vm.runInNewContext(`(${readNetdiskPage.toString()})(${JSON.stringify(provider)},${JSON.stringify(url)})`,{
    window, document, URL, location:{href:url}, Date:class extends Date {static now(){return now;}},
    localStorage:{length:storage.size, key:i=>[...storage.keys()][i],
      getItem:key=>{reads.push(key);return storage.get(key) ?? null;},
      setItem:forbidden('storage write'), removeItem:forbidden('storage remove'), clear:forbidden('storage clear')},
    fetch:forbidden('network'), XMLHttpRequest:forbidden('network')
  });
  assert.deepEqual(effects,[]);
  assert.deepEqual([...storage],before);
  return {result:JSON.parse(JSON.stringify(result)), reads};
}

for (const provider of ['xunlei','guangya']) {
  test(`${provider}: documented storage shape captures auth and matching captcha without side effects`,()=>{
    const client = provider === 'guangya' ? webClient : 'Xqp0kJBXWhwaTpB6';
    const {result,reads} = capture(provider,[
      [`credentials_${client}`,credentials('current',7200,{refresh_token:'never-export',unrelated:'never-export'})],
      [`captcha_${client}`,captcha(provider,'current-captcha')],
      ['deviceid','wdi10.0123456789abcdef0123456789abcdef,web'],
      ['unrelated','never-read']
    ]);
    assert.deepEqual(result.tokens,{access_token:'current',token_type:'Bearer',user_id:'user-current',client_id:client,captcha_token:'current-captcha',deviceid:'0123456789abcdef0123456789abcdef'});
    assert.equal(result.accountId,'user-current');
    assert(!JSON.stringify(result).includes('never-export'));
    assert(!reads.includes('unrelated'));
  });

  test(`${provider}: newer unexpired credential wins regardless of storage enumeration order`,()=>{
    const entries = [
      ['credentials_recent',credentials('recent',7200)],
      ['captcha_recent',captcha(provider,'recent-captcha')],
      ['credentials_old',credentials('old',3600)],
      ['captcha_old',captcha(provider,'old-captcha')],
      ['credentials_expired',credentials('expired',-1)],
      ['captcha_expired',captcha(provider,'expired-client-captcha')]
    ];
    for (const ordered of [entries,[...entries].reverse()]) {
      const {result,reads} = capture(provider,ordered);
      assert.deepEqual(result.tokens,{access_token:'recent',token_type:'Bearer',user_id:'user-recent',client_id:'recent',captcha_token:'recent-captcha'});
      assert(!reads.includes('captcha_old'));
      assert(!reads.includes('captcha_expired'));
    }
  });

  test(`${provider}: incomplete credentials cannot retain another client's access token or user`,()=>{
    const {result} = capture(provider,[
      ['credentials_complete',credentials('complete',3600)],
      ['captcha_complete',captcha(provider,'complete-captcha')],
      ['credentials_partial',{token_type:'Other',user_id:'wrong-user',expires_at:at(7200)}],
      ['captcha_partial',captcha(provider,'wrong-captcha')]
    ]);
    assert.deepEqual(result.tokens,{access_token:'complete',token_type:'Bearer',user_id:'user-complete',client_id:'complete',captcha_token:'complete-captcha'});
  });

  test(`${provider}: optional auth fields come only from the chosen credential`,()=>{
    const {result} = capture(provider,[
      ['credentials_old',credentials('old',3600)],
      ['credentials_selected',{access_token:'selected',expires_at:at(7200)}],
      ['captcha_old',captcha(provider,'wrong-captcha')]
    ]);
    assert.deepEqual(result.tokens,{access_token:'selected',client_id:'selected'});
    assert.equal(result.accountId,'');
  });

  test(`${provider}: missing selected-client captcha never borrows another client's captcha`,()=>{
    const {result} = capture(provider,[
      ['credentials_selected',credentials('selected',7200)],
      ['credentials_other',credentials('other',3600)],
      ['captcha_other',captcha(provider,'wrong-captcha')]
    ]);
    assert.equal(result.tokens.client_id,'selected');
    assert.equal(result.tokens.captcha_token,undefined);
  });

  test(`${provider}: expired, invalid and orphan captcha tokens are not captured`,()=>{
    for (const expiry of [at(-1),at(0),'1970-01-01T00:00:00Z','invalid-date']) {
      const {result} = capture(provider,[
        ['credentials_client',credentials('auth',7200)],
        ['captcha_client',{...captcha(provider,'unusable-captcha'),expires_at:expiry}],
        ['captcha_other',captcha(provider,'orphan-captcha')]
      ]);
      assert.equal(result.tokens.access_token,'auth');
      assert.equal(result.tokens.captcha_token,undefined);
    }
    assert.deepEqual(capture(provider,[['captcha_orphan',captcha(provider,'orphan')]]).result.tokens,{});
  });

  test(`${provider}: expired or malformed credentials do not produce a partial auth bundle`,()=>{
    for (const value of [credentials('expired',-1),credentials('at-boundary',0),
      credentials('epoch',1,{expires_at:'1970-01-01T00:00:00Z'}),
      credentials('bad-date',1,{expires_at:'invalid-date'}),null,'{',[],
      credentials('bad-header',7200,{access_token:'invalid\r\nvalue'})]) {
      const {result} = capture(provider,[['credentials_client',value],['captcha_client',captcha(provider,'orphan')]]);
      assert.deepEqual(result.tokens,{});
    }
  });

  test(`${provider}: legacy unknown expiry remains usable but does not displace a known unexpired candidate`,()=>{
    const legacy = {access_token:'legacy',token_type:'Bearer'};
    assert.equal(capture(provider,[['credentials_legacy',legacy],['captcha_legacy',{captcha_token:'legacy-captcha'}]]).result.tokens.captcha_token,'legacy-captcha');
    const {result} = capture(provider,[
      ['credentials_current',credentials('current',3600)],
      ['credentials_legacy',legacy],
      ['captcha_legacy',{captcha_token:'wrong-captcha'}]
    ]);
    assert.equal(result.tokens.access_token,'current');
    assert.equal(result.tokens.captcha_token,undefined);
  });
}

test('guangya: official web client wins over a different client with a later expiry',()=>{
  const {result} = capture('guangya',[
    [`credentials_${webClient}`,credentials('web',3600)],
    [`captcha_${webClient}`,captcha('guangya','web-captcha')],
    ['credentials_other',credentials('other',7200)],
    ['captcha_other',captcha('guangya','wrong-captcha')]
  ]);
  assert.equal(result.tokens.access_token,'web');
  assert.equal(result.tokens.client_id,webClient);
  assert.equal(result.tokens.captcha_token,'web-captcha');
});

test('guangya: SDK sub identity and current_sub select the matching account-scoped credential',()=>{
  const {result} = capture('guangya',[
    ['current_sub','active-user'],
    [`credentials_${webClient}@active-user`,credentials('active',3600,{user_id:undefined,sub:'active-user'})],
    [`credentials_${webClient}@other-user`,credentials('other',7200,{user_id:undefined,sub:'other-user'})],
    [`captcha_${webClient}`,captcha('guangya','web-captcha')]
  ]);
  assert.deepEqual(result.tokens,{access_token:'active',token_type:'Bearer',user_id:'active-user',client_id:webClient,captcha_token:'web-captcha'});
  assert.equal(result.accountId,'active-user');
});

test('guangya: expired official web credentials allow an existing unexpired fallback client',()=>{
  const {result} = capture('guangya',[
    ['credentials_fallback',credentials('fallback',3600)],
    ['captcha_fallback',captcha('guangya','fallback-captcha')],
    [`credentials_${webClient}`,credentials('expired-web',-1)],
    [`captcha_${webClient}`,captcha('guangya','wrong-captcha')]
  ]);
  assert.equal(result.tokens.client_id,'fallback');
  assert.equal(result.tokens.captcha_token,'fallback-captcha');
});

test('guangya: known unexpired credentials take priority over official-client credentials with unknown expiry',()=>{
  const {result} = capture('guangya',[
    ['credentials_valid',credentials('valid',3600)],
    [`credentials_${webClient}`,{access_token:'unknown-expiry'}]
  ]);
  assert.equal(result.tokens.access_token,'valid');
  assert.equal(result.tokens.client_id,'valid');
});

test('xunlei: documented token field takes precedence over the existing captcha_token alias',()=>{
  const {result} = capture('xunlei',[
    ['credentials_client',credentials('auth',3600)],
    ['captcha_client',{...captcha('xunlei','stored-token'),captcha_token:'legacy-alias'}]
  ]);
  assert.equal(result.tokens.captcha_token,'stored-token');
});
