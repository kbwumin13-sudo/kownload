const identities = new WeakMap();

export function browserClientId(browser) {
  if (!identities.has(browser)) {
    const pending = (async () => {
      const saved = await browser.storage.local.get('netdiskInstanceId');
      if (typeof saved.netdiskInstanceId === 'string' && saved.netdiskInstanceId.trim()) return saved.netdiskInstanceId;
      const id = globalThis.crypto.randomUUID();
      await browser.storage.local.set({netdiskInstanceId:id});
      return id;
    })().catch(error => { identities.delete(browser);throw error; });
    identities.set(browser,pending);
  }
  return identities.get(browser);
}
