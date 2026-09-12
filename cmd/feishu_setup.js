// Runs only inside the dedicated, user-authenticated Feishu console window.
// Console contracts: https://github.com/deepcoldy/botmux/blob/master/src/setup/open-platform-automation.ts
async function irisCreateFeishuApp(name,mode='create',existingID='') {
  if (!['open.feishu.cn', 'open.larkoffice.com'].includes(location.hostname) || !window.csrfToken || !window.user?.id) throw new Error('请先完成飞书开放平台登录');
  const request = async (path, body = {}) => {
    const response = await fetch(path, {method:'POST',credentials:'same-origin',headers:body instanceof FormData ? {'x-csrf-token':window.csrfToken} : {'Content-Type':'application/json','x-csrf-token':window.csrfToken},body:body instanceof FormData ? body : JSON.stringify(body)});
    if (!response.ok) throw new Error(`开放平台请求失败：${path}（HTTP ${response.status}，结果可能未知，请勿重复创建）`);
    const result = await response.json();
    if (result.code !== 0) throw new Error(`开放平台请求失败：${path}（code ${result.code}）`);
    return result.data || result;
  };
  if(mode==='list') {
    const apps=[];
    for(let cursor=0;cursor<500;cursor+=100) {
      const data=await request('/developers/v1/app/list',{Count:100,Cursor:cursor,QueryFilter:{}});
      const rows=data.apps||[];
      for(const app of rows) {
        const app_id=app.clientId||app.client_id||app.appId||app.app_id||app.appID;
        if(/^cli_[a-zA-Z0-9]+$/.test(app_id||''))apps.push({app_id,name:app.name||app.appName||app.app_name||app_id});
      }
      if(rows.length<100||cursor+100>=data.totalCount)break;
      if(cursor===400)throw new Error('应用数量超过 500，请使用手动填写接入');
    }
    return {apps};
  }
  if(mode==='connect') {
    if(!/^cli_[a-zA-Z0-9]+$/.test(existingID))throw new Error('无效的 App ID');
    window.irisSetupProgress?.(JSON.stringify({stage:'verifying',message:'正在读取已有应用的接入配置…',app_id:existingID}));
    // Read existing credentials only; never reset secrets or change its release.
    const versions=await request(`/developers/v1/app_version/list/${existingID}`);
    if(!(versions.versions||[]).some(v=>v.versionStatus===2))throw new Error('该应用尚未上线，请先在应用后台确认发布状态');
    const credentials=await request(`/developers/v1/secret/${existingID}`);
    if(!credentials.secret)throw new Error('未能读取应用凭证，请确认当前账号有该应用的管理权限');
    return {app_id:existingID,app_secret:credentials.secret,app_name:name,owner_email:window.user.email||''};
  }
  if(mode!=='create')throw new Error('不支持的应用操作');
  // Native canvas supplies a small Iris icon; no external image or dependency.
  const canvas=document.createElement('canvas');canvas.width=canvas.height=512;
  const paint=canvas.getContext('2d');paint.fillStyle='#42694c';paint.fillRect(0,0,512,512);paint.fillStyle='#fff';paint.font='italic 180px Georgia';paint.textAlign='center';paint.fillText('iris',256,306);
  const form=new FormData();form.append('file',await new Promise(resolve=>canvas.toBlob(resolve,'image/png')),'iris.png');form.append('uploadType','4');form.append('isIsv','false');form.append('scale',JSON.stringify({width:512,height:512}));
  const uploaded=await request('/developers/v1/app/upload/image',form);
  if (!uploaded.url) throw new Error('图标上传未返回地址');
  const created=await request('/developers/v1/manifest/upsert_by_template',{appManifestTemplateID:'developer_console',createAppUserCustomField:{i18n:{zh_cn:{name,description:'Iris AI 助理'}},avatar:uploaded.url,primaryLang:'zh_cn'},cid:crypto.randomUUID(),HTTPHead:{}});
  const id=created.ClientID||created.clientId||created.clientID||created.appId;
  if (!/^cli_[a-zA-Z0-9]+$/.test(id||'')) throw new Error('创建结果未返回 App ID，请在应用后台确认结果，不要重复创建');
  const progress=(stage,message)=>window.irisSetupProgress?.(JSON.stringify({stage,message,app_id:id}));
  try {
    progress('configuring','应用已创建，正在配置权限与消息事件…');
    const required=['im:message','im:message:send_as_bot','im:message.group_msg','im:resource','im:chat:create','im:chat:read','im:chat:update','im:chat.members:read','im:chat.members:write_only','im:chat.members:bot_access','cardkit:card:read','cardkit:card:write','contact:user.base:readonly','contact:user.id:readonly'];
    const catalog=await request(`/developers/v1/scope/all/${id}`);
    const scopes=new Map();
    const visit=(value,bucket='')=>{
      if (!value||typeof value!=='object') return;
      const scope=value.scope_name||value.scopeName||value.name||value.key||value.scopeKey;
      const scopeID=value.id||value.scope_id||value.scopeId||value.scopeID;
      if (scope&&scopeID&&bucket!=='user') scopes.set(scope,String(scopeID));
      for(const [key,child] of Object.entries(value)) if(child&&typeof child==='object') visit(child,/user/i.test(key)?'user':/app|tenant|client/i.test(key)?'app':bucket);
    };
    visit(catalog);
    const missing=required.filter(scope=>!scopes.has(scope));
    if(missing.length) throw new Error('权限目录中缺少：'+missing.join('、'));
    await request(`/developers/v1/scope/update/${id}`,{clientId:id,appScopeIDs:required.map(scope=>scopes.get(scope)),userScopeIDs:[],scopeIds:[],operation:'add',isDeveloperPanel:true});
    // Restrict required staff data to the app's availability, never the whole tenant.
    const ranges=await request(`/developers/v1/privilege/all/${id}`);
    const narrowed=[];
    for(const range of ranges.privileges||[]) {
      if(!range.isRequired||range.schemaType!==1||range.organizationType!==1) continue;
      let schema=range.schemaContent;
      if(!schema&&range.schema) schema=JSON.parse(range.schema).schema_content;
      const fields=(schema?.selectionExpressionSchemaContent||schema?.SelectionExpressionSchemaContent)?.fields||[];
      if(!fields.length||fields.some(field=>!field.id||field.data_source?.type!=='select_staff'||!field.operators?.includes('in'))) continue;
      const current=range.content?JSON.parse(range.content):{};
      if(current.mode==='part'&&current.filters?.length) continue;
      narrowed.push({...range,content:JSON.stringify({biz_id:range.bizId,mode:'part',resource:range.resource||'',filters:fields.map(field=>({field:field.id,value:JSON.stringify([{mode:'availability_of_app',members:[],departments:[],groups:[]}]),operator:'in'})),expression:fields.map((_,i)=>String(i+1)).join(' and ')})});
    }
    if(narrowed.length) await request(`/developers/v1/privilege/update/${id}`,{clientId:id,privileges:narrowed});
    await request(`/developers/v1/robot/switch/${id}`,{clientId:id,enable:true});
    await request(`/developers/v1/event/switch/${id}`,{clientId:id,eventMode:4});
    const events=['im.message.receive_v1','im.chat.member.bot.added_v1','im.message.reaction.created_v1','im.message.reaction.deleted_v1'];
    await request(`/developers/v1/event/update/${id}`,{clientId:id,eventMode:4,operation:'add',events:[],appEvents:events,userEvents:[]});
    await request(`/developers/v1/callback/switch/${id}`,{clientId:id,callbackMode:4});
    await request(`/developers/v1/callback/update/${id}`,{clientId:id,callbackMode:4,operation:'add',callbacks:['card.action.trigger']});
    const eventState=await request(`/developers/v1/event/${id}`,{needEventDetail:true});
    const callbackState=await request(`/developers/v1/callback/${id}`);
    const eventIDs=[...(eventState.events||[]),...(eventState.appEvents||[]),...(eventState.appEventDetails||[]).flatMap(group=>group.items||[])].map(item=>typeof item==='string'?item:item.id);
    if(eventState.eventMode!==4||events.some(event=>!eventIDs.includes(event))) throw new Error('消息事件配置回读失败');
    if(callbackState.callbackMode!==4||!(callbackState.callbacks||[]).some(item=>(item.id||item)==='card.action.trigger')) throw new Error('卡片回调配置回读失败');
    const visibility={departments:[],members:[String(window.user.id)],groups:[],isAll:0};
    progress('publishing','权限配置完成，正在发布应用…');
    const version=await request(`/developers/v1/app_version/create/${id}`,{appVersion:'1.0.0',mobileDefaultAbility:'bot',pcDefaultAbility:'bot',changeLog:'Iris initial release',visibleSuggest:visibility,blackVisibleSuggest:{departments:[],members:[],groups:[],isAll:0}});
    const versionID=version.versionId||version.version_id||version.id||version.appVersion?.versionId;
    if(!versionID) throw new Error('未返回版本 ID，无法确认发布');
    await request(`/developers/v1/publish/commit/${id}/${versionID}`,{clientId:id});
    progress('publishing','已提交发布，正在确认上线状态…');
    let released;
    for(let attempt=0;attempt<10;attempt++) {
      const versions=await request(`/developers/v1/app_version/list/${id}`);
      released=(versions.versions||[]).find(v=>String(v.versionId||v.id)===String(versionID));
      // Developer-console enums differ from the public application's status field.
      if(released?.versionStatus===2) break;
      if(attempt<9) await new Promise(resolve=>setTimeout(resolve,2000));
    }
    if(released?.versionStatus!==2) throw new Error(released?.versionStatus===1?'发布已提交，正在等待企业审批':'发布已提交，但暂未确认上线状态，请在应用后台查看');
    progress('verifying','应用已上线，正在获取接入配置…');
    const credentials=await request(`/developers/v1/secret/${id}`);
    if(!credentials.secret) throw new Error('应用未返回 Secret');
    return {app_id:id,app_secret:credentials.secret,app_name:name,owner_email:window.user.email||''};
  }catch(error){throw new Error(`应用 ${id} 已创建；${error.message}。请在应用后台处理，勿重复新建。`)}
}
