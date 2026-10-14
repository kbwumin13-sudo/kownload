import test from 'node:test';
import assert from 'node:assert/strict';
import {pinnedCandidate} from '../lib.js';

test('a clicked iframe player selects one matching resource from that frame', () => {
  const items = [{id:'main',frameId:0,url:'https://cdn.test/video.mp4'},
    {id:'frame',frameId:7,url:'https://cdn.test/video.mp4'}];
  assert.equal(pinnedCandidate(items,'https://cdn.test/video.mp4',7).id,'frame');
  assert.equal(pinnedCandidate(items,'https://cdn.test/video.mp4',0).id,'main');
  assert.equal(pinnedCandidate(items,'https://cdn.test/video.mp4',null).id,'main');
  assert.equal(pinnedCandidate(items,'https://cdn.test/video.mp4',99),undefined);
  assert.equal(pinnedCandidate(items,'https://cdn.test/missing.mp4',7),undefined);
});
