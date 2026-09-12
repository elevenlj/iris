// Runs only inside the dedicated, user-authenticated Feishu console window.
// Console contracts: https://github.com/deepcoldy/botmux/blob/master/src/setup/open-platform-automation.ts
async function irisCreateFeishuApp(name) {
  if (!['open.feishu.cn', 'open.larkoffice.com'].includes(location.hostname) || !window.csrfToken || !window.user?.id) throw new Error('请先完成飞书开放平台登录');
  const request = async (path, body = {}) => {
    const response = await fetch(path, {method:'POST',credentials:'same-origin',headers:body instanceof FormData ? {'x-csrf-token':window.csrfToken} : {'Content-Type':'application/json','x-csrf-token':window.csrfToken},body:body instanceof FormData ? body : JSON.stringify(body)});
    if (!response.ok) throw new Error(`开放平台请求失败：${path}（HTTP ${response.status}，结果可能未知，请勿重复创建）`);
    const result = await response.json();
    if (result.code !== 0) throw new Error(`开放平台请求失败：${path}（code ${result.code}）`);
    return result.data || result;
  };
  // Native canvas supplies a small Iris icon; no external image or dependency.
  const canvas=document.createElement('canvas');canvas.width=canvas.height=512;
  const paint=canvas.getContext('2d');paint.fillStyle='#42694c';paint.fillRect(0,0,512,512);paint.fillStyle='#fff';paint.font='italic 180px Georgia';paint.textAlign='center';paint.fillText('iris',256,306);
  const form=new FormData();form.append('file',await new Promise(resolve=>canvas.toBlob(resolve,'image/png')),'iris.png');form.append('uploadType','4');form.append('isIsv','false');form.append('scale',JSON.stringify({width:512,height:512}));
  const uploaded=await request('/developers/v1/app/upload/image',form);
  if (!uploaded.url) throw new Error('图标上传未返回地址');
  const created=await request('/developers/v1/manifest/upsert_by_template',{appManifestTemplateID:'developer_console',createAppUserCustomField:{i18n:{zh_cn:{name,description:'Iris AI 助理'}},avatar:uploaded.url,primaryLang:'zh_cn'},cid:crypto.randomUUID(),HTTPHead:{}});
  const id=created.ClientID||created.clientId||created.clientID||created.appId;
  if (!/^cli_[a-zA-Z0-9]+$/.test(id||'')) throw new Error('创建结果未返回 App ID，请在应用后台确认结果，不要重复创建');
  try {
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
    const version=await request(`/developers/v1/app_version/create/${id}`,{appVersion:'1.0.0',mobileDefaultAbility:'bot',pcDefaultAbility:'bot',changeLog:'Iris initial release',visibleSuggest:visibility,blackVisibleSuggest:{departments:[],members:[],groups:[],isAll:0}});
    const versionID=version.versionId||version.version_id||version.id||version.appVersion?.versionId;
    if(!versionID) throw new Error('未返回版本 ID，无法确认发布');
    await request(`/developers/v1/publish/commit/${id}/${versionID}`,{clientId:id});
    const versions=await request(`/developers/v1/app_version/list/${id}`);
    const released=(versions.versions||[]).find(v=>String(v.versionId||v.id)===String(versionID));
    if(!released||released.status!==1) throw new Error('版本尚未上线，可能需要企业审批，请在应用后台查看');
    const credentials=await request(`/developers/v1/secret/${id}`);
    if(!credentials.secret) throw new Error('应用未返回 Secret');
    return {app_id:id,app_secret:credentials.secret,app_name:name,owner_email:window.user.email||''};
  }catch(error){throw new Error(`应用 ${id} 已创建；${error.message}。请在应用后台处理，勿重复新建。`)}
}
