import test from 'node:test';
import assert from 'node:assert/strict';
import { emptyModel, modelReducer } from './acp-model.ts';
const description=(revision='40',from='1',id='c1')=>({conversation_id:id,revision,retained_from_order:from,head_order:'150'});
const entry=(id,revision)=>({entry_id:`e-${id}`,order:String(id),entry_revision:String(revision),type:'tool',tool:{status:'completed'}});
const page=(conversation,entries)=>({conversation,entries,through_order:'150',has_more:false,range_evicted:false});
const select=(d=description())=>modelReducer(emptyModel,{type:'select',conversation:d});
const read=(cache,conversation,entries,direction='latest')=>modelReducer(cache,{type:'page',page:page(conversation,entries),direction});
test('older page accepts unseen entries without replacing a newer tool',()=>{
 let cache=read(select(),description('46'),[entry(120,46)]);
 cache=read(cache,description('43'),[entry(90,42),entry(120,43)],'older');
 assert.equal(cache.entries['e-120'].entry_revision,'46');
 assert.equal(cache.entries['e-90'].entry_revision,'42');
 assert.equal(cache.conversation.revision,'46');
});
test('older responses cannot resurrect evicted entries or change generation',()=>{
 let cache=read(select(),description('47','100'),[entry(120,46)]);
 cache=read(cache,description('43'),[entry(90,42)],'older');
 assert.equal(cache.entries['e-90'],undefined);
 cache=modelReducer(cache,{type:'select',conversation:description('1','1','c2')});
 const before=cache;
 cache=read(cache,description('48'),[entry(130,48)]);
 assert.equal(cache,before);
});
test('new latest page never clears older entry refresh obligations',()=>{
 let cache=read(select(),description(),[entry(10,30)]);
 cache=modelReducer(cache,{type:'notify',change:{conversation_id:'c1',previous_revision:'40',revision:'46',changed_entry_ids:['e-10']}});
 cache=read(cache,description('50'),[entry(120,50)]);
 assert.equal(cache.dirty['e-10'],'46');
 cache=modelReducer(cache,{type:'get',result:{conversation:description('49'),entries:[entry(10,44)],missing:[],unprocessed_entry_ids:[]}});
 assert.equal(cache.dirty['e-10'],undefined);
 assert.equal(cache.entries['e-10'].entry_revision,'44');
});
test('notifications during a read survive its earlier response; integer strings stay exact',()=>{
 let cache=read(select(description('9007199254740993')),description('9007199254740993'),[entry(10,'9007199254740993')]);
 cache=modelReducer(cache,{type:'notify',change:{conversation_id:'c1',previous_revision:'9007199254740993',revision:'9007199254740995',changed_entry_ids:['e-10']}});
 cache=read(cache,description('9007199254740994'),[entry(10,'9007199254740994')]);
 assert.equal(cache.dirty['e-10'],'9007199254740995');
 assert.equal(cache.entries['e-10'].entry_revision,'9007199254740994');
});

const entriesBetween = (from, through, revision) => Array.from({ length: through - from + 1 }, (_, index) => entry(from + index, revision));
const historyDescription = (head) => ({ ...description(String(head)), head_order: String(head) });
function historyPage(cache, from, through, head, direction = 'latest') {
 return modelReducer(cache, { type: 'page', direction, page: {
  conversation: historyDescription(head), entries: entriesBetween(from, through, head), through_order: String(head),
  has_more: from > 1, next_cursor: from > 1 ? `before-${from}` : undefined, range_evicted: false,
 } });
}

test('latest refresh leaves the unread middle reachable after all dirty entries are refreshed', () => {
 let cache = historyPage(select(historyDescription(10)), 1, 10, 10);
 cache = modelReducer(cache, { type: 'notify', change: { conversation_id: 'c1', previous_revision: '10', revision: '300', invalidates_all: true } });
 // Discovery of the new head must not be confused with having read its bodies.
 cache = modelReducer(cache, { type: 'select', conversation: historyDescription(300) });
 cache = historyPage(cache, 251, 300, 300);
 assert.equal(cache.dirty['e-10'], '300');
 cache = modelReducer(cache, { type: 'get', result: {
  conversation: historyDescription(300), entries: entriesBetween(1, 10, 10), missing: [], unprocessed_entry_ids: [],
 } });
 assert.deepEqual(cache.dirty, {});
 assert.equal(cache.latestRevision, undefined);
 assert.equal(cache.cursor, 'before-251');
 for (let before = 251; before > 1; before -= 50) {
  assert.equal(cache.cursor, `before-${before}`);
  cache = historyPage(cache, before - 50, before - 1, 300, 'older');
 }
 assert.equal(cache.cursor, undefined);
 assert.deepEqual(Object.keys(cache.entries).sort(), entriesBetween(1, 300, 300).map((item) => item.entry_id).sort());
});

test('overlapping and adjacent latest refreshes preserve paging progress across repeated gaps', () => {
 let cache = historyPage(select(historyDescription(150)), 101, 150, 150);
 cache = historyPage(cache, 51, 100, 150, 'older');
 cache = historyPage(cache, 131, 180, 180);
 assert.equal(cache.cursor, 'before-51');
 cache = historyPage(cache, 181, 230, 230);
 assert.equal(cache.cursor, 'before-51');
 cache = historyPage(cache, 351, 400, 400);
 assert.equal(cache.cursor, 'before-351');
 cache = historyPage(cache, 301, 350, 400, 'older');
 cache = historyPage(cache, 381, 430, 430);
 assert.equal(cache.cursor, 'before-301');
 cache = historyPage(cache, 481, 530, 530);
 assert.equal(cache.cursor, 'before-481');
 for (let before = 481; before > 1; before -= 40) {
  assert.equal(cache.cursor, `before-${before}`);
  cache = historyPage(cache, before - 40, before - 1, 530, 'older');
 }
 assert.equal(cache.cursor, undefined);
 assert.equal(Object.keys(cache.entries).length, 530);
});

test('an older latest snapshot cannot move coverage backward; a fully read retained window clears the cursor', () => {
 let cache = historyPage(select(historyDescription(300)), 251, 300, 300);
 cache = historyPage(cache, 201, 250, 300, 'older');
 cache = historyPage(cache, 1, 10, 10);
 assert.equal(cache.latestThrough, '300');
 assert.equal(cache.cursor, 'before-201');
 cache = modelReducer(cache, { type: 'page', direction: 'latest', page: {
  conversation: { ...historyDescription(301), retained_from_order: '301' }, entries: [entry(301, 301)],
  through_order: '301', has_more: false, range_evicted: true,
 } });
 assert.equal(cache.cursor, undefined);
 assert.deepEqual(Object.keys(cache.entries), ['e-301']);
});
