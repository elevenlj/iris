const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../cmd/feishu_setup.js'),'utf8');
const required=['im:message','im:message:send_as_bot','im:message.group_msg','im:resource','im:chat:create','im:chat:read','im:chat:update','im:chat.members:read','im:chat.members:write_only','im:chat.members:bot_access','cardkit:card:read','cardkit:card:write','contact:user.base:readonly','contact:user.id:readonly'];

async function scenario(failure=''){
 const calls=[];
 const context=vm.createContext({location:{hostname:'open.feishu.cn'},window:{csrfToken:'csrf',user:{id:'creator',email:'creator@example.com'}},crypto:{randomUUID:()=> 'request-id'},FormData,Blob,Map,document:{createElement:()=>({getContext:()=>({fillRect(){},fillText(){}}),toBlob:resolve=>resolve(new Blob(['png']))})},fetch:async(url,options)=>{
  const body=options.body instanceof FormData?options.body:JSON.parse(options.body);
  calls.push({url,body});
  let data={};
  if(url.endsWith('/upload/image'))data={url:'https://example.com/icon.png'};
  else if(url.endsWith('/upsert_by_template'))data={ClientID:'cli_new'};
  else if(url.includes('/scope/all/'))data={appScopes:required.filter(s=>failure!=='scope'||s!=='im:message.group_msg').map((name,i)=>({name,id:'app-'+i})),userScopes:[{name:'im:message.group_msg',id:'wrong-user-id'}]};
  else if(url.includes('/privilege/all/'))data={privileges:[{isRequired:true,schemaType:1,organizationType:1,bizId:'contacts',schemaContent:{selectionExpressionSchemaContent:{fields:[{id:'users',data_source:{type:'select_staff'},operators:['in']}]}},content:'{"mode":"all"}'}]};
  else if(url.endsWith('/event/cli_new'))data={eventMode:failure==='event'?1:4,appEvents:['im.message.receive_v1','im.chat.member.bot.added_v1','im.message.reaction.created_v1','im.message.reaction.deleted_v1']};
  else if(url.endsWith('/callback/cli_new'))data={callbackMode:4,callbacks:failure==='callback'?[]:['card.action.trigger']};
  else if(url.includes('/app_version/create/'))data={versionId:'version-1'};
  else if(url.includes('/app_version/list/'))data={versions:[{versionId:'version-1',status:failure==='approval'?0:1}]};
  else if(url.includes('/secret/'))data={secret:'never-log-secret'};
  const broken=failure==='transport'&&url.endsWith('/upsert_by_template');
  return {ok:!broken,status:broken?503:200,json:async()=>({code:0,data})};
 }});
 vm.runInContext(source,context);
 let result,error;
 try{result=await context.irisCreateFeishuApp('My bot')}catch(e){error=e}
 return {calls,result,error};
}
(async()=>{
 const success=await scenario();assert.ifError(success.error);assert.equal(success.result.app_id,'cli_new');
 const update=success.calls.find(c=>c.url.includes('/scope/update/')).body;
 assert.equal(update.appScopeIDs.length,required.length);assert.equal(update.userScopeIDs.length,0);assert(!update.appScopeIDs.includes('wrong-user-id'));
 const version=success.calls.find(c=>c.url.includes('/app_version/create/')).body;
 assert.deepEqual(Array.from(version.visibleSuggest.members),['creator']);assert.equal(version.visibleSuggest.isAll,0);
 const privilege=JSON.parse(success.calls.find(c=>c.url.includes('/privilege/update/')).body.privileges[0].content);
 assert.equal(privilege.mode,'part');assert.equal(JSON.parse(privilege.filters[0].value)[0].mode,'availability_of_app');
 for(const failure of ['scope','event','callback','approval','transport']){
  const result=await scenario(failure);assert(result.error,failure+' should fail closed');
  assert.equal(result.calls.filter(c=>c.url.endsWith('/upsert_by_template')).length,1,'must not retry app creation');
  assert(!result.error.message.includes('never-log-secret'),'error leaked credentials');
  assert(!result.result,'must not report incomplete setup as successful');
 }
 console.log('PASS: creation, application-scoped permissions, events/callback readback, publication, owner visibility, failures');
})().catch(error=>{console.error(error);process.exitCode=1});
