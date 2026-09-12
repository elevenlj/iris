// Real Chrome regression: a session-only login must survive two browser exits.
// Uses only a local test cookie, never a user's browser profile or credentials.
const assert=require('node:assert/strict');
const fs=require('node:fs/promises');
const os=require('node:os');
const path=require('node:path');
const http=require('node:http');
const {spawn}=require('node:child_process');
const {chromium}=require('playwright-core');
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
(async()=>{
 const profile=await fs.mkdtemp(path.join(os.tmpdir(),'iris-login-cookie-test-'));
 const server=http.createServer((req,res)=>{
  if(req.url==='/login')res.setHeader('Set-Cookie','iris_test_session=present; HttpOnly; SameSite=Lax; Path=/');
  res.end(req.headers.cookie?.includes('iris_test_session=present')?'logged in':'logged out');
 });
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const base='http://127.0.0.1:'+server.address().port;
 try {
  for(let attempt=0;attempt<3;attempt++) {
   await fs.rm(path.join(profile,'DevToolsActivePort'),{force:true});
   const command=spawn(process.env.CHROME_PATH||'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',[
    ...(attempt===0?[]:['--headless=new']),'--user-data-dir='+profile,'--restore-last-session','--remote-debugging-address=127.0.0.1','--remote-debugging-port=0','--no-first-run','--no-default-browser-check',base
   ],{stdio:'ignore'});
   const exited=new Promise(resolve=>command.once('exit',resolve));
   let browser;
   try {
    let endpoint;
    for(let i=0;i<100;i++) {
     try { const [port]= (await fs.readFile(path.join(profile,'DevToolsActivePort'),'utf8')).split('\n'); endpoint='http://127.0.0.1:'+port;break; }catch{}
     await delay(100);
    }
    assert(endpoint,'Chrome did not start');
    browser=await chromium.connectOverCDP(endpoint);
    const context=browser.contexts()[0],page=await context.newPage();
    if(attempt===0)await page.goto(base+'/login');
    await page.goto(base);
    assert.equal(await page.locator('body').innerText(),'logged in','login lost at browser launch '+attempt);
    const client=await browser.newBrowserCDPSession();
    await client.send('Browser.close').catch(()=>{});
    await Promise.race([exited,delay(5000).then(()=>{throw new Error('Chrome did not close gracefully')})]);
   } finally { if(browser)await browser.close().catch(()=>{});command.kill();await exited; }
  }
  console.log('PASS: native headed login restored in two headless Chrome launches');
 } finally {server.close();await fs.rm(profile,{recursive:true,force:true});}
})().catch(error=>{console.error(error);process.exitCode=1});
