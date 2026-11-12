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
let bots = [{id:'default',name:'Primary',system_prompt:'机器人 A\n多行指令'}, {id:'bot-two',name:'Second'}];
let confirmed = false, confirmation = '', fail = false, deletes = 0, redirect = '';
const context = vm.createContext({
  $, BOT_BASE:'', state:{config:{}}, document:{createElement:()=>({})}, console,
  AbortController, Date, setTimeout:fn=>{queueMicrotask(fn);return 0},clearTimeout(){},
  location:{replace:url=>{redirect=url},assign:url=>{redirect=url}}, ensureSettingsAccess:async()=>true,
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
assert.equal($('bot-system-prompt').value,'');
assert.equal($('bot-delete').hidden,true);
await vm.runInContext('openBotEditor(true)',context);
assert.equal($('bot-system-prompt').value,'机器人 A\n多行指令');
$('bot-agent').value='codex';
$('bot-system-prompt').value='保留新指令\n第二行';
assert.equal(vm.runInContext('readBot()',context).system_prompt,'保留新指令\n第二行');
vm.runInContext('setBotPending(true)',context);
assert.equal($('bot-system-prompt').disabled,true);
vm.runInContext('setBotPending(false)',context);
$('bot-system-prompt').value='';
assert.equal(vm.runInContext('readBot()',context).system_prompt,'');
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

for (const mode of ['success','permissions','created','unknown','cancel','registration-error','browser-warning']) {
  vm.runInContext('editingBotID="";botSavePending=false;botCreationBlocked=false',context);
  $('bot-name').value='Fallback bot';$('bot-agent').value='codex';$('bot-directory').value='/workspace';
  $('bot-app-secret').value='';
  let begins=0,polls=0,saves=0;
  redirect='';
  context.api=async(path,options={})=>{
    if(path==='/api/lark-app-registration') {
      begins++;
      if(mode==='registration-error')throw new Error('registration unavailable');
      return {verification_uri_complete:'https://open.feishu.cn/page/cli?user_code=test',device_code:'private-code',brand:'feishu',expires_in:3600,interval:5};
    }
    if(path==='/api/lark-app-registration/poll') {
      polls++;
      if(mode==='cancel') { $('bot-cancel').onclick();return {pending:true}; }
      if(polls===1)return {pending:true};
      return {app_id:'cli_registered',app_secret:'test-secret',lark_notify_receive_id:'ou_owner'};
    }
    if(path==='/api/bots') {
      if (!options.method) return [{id:'bot-registered',app_id:'cli_registered',app_secret:'test-secret',receive_id:'ou_owner'}];
      saves++;
      const bot=JSON.parse(options.body);
      assert.equal(bot.app_id,'cli_registered');assert.equal(bot.default_agent_id,'codex');assert.equal(bot.default_workspace_dir,'/workspace');
      return {id:'bot-registered',app_id:'cli_registered',warning:mode==='permissions'?'尚未开通功能权限：im:message.group_msg':''};
    }
    throw new Error('unexpected API '+path);
  };
  context.failureMode=mode;
  vm.runInContext(`streamBotSetup=async(bot,receive)=>{
    if(failureMode==='browser-warning'){receive({stage:'done',bot_id:'bot-registered',app_id:'cli_registered',warning:'尚未开通功能权限：im:message.group_msg'});return;}
    if(failureMode==='created')receive({stage:'configuring',app_id:'cli_existing'});
    receive({stage:'error',error:'browser failed',registration_fallback:failureMode!=='unknown'});
  }`,context);
  if(mode==='success'||mode==='permissions'||mode==='browser-warning')await vm.runInContext('createBot()',context);
  else if(mode==='cancel')await vm.runInContext('createBot()',context);
  else await assert.rejects(vm.runInContext('createBot()',context),mode==='registration-error'?/registration unavailable/:/browser failed/);
  assert.equal(begins,['created','unknown','browser-warning'].includes(mode)?0:1,mode);
  assert.equal(saves,['success','permissions'].includes(mode)?1:0,mode);
  assert.equal($('bot-scan').disabled,!['unknown'].includes(mode),mode);
  assert.equal($('bot-save').disabled,false);
  assert.equal($('bot-registration').hidden,true);
  if(mode==='success')assert.equal(redirect,'/bots/bot-registered/');
  else assert.equal(redirect,'');
  if(mode==='permissions'||mode==='browser-warning') {
    assert.match($('bot-error').textContent,/已保存/);
    assert.match($('bot-error').textContent,/im:message.group_msg/);
    assert.equal($('bot-created-app').hidden,false);
    assert.equal($('bot-open').href,'/bots/bot-registered/');
    assert.equal(vm.runInContext('editingBotID',context),'bot-registered');
    assert.equal($('bot-app-id').value,'cli_registered');
    assert.equal($('bot-app-secret').value,'test-secret');
    assert.equal($('bot-app-fields').hidden,false);
    assert.equal($('bot-save').hidden,false);
  }
}
console.log('bot QR fallback, cancellation and permission failure checks ok');
