const assert=require('node:assert/strict');
const {chromium}=require('playwright-core');
const {pathToFileURL}=require('node:url');
const path=require('node:path');
const base=process.env.IRIS_TEST_URL;
(async()=>{
 const browser=await chromium.launch({executablePath:process.env.CHROME_PATH||'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1000}});
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(base);
  await page.locator('#auth-password').fill('iris-browser-password');
  await page.locator('#auth-confirm-password').fill('iris-browser-password');
  await page.locator('#auth-submit').click();
  await page.waitForFunction(()=>Boolean(window.irisApp&&document.querySelector('#bot-select')?.options.length===2));
  for (const selector of ['#new-session','#composer','#composer-input','#quick-list','#help-open','.notify-input','.notify-row','#config-prev','#config-next']) assert.equal(await page.locator(selector).count(),0,selector+' must not exist');
  await page.waitForFunction(()=>window.irisApp.state.socket?.readyState===WebSocket.OPEN);
  await page.locator('#terminal').click();
  await page.keyboard.type('printf "ROOT_TERMINAL_%s\\n" OK');
  await page.keyboard.press('Enter');
  await page.waitForFunction(()=>document.querySelector('#terminal').textContent.includes('ROOT_TERMINAL_OK'));
  if(await page.locator('#config-dialog').isVisible())await page.locator('#config-cancel').click();
  await page.locator('#bot-settings').click();
  await page.locator('#bot-dialog').waitFor({state:'visible'});
  assert.equal(await page.locator('#bot-name').inputValue(),'开发助手');
  assert.equal(await page.locator('#bot-app-name').inputValue(),'开发助手应用');
  assert.equal(await page.locator('#bot-app-secret').getAttribute('type'),'password');
  assert.equal(await page.locator('#bot-dialog').getByText('测试飞书配置',{exact:true}).count(),0);
  assert.equal(await page.locator('#bot-dialog').getByText('自动配置权限',{exact:true}).count(),0);
  assert(await page.locator('.bot-app-heading #bot-console').isVisible());
  await page.locator('#bot-name').fill('开发助手改名');
  await page.locator('#bot-save').click();
  await page.waitForFunction(()=>document.querySelector('#bot-select')?.selectedOptions[0]?.textContent==='开发助手改名');
  assert.equal((await (await page.request.get(base+'/api/bots')).json())[0].app_name,'开发助手应用');
  await page.locator('#bot-select').selectOption('bot-second');
  await page.waitForURL('**/bots/bot-second/');
  await page.waitForFunction(()=>window.irisApp?.state.socket?.readyState===WebSocket.OPEN);
  assert.equal(await page.locator('.session-name').innerText(),'Other terminal');
  assert(await page.evaluate(()=>window.irisApp.state.socket.url.includes('/bots/bot-second/api/sessions/')));
  await page.locator('#terminal').click();
  await page.keyboard.type('printf "SECOND_TERMINAL_%s\\n" OK');await page.keyboard.press('Enter');
  await page.waitForFunction(()=>document.querySelector('#terminal').textContent.includes('SECOND_TERMINAL_OK'));
  assert.equal(await page.locator('#session-agent').textContent(),'Shell');
  assert(await page.locator('#session-directory').textContent());
  const root=await page.request.get(base+'/api/sessions/sess-1/output');assert(!(await root.text()).includes('SECOND_TERMINAL_OK'));
  const upload={multipart:{file:{name:'isolation.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jD1sAAAAASUVORK5CYII=','base64')}}};
  const secondUpload=await page.request.post(base+'/bots/bot-second/api/sessions/sess-1/uploads',upload);
  assert.equal(secondUpload.status(),201);
  const secondPath=(await secondUpload.json()).path;
  const rootUpload=await page.request.post(base+'/api/sessions/sess-1/uploads',upload);
  assert.equal(rootUpload.status(),201);
  assert(secondPath.includes('/bots/bot-second/data/uploads/'));
  assert(!(await rootUpload.json()).path.includes('/bots/bot-second/'));
  await page.locator('#config-open').click();
  await page.locator('#config-dialog').waitFor({state:'visible'});
  assert(await page.locator('#config-security #cfg-auto-start-enabled').isVisible());
  assert.equal(await page.locator('.config-tab:visible').allTextContents().then(xs=>xs.join(',')),'Agent,安全,工作目录');
  await page.locator('[data-config-target="config-session"]').click();
  assert.equal(await page.locator('.agent-option-row').count(),1);
  assert.equal(await page.locator('#custom-shortcut-add').isVisible(),false);
  assert.equal(await page.locator('#config-dialog .dialog-actions button:visible').allTextContents().then(xs=>xs.join(',')),'取消,保存');
  await page.evaluate(()=>document.querySelector('#config-form').requestSubmit());
  await page.locator('#config-dialog').waitFor({state:'hidden'});
  assert.equal((await (await page.request.get(base+'/api/bots')).json())[0].app_id,'cli_fixture');
  await page.locator('#bot-add').click();
  await page.locator('#bot-dialog').waitFor({state:'visible'});
  assert(await page.locator('#bot-scan').isVisible());
  assert.equal(await page.locator('#bot-app-fields').isVisible(),false);
  await page.locator('#bot-existing').click();
  assert(await page.locator('#bot-app-id').isVisible());
  await page.locator('#bot-cancel').click();
  const design=await browser.newPage();
  await design.goto(pathToFileURL(path.join(__dirname,'../docs/robots-prototype.html')).href);
  for(const width of [1440,1024,760,390]){
   await page.setViewportSize({width,height:1000});
   await design.setViewportSize({width,height:1000});
   if(width>=760){
    for(const [actual,expected]of [['.sidebar','.app>.sessions'],['.main','.workspace']]){
     const a=await page.locator(actual).boundingBox(),b=await design.locator(expected).boundingBox();
     for(const key of ['x','y','width','height'])assert(Math.abs(a[key]-b[key])<=1,actual+' differs from approved design: '+key);
    }
    for(const [actual,expected]of [['.terminal-shell','.terminal'],['#active-title','.workspace h2']]){
     for(const prop of ['backgroundColor','borderRadius','fontFamily','fontSize'].filter(p=>actual==='.terminal-shell'?['backgroundColor','borderRadius'].includes(p):['fontFamily','fontSize'].includes(p))){
      assert.equal(await page.locator(actual).evaluate((e,p)=>getComputedStyle(e)[p],prop),await design.locator(expected).evaluate((e,p)=>getComputedStyle(e)[p],prop),actual+' '+prop);
     }
    }
   }
   const row=await page.locator('.bot-title').boundingBox(),gear=await page.locator('#bot-settings').boundingBox();
   assert(gear.x+gear.width<=row.x+row.width+1,'settings overflow');
   assert.equal(gear.width,44);
   assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'page horizontal overflow');
   await page.locator('#bot-settings').click();
   await page.locator('#bot-dialog').waitFor({state:'visible'});
   const dialog=await page.locator('#bot-dialog').boundingBox();assert(dialog.x>=0&&dialog.x+dialog.width<=width,'dialog overflow');
   await page.locator('#bot-cancel').click();
  }
  await design.close();
  await page.setViewportSize({width:1440,height:1000});
  await page.screenshot({path:'/tmp/iris-multi-bot-workspace.png'});
  await page.locator('#bot-settings').click();await page.locator('#bot-dialog').waitFor({state:'visible'});await page.screenshot({path:'/tmp/iris-multi-bot-settings.png'});await page.locator('#bot-cancel').click();
  await page.locator('.delete-btn').click();await page.waitForFunction(()=>document.querySelectorAll('.session').length===0);
  await page.locator('#bot-select').selectOption('default');await page.waitForURL(base+'/');
  await page.waitForFunction(()=>document.querySelector('.session-name')?.textContent==='Root terminal');
  assert.deepEqual(errors,[]);
  console.log('PASS: real PTY/WebSocket, bot switching, output isolation, settings auth/save, bot edit/add, controls, deletion and 4 viewport widths');
 }finally{await browser.close()}
})().catch(e=>{console.error(e);process.exitCode=1});
