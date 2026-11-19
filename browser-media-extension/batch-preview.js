// Runs in the isolated world of one browser-verified top document.
export function readLinkSelection() {
  const maxEntries = 200;
  const maxURL = 8192;
  const links = [];
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed ||
      document.activeElement?.matches('iframe,frame')) return {links};
  function append(url) {
    if (links.length >= maxEntries || new TextEncoder().encode(url).length > maxURL) return false;
    links.push(url);
    return true;
  }
  function selectedURL(value) {
    const raw = value.trim();
    if (!raw || /[\u0000-\u0020\u007f\\]/.test(raw)) return '';
    try {
      // Use the effective document base only for relative attributes. The
      // browser parser validates absolute URLs, but its serialization is lossy.
      const absolute = /^[a-z][a-z\d+.-]*:/i.test(raw);
      const parsed = absolute ? new URL(raw) : new URL(raw, document.baseURI);
      if ((absolute && !/^https?:\/\//i.test(raw)) ||
          !['http:', 'https:'].includes(parsed.protocol) ||
          parsed.username || parsed.password) return '';
      return absolute ? raw : parsed.href;
    } catch {
      return '';
    }
  }
  for (let i = 0; i < selection.rangeCount; i++) {
    const range = selection.getRangeAt(i);
    if (range.collapsed) continue;
    const start = range.startContainer;
    const end = range.endContainer;
    const walker = document.createTreeWalker(range.commonAncestorContainer, NodeFilter.SHOW_ALL);
    const ancestor = (start.nodeType === Node.ELEMENT_NODE ? start : start.parentElement)?.closest('a[href]');
    if (ancestor && range.intersectsNode(ancestor) && !append(selectedURL(ancestor.getAttribute('href')))) return {tooLarge:true};

    // Begin at the selected boundary, and stop at the end boundary. In
    // particular, a selection spanning two sections never queries the page.
    walker.currentNode = start;
    let node = start;
    if (start.nodeType === Node.ELEMENT_NODE) {
      node = start.childNodes[range.startOffset];
      if (!node) {
        while (walker.currentNode !== range.commonAncestorContainer && !node) {
          node = walker.nextSibling();
          if (!node) walker.parentNode();
        }
      }
    }
    if (node) walker.currentNode = node;
    let plain = '';
    let visited = 0;
    while (node && range.comparePoint(node, 0) <= 0) {
      if (++visited > 20000) return {tooLarge:true};
      if (node !== ancestor && node.nodeType === Node.ELEMENT_NODE &&
          node.matches('a[href]') && range.intersectsNode(node) &&
          !append(selectedURL(node.getAttribute('href')))) return {tooLarge:true};
      if (node.nodeType === Node.TEXT_NODE) {
        if (!node.parentElement?.closest('a[href]')) {
          const from = node === start ? range.startOffset : 0;
          const to = node === end ? range.endOffset : node.length;
          if (plain.length + to - from > 65536) return {tooLarge:true};
          plain += node.substringData(from, to - from) + ' ';
        } else {
          plain += ' ';
        }
      }
      if (node === end && (node.nodeType !== Node.ELEMENT_NODE || range.endOffset === 0)) break;
      node = walker.nextNode();
    }
    for (const token of plain.split(/\s+/)) {
      if (!/^https?:\/\//i.test(token) || !selectedURL(token)) continue;
      if (!append(token)) return {tooLarge:true};
    }
  }
  return {links};
}

const httpOrigin = raw => {
  try {
    const parsed = new URL(raw);
    return ['http:', 'https:'].includes(parsed.protocol) ? parsed.origin : null;
  } catch {
    return null;
  }
};

export async function importLinkSelection(browser, sender, capturedHeaders, send) {
  if (sender.id !== browser.runtime.id || sender.tab ||
      sender.url !== browser.runtime.getURL('popup.html')) return null;
  const [tab] = await browser.tabs.query({active:true,currentWindow:true});
  if (!Number.isInteger(tab?.id) || tab.incognito) return 'selectionUnavailable';
  const target = {tabId:tab.id,frameId:0};
  const frame = await browser.webNavigation.getFrame(target);
  const origin = httpOrigin(frame?.url);
  if (!origin || !frame.documentId || frame.documentLifecycle !== 'active') return 'selectionUnavailable';
  const [read] = await browser.scripting.executeScript({
    target:{tabId:tab.id,documentIds:[frame.documentId]},func:readLinkSelection,world:'ISOLATED',
  });
  if (read?.frameId !== 0 || read.documentId !== frame.documentId) return 'selectionUnavailable';
  if (read.result?.tooLarge) return 'selectionTooLarge';
  if (!Array.isArray(read.result?.links) || !read.result.links.length) return 'selectionEmpty';
  if (read.result.links.length > 200) return 'selectionTooLarge';
  const current = await browser.webNavigation.getFrame(target);
  if (current?.documentId !== frame.documentId || current.url !== frame.url ||
      current.documentLifecycle !== 'active') return 'selectionUnavailable';
  const items = read.result.links.map(url => {
    if (httpOrigin(url) !== origin) return {url};
    const header = capturedHeaders?.({...target,documentId:frame.documentId,url,origin});
    return header && Object.keys(header).length
      ? {url,header,context:frame.documentId}
      : {url};
  });
  const params = {version:1,items};
  if (new TextEncoder().encode(JSON.stringify(params)).length > 65536) return 'selectionTooLarge';
  await send({method:'batchPreview',params});
  return 'selectionOpened';
}
