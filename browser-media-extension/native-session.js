import {bridgeFailure} from './lib.js';

// A connection exists only while an explicit capture/selection flow holds it.
// Requests are serialized because Native Messaging replies carry no frame ID.
export function createNativeSession(browser,{timeoutMs=25000}={}) {
  const port=browser.runtime.connectNative('com.gopeed.gopeed'),queue=[];
  let current,closed=false;
  function close() {
    if (closed) return;
    closed=true;
    if (current) {clearTimeout(current.timer);current.reject(bridgeFailure('ipc_unavailable'));current=null;}
    for (const pending of queue.splice(0)) pending.reject(bridgeFailure('ipc_unavailable'));
    try {port.disconnect();} catch {}
  }
  function pump() {
    if (closed || current || !queue.length) return;
    current=queue.shift();current.timer=setTimeout(close,timeoutMs);
    try {port.postMessage(current.message);} catch {close();}
  }
  port.onMessage.addListener(reply=>{
    if (closed || !current) return;
    const pending=current;current=null;clearTimeout(pending.timer);
    pending.resolve(reply);pump();
  });
  port.onDisconnect.addListener(()=>{void browser.runtime.lastError;close();});
  return {get closed(){return closed;},close,send(message){
    if (closed) return Promise.reject(bridgeFailure('ipc_unavailable'));
    return new Promise((resolve,reject)=>{queue.push({message,resolve,reject});pump();});
  }};
}
