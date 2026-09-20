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
