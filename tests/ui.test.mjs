import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {JSDOM} from 'jsdom';
const html=await readFile(new URL('../ui/index.html',import.meta.url),'utf8');
const js=await readFile(new URL('../ui/app.js',import.meta.url),'utf8');
const tick=()=>new Promise(r=>setTimeout(r,20));
async function setup(){
 const dom=new JSDOM(html,{url:'https://sub2api.invalid/api/v1/plugin-ui/test/index.html#bridge_token=token',runScripts:'outside-only'});const w=dom.window;const calls=[];const state={enabled:true,target_count:1,monitoring_state:'running',accounts:[{id:1,name:'自动读取的账号',type:'oauth',status:'active',schedulable:true,supported:true,suggested_model:'gpt-6.1-terra',suggested_expected:'gpt-6.1-terra'},{id:2,name:'另一个账号',type:'oauth',status:'active',schedulable:false,supported:true,suggested_model:'gpt-6.1-sol',suggested_expected:'gpt-6-astra'}],models:[{id:'gpt-6.1-terra',display_name:'Terra'}],state:{requests:9,records:{1:{account_id:1,status:'quarantined',hold:true,failures:3,analysis:{predicted:'<img src=x onerror=alert(1)>'},reason:'test',next_at:'2026-10-30T08:00:00Z'}},events:[]}};
 const config={enabled:true,auto_pause:true,auto_recover:true,pause_unavailable:false,admin_url:'http://127.0.0.1:8080',admin_key:'test-secret',data_dir:'/app/data/sub2api-account-guard',normal_seconds:1800,retry_seconds:120,quarantine_seconds:21600,timeout_seconds:180,daily_requests:2000,targets:[{account_id:1,model:'gpt-6.1-terra',expected:'gpt-6.1-terra',enabled:true}]};
 const host={postMessage(m){calls.push(m);if(!['config.load','config.save','config.test','plugin.status'].includes(m.type))return;const response={source:'sub2api-plugin-host',bridge_token:'token',request_id:m.request_id,ok:true};if(m.type==='config.load')response.config=config;else if(m.type==='config.save'){Object.assign(config,m.config);response.config=config;}else if(m.type==='config.test'){if(config.manual_request_id){state.manual_busy=true;response.result={success:true,message:'检测已开始',status_json:JSON.stringify(state)};}else response.result={success:true,message:'连接正常'};}else response.result={status_json:JSON.stringify(state)};queueMicrotask(()=>w.dispatchEvent(new w.MessageEvent('message',{source:host,data:response})));}};
 Object.defineProperty(w,'parent',{value:host});w.eval(js);await tick();return {dom,w,calls,host,state,config};
}
test('bridge loads native config and renders status without interpreting upstream HTML',async()=>{const {dom,w,calls}=await setup();assert.equal(w.document.getElementById('auto_recover').checked,true);assert.equal(w.document.getElementById('holdCount').textContent,'1');assert.equal(w.document.querySelectorAll('#records img').length,0);assert.match(w.document.getElementById('records').textContent,/<img src=x/);assert(calls.some(x=>x.type==='plugin.status'));assert(calls.some(x=>x.type==='config.load'));dom.window.close();});
test('saving preserves explicit targets and automatic recovery policy',async()=>{const {dom,w,calls}=await setup();w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();const m=calls.find(x=>x.type==='config.save');assert.equal(m.config.auto_recover,true);assert.equal(m.config.targets[0].account_id,1);assert.equal(m.config.admin_key,'test-secret');assert(!w.document.getElementById('records').textContent.includes('test-secret'));dom.window.close();});
test('diagnostics uses config.test rather than probes or direct admin fetch',async()=>{const {dom,w,calls}=await setup();w.document.getElementById('diagnose').click();await tick();assert.equal(calls.filter(x=>x.type==='config.test').length,1);assert.equal(w.document.getElementById('message').textContent,'连接正常');assert(!js.includes('fetch('));dom.window.close();});
test('standalone preview fails closed without host token',async()=>{const dom=new JSDOM(html,{url:'https://example.invalid',runScripts:'outside-only'});dom.window.eval(js);await tick();assert.match(dom.window.document.getElementById('message').textContent,/Sub2API 插件管理页/);dom.window.close();});

test('automatically lists named accounts with checkboxes and no ID entry',async()=>{
 const {dom,w}=await setup();
 assert.equal(w.document.querySelectorAll('#targets tr[data-account-id]').length,2);
 assert.match(w.document.getElementById('targets').textContent,/自动读取的账号/);
 assert.equal(w.document.querySelectorAll('#targets input[type=number]').length,0);
 assert.equal(w.document.querySelector('#targets tr[data-account-id="1"] input[type=checkbox]').checked,true);
 assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]').checked,false);
 w.document.getElementById('accountSearch').value='另一个';w.document.getElementById('accountSearch').dispatchEvent(new w.Event('input'));
 assert.equal(w.document.querySelector('#targets tr[data-account-id="1"]').hidden,true);
 dom.window.close();
});
test('status refresh preserves draft selection and counts saved runtime targets',async()=>{
 const {dom,w,state,calls}=await setup();
 w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]').checked=true;
 state.accounts[1].schedulable=true;
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]').checked,true);
 assert.equal(w.document.getElementById('targetCount').textContent,'1');
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
 const saved=calls.find(x=>x.type==='config.save');assert.equal(saved.config.targets.length,2);assert.equal(saved.config.targets[1].account_id,2);
 dom.window.close();
});
test('zero targets shows waiting state and scoped discovery errors',async()=>{
 const {dom,w,state}=await setup();state.target_count=0;state.monitoring_state='waiting_targets';state.accounts_error='管理员访问密钥缺少权限（HTTP 403）';state.accounts=[];
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('targetCount').textContent,'0');assert.equal(w.document.getElementById('connection').textContent,'等待选择账号');assert.match(w.document.getElementById('accountError').textContent,/403/);
 dom.window.close();
});

test('manual detection saves a dedicated request and shows busy progress',async()=>{
 const {dom,w,calls}=await setup();w.document.getElementById('runNow').click();await tick();
 const save=calls.find(c=>c.type==='config.save');assert(save.config.manual_request_id);assert.equal(save.config.test_key,undefined);
 assert.equal(w.document.getElementById('runNow').disabled,true);assert.equal(w.document.getElementById('runLabel').textContent,'检测中…');
 const before=calls.length;w.document.getElementById('runNow').click();await tick();assert.equal(calls.length,before);dom.window.close();
});
test('manual detection needs a selected account, not a testing key',async()=>{
 const {dom,w,calls}=await setup();assert.equal(w.document.getElementById('test_key'),null);
 w.document.querySelector('#targets tr[data-account-id="1"] input[type=checkbox]').checked=false;
 w.document.getElementById('runNow').click();await tick();
 assert.match(w.document.getElementById('message').textContent,/至少一个/);assert.equal(calls.filter(c=>c.type==='config.save').length,0);dom.window.close();
});
test('bulk selection follows filter and shows unsaved state',async()=>{
 const {dom,w}=await setup();w.document.getElementById('accountSearch').value='另一个';w.document.getElementById('accountSearch').dispatchEvent(new w.Event('input'));
 w.document.getElementById('selectVisible').click();assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]').checked,true);
 assert.match(w.document.getElementById('saveState').textContent,/未保存/);assert.equal(w.document.getElementById('runLabel').textContent,'保存并立即检测');
 w.document.getElementById('toggleKey').click();assert.equal(w.document.getElementById('admin_key').type,'text');w.document.getElementById('toggleKey').click();assert.equal(w.document.getElementById('admin_key').type,'password');dom.window.close();
});
test('removed historical targets are hidden without hiding owned quarantines',async()=>{
 const {dom,w,state}=await setup();state.state.records[999]={account_id:999,status:'skipped',hold:false};state.state.records[998]={account_id:998,status:'quarantined',hold:true};
 w.document.getElementById('refresh').click();await tick();
 assert(!w.document.getElementById('records').textContent.includes('999'));assert(w.document.getElementById('records').textContent.includes('998'));dom.window.close();
});
test('scheduled sample progress is not presented as an idle manual task',async()=>{
 const {dom,w,state}=await setup();state.active_sample={account_id:1,index:2,phase:'reasoning',started_at:new Date().toISOString(),timeout_seconds:180};
 w.document.getElementById('refresh').click();await tick();assert.equal(w.document.getElementById('runTitle').textContent,'自动检测中');assert.match(w.document.getElementById('records').textContent,/正在生成/);dom.window.close();
});
test('parallel account progress renders every worker and saves concurrency',async()=>{
 const {dom,w,state,calls}=await setup();state.manual_busy=true;state.concurrency=3;state.active_samples=[1,2,3].map(id=>({account_id:id,index:1,phase:'output',started_at:new Date().toISOString(),timeout_seconds:180,output_bytes:12}));
 w.document.getElementById('refresh').click();await tick();assert.equal(w.document.querySelectorAll('#activeSamples').length,0);assert.match(w.document.getElementById('runHint').textContent,/3 个账号并行/);
 state.manual_busy=false;state.active_samples=[];w.document.getElementById('refresh').click();await tick();assert.equal(w.document.getElementById('runPanel').dataset.status,'idle');
 w.document.getElementById('concurrency').value='5';w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();assert.equal(calls.find(x=>x.type==='config.save').config.concurrency,5);dom.window.close();
});

test('new selection immediately shows pending-save row then queued runtime row',async()=>{
 const {dom,w,state,config}=await setup();
 const box=w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]');box.checked=true;box.dispatchEvent(new w.Event('change',{bubbles:true}));
 assert.match(w.document.querySelector('#records tr[data-account-id="2"]').textContent,/待保存/);
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
 assert.match(w.document.querySelector('#records tr[data-account-id="2"]').textContent,/待自动检测/);
 state.targets=config.targets;state.accounts.push({id:3,name:'新加入的账号',type:'oauth',status:'active',schedulable:true,supported:true});state.targets=[...state.targets,{account_id:3,enabled:true,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'}];
 w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.querySelector('#records tr[data-account-id="3"]').textContent,/新加入的账号.*可调度.*待自动检测/);dom.window.close();
});
test('disabled historical target is moved to history and live scheduling overrides stale skipped status',async()=>{
 const {dom,w,state}=await setup();state.targets=[{account_id:1,enabled:true},{account_id:2,enabled:false}];
 state.state.records[2]={account_id:2,status:'paused_observation',reason:'上游 HTTP 403',hold:false};
 state.state.records[1]={account_id:1,status:'skipped',reason:'账号已被外部暂停',checked_at:'2026-10-30T10:00:00Z',hold:false};
 w.document.getElementById('refresh').click();await tick();
 assert(!w.document.getElementById('records').textContent.includes('403'));assert.match(w.document.getElementById('historyRecords').textContent,/403/);
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').textContent,/可调度.*待自动检测/);dom.window.close();
});
test('retry progress and failed retest do not claim a new quarantine',async()=>{
 const {dom,w,state}=await setup();state.state.records[1].status='unavailable';state.state.records[1].reason='重试两次仍失败';
 state.active_samples=[{account_id:1,index:2,attempt:3,phase:'retry_wait',started_at:new Date().toISOString(),timeout_seconds:2}];
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('activeSamples'),null);assert.match(w.document.getElementById('records').textContent,/等待自动重试/);
 state.active_samples=[];w.document.getElementById('refresh').click();await tick();assert.match(w.document.getElementById('records').textContent,/复测失败.*保留原隔离/);assert.equal(w.document.getElementById('pause_unavailable'),null);dom.window.close();
});

test('missing unchecked targets are omitted and pruned on save; selected or owned ones remain visible',async()=>{
 const {dom,w,state,calls}=await setup();
 state.targets=[{account_id:1,enabled:true,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'},{account_id:13028,enabled:false,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'},{account_id:900,enabled:true,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'},{account_id:901,enabled:false,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'}];
 state.state.records[901]={account_id:901,status:'quarantined',hold:true};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.querySelector('#targets tr[data-account-id="13028"]'),null);
 assert(w.document.querySelector('#targets tr[data-account-id="900"]'));
 assert(w.document.querySelector('#targets tr[data-account-id="901"]'));
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
 const targets=calls.find(x=>x.type==='config.save').config.targets;
 assert(!targets.some(x=>x.account_id===13028));assert(targets.some(x=>x.account_id===900));assert(targets.some(x=>x.account_id===901));dom.window.close();
});

test('monitoring leads, settings are collapsed, and edits survive an active scheduled batch',async()=>{
 const {dom,w,state,calls}=await setup();assert.equal(w.document.getElementById('configPanel').open,false);assert.equal(w.document.getElementById('diagnosticsPanel').open,false);
 state.batch_busy=true;state.batch={kind:'scheduled',total:5,completed:1,active_ids:[1],queued_ids:[2,3,4]};
 w.document.getElementById('refresh').click();await tick();w.document.getElementById('manageAccounts').click();assert.equal(w.document.getElementById('configPanel').open,true);
 const el=w.document.getElementById('normal_seconds');el.value='2400';el.dispatchEvent(new w.Event('input',{bubbles:true}));assert.equal(w.document.getElementById('settings').disabled,false);assert.equal(w.document.getElementById('save').disabled,true);
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();assert.equal(calls.filter(x=>x.type==='config.save').length,0);
 state.batch_busy=false;state.batch.status='completed';w.document.getElementById('refresh').click();await tick();assert.equal(el.value,'2400');assert.equal(w.document.getElementById('save').disabled,false);dom.window.close();
});
