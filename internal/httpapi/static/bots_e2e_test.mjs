import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const elements = new Map();
const $ = id => {
  if (!elements.has(id)) elements.set(id, {
    value:'', hidden:false, disabled:false, children:[],
    replaceChildren(){this.children=[]}, appendChild(child){this.children.push(child)},
    setAttribute(){}, addEventListener(){}, classList:{remove(){}}, showModal(){}, close(){},
  });
  return elements.get(id);
};
let bots = [{id:'default',name:'Primary'}, {id:'bot-two',name:'Second'}];
let confirmed = false, confirmation = '', fail = false, deletes = 0, redirect = '';
const context = vm.createContext({
  $, BOT_BASE:'', state:{config:{}}, document:{createElement:()=>({})}, console,
  location:{replace:url=>{redirect=url}}, ensureSettingsAccess:async()=>true,
  renderAgentSelect(){}, larkAppConsoleURL:()=>'',
  confirm:text=>{confirmation=text;return confirmed}, alert(){},
  api:async(path,options={})=>{
    if (options.method === 'DELETE') {
      if (fail) throw new Error('备份失败');
      deletes++; bots=bots.filter(bot=>bot.id!==JSON.parse(options.body).id);
      return {backup_path:'/isolated-test/backup'};
    }
    if (path.includes('delete_id=')) return {sessions:3,running:1};
    return bots;
  },
});
vm.runInContext(fs.readFileSync(new URL('./bots.js',import.meta.url),'utf8'),context);
await vm.runInContext('loadBots();',context);
await vm.runInContext('openBotEditor(false)',context);
assert.equal($('bot-delete').hidden,true);
await vm.runInContext('openBotEditor(true)',context);
assert.equal($('bot-delete').hidden,false);
await vm.runInContext('deleteBot()',context);
assert.equal(deletes,0,'cancel must not delete');
assert.match(confirmation,/Primary/);
assert.match(confirmation,/3 个会话，1 个运行中任务/);
assert.equal($('bot-delete').disabled,false);
confirmed=true; fail=true;
await assert.rejects(vm.runInContext('deleteBot()',context),/备份失败/);
assert.equal(deletes,0);
assert.equal(redirect,'');
assert.equal($('bot-delete').disabled,false);
fail=false;
await Promise.all([vm.runInContext('deleteBot()',context),vm.runInContext('deleteBot()',context)]);
assert.equal(deletes,1,'pending delete must not submit twice');
assert.equal(redirect,'/bots/bot-two/');
bots=[];
await vm.runInContext('loadBots()',context);
assert.equal($('bot-select').hidden,true);
assert.equal($('bot-settings').hidden,true);
assert.equal($('bot-add').disabled,false);
console.log('bot deletion UI checks ok');
