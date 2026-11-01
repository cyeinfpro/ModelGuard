import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {JSDOM} from 'jsdom';
const html=await readFile(new URL('../ui/index.html',import.meta.url),'utf8');
const js=await readFile(new URL('../ui/app.js',import.meta.url),'utf8');
const tick=()=>new Promise(r=>setTimeout(r,20));
async function setup(options={}){
 const dom=new JSDOM(html,{url:'https://sub2api.invalid/api/v1/plugin-ui/test/index.html#bridge_token=token',runScripts:'outside-only'});const w=dom.window;const calls=[];const state={enabled:true,target_count:1,monitoring_state:'running',accounts:[{id:1,name:'自动读取的账号',type:'oauth',status:'active',schedulable:true,supported:true,suggested_model:'gpt-6.1-terra',suggested_expected:'gpt-6.1-terra'},{id:2,name:'另一个账号',type:'oauth',status:'active',schedulable:false,supported:true,suggested_model:'gpt-6.1-sol',suggested_expected:'gpt-6-astra'}],models:[{id:'gpt-6.1-terra',display_name:'Terra'}],state:{requests:9,records:{1:{account_id:1,status:'quarantined',hold:true,failures:3,analysis:{predicted:'<img src=x onerror=alert(1)>'},reason:'test',next_at:'2026-10-30T08:00:00Z'}},events:[]}};
 const config={enabled:true,auto_pause:true,auto_recover:true,pause_unavailable:false,admin_url:'http://127.0.0.1:8080',admin_key:'test-secret',data_dir:'/app/data/sub2api-account-guard',normal_seconds:1800,retry_seconds:120,quarantine_seconds:21600,timeout_seconds:180,daily_requests:2000,targets:[{account_id:1,model:'gpt-6.1-terra',expected:'gpt-6.1-terra',enabled:true}]};
 Object.assign(config,options.config||{});Object.assign(state,options.state||{});
 const host={postMessage(m){calls.push(m);if(!['config.load','config.save','config.test','plugin.status'].includes(m.type))return;const response={source:'sub2api-plugin-host',bridge_token:'token',request_id:m.request_id,ok:true};if(m.type==='config.load')response.config=config;else if(m.type==='config.save'){Object.assign(config,m.config);response.config=config;}else if(m.type==='config.test'){if(config.manual_request_id){state.manual_busy=true;response.result={success:true,message:'检测已开始',status_json:JSON.stringify(state)};}else response.result={success:true,message:'连接正常',status_json:JSON.stringify({...state,diagnostic:{kind:'connection'}})};}else response.result={status_json:JSON.stringify(state)};const deliver=()=>w.dispatchEvent(new w.MessageEvent('message',{source:host,data:response}));if(options.respond?.({m,response,config,state,w,deliver})!==false)queueMicrotask(deliver);}};
 Object.defineProperty(w,'parent',{value:host});w.eval(js);await tick();return {dom,w,calls,host,state,config};
}
test('legacy holds return to retesting without claiming immediate recovery',async()=>{
 for(const status of ['external_change','pause_uncertain']){
  const {dom,w,state,calls}=await setup();
  state.state.records[1]={account_id:1,status,hold:true,reason:'old reason'};
  state.last_error='old reason';state.batch={status:'completed_with_errors',errors:1};
  w.document.getElementById('refresh').click();await tick();
  assert.equal(w.document.getElementById('runTitle').textContent,'暂停账号待插件复测');
  assert.match(w.document.getElementById('runHint').textContent,/1 个.*连续两轮有效通过.*不立即解除暂停/);
  assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/等待插件复测/);
  assert.equal(calls.filter(c=>c.type==='config.save'||c.type==='config.test').length,0);
  state.enabled=false;w.document.getElementById('refresh').click();await tick();
  assert.match(w.document.getElementById('runHint').textContent,/手动复测/);
  state.active_samples=[{account_id:1,index:1,phase:'output'}];state.batch_busy=true;
  w.document.getElementById('refresh').click();await tick();
  assert.equal(w.document.getElementById('runTitle').textContent,'自动检测中');
  dom.window.close();
 }
});
test('external pauses on enabled targets are eligible for managed retesting',async()=>{
 const {dom,w,state}=await setup();
 state.accounts[0].schedulable=false;state.state.records[1]={account_id:1,status:'paused_observation',hold:false};
 w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/待插件复测.*两轮通过/);
 state.accounts[0].status='disabled';w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/非 active.*保留宿主状态/);
 state.accounts=[];w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/已删除账号不会重建/);
 dom.window.close();
});
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

test('first use leads with connection and saves only connection before discovery',async()=>{
 const {dom,w,calls,state}=await setup({config:{admin_key:'',targets:[],enabled:false,auto_pause:false,auto_recover:false},state:{target_count:0,accounts:[],monitoring_state:'waiting_targets',connection:{state:'needs_credentials',next_action:'填写管理地址和密钥'}},respond({m,state,response}){
  if(m.type==='config.test'){state.connection={state:'ready'};state.accounts=[{id:7,name:'首次接入的账号',type:'oauth',status:'active',schedulable:true,supported:true,suggested_model:'gpt-6.1-terra',suggested_expected:'gpt-6.1-terra'}];response.result={success:true,message:'管理端已连接',status_json:JSON.stringify({...state,diagnostic:{kind:'connection'}})};}
 }});
 assert.equal(w.document.getElementById('app').dataset.onboarding,'true');
 assert.equal(w.document.getElementById('setupSteps').hidden,false);
 assert.equal(w.document.getElementById('configPanel').open,true);
 assert(w.document.getElementById('connectSection').compareDocumentPosition(w.document.getElementById('accountsSection'))&w.Node.DOCUMENT_POSITION_FOLLOWING);
 w.document.getElementById('admin_key').value='new-key';w.document.getElementById('connectAccounts').click();await tick();await tick();
 const save=calls.find(c=>c.type==='config.save');assert.equal(save.config.admin_key,'new-key');assert.deepEqual(save.config.targets,[]);
 assert(calls.findIndex(c=>c.type==='config.save')<calls.findIndex(c=>c.type==='config.test'));
 assert.match(w.document.getElementById('targets').textContent,/首次接入的账号/);
 assert.equal(w.document.querySelector('#targets details').open,false);
 assert.equal(w.document.getElementById('startSetup').disabled,true);
 dom.window.close();
});
test('saving connection preserves unsaved selection and timing without committing them',async()=>{
 const {dom,w,calls}=await setup();
 const timing=w.document.getElementById('normal_seconds');timing.value='2400';timing.dispatchEvent(new w.Event('input',{bubbles:true}));
 const selected=w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]');selected.checked=true;selected.dispatchEvent(new w.Event('change',{bubbles:true}));
 const key=w.document.getElementById('admin_key');key.value='replacement-key';key.dispatchEvent(new w.Event('input',{bubbles:true}));
 w.document.getElementById('connectAccounts').click();await tick();await tick();
 const save=calls.find(c=>c.type==='config.save');assert.equal(save.config.normal_seconds,1800);assert.equal(save.config.targets.length,1);assert.equal(save.config.admin_key,'replacement-key');
 assert.equal(timing.value,'2400');assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] input[type=checkbox]').checked,true);
 assert.match(w.document.getElementById('saveState').textContent,/未保存/);
 dom.window.close();
});
test('only selected accounts require model inputs and model details survive discovery',async()=>{
 const {dom,w,state}=await setup();
 assert.equal(w.document.querySelector('#targets tr[data-account-id="1"] [data-field="model"]').required,true);
 assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] [data-field="model"]').required,false);
 const detail=w.document.querySelector('#targets tr[data-account-id="1"] details');detail.open=true;
 const input=detail.querySelector('input');input.focus();input.setSelectionRange(2,4);
 state.accounts[0].name='刷新后的账号名称';w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.querySelector('#targets tr[data-account-id="1"] details').open,true);
 assert.equal(w.document.activeElement.dataset.field,'model');assert.equal(w.document.activeElement.selectionStart,2);
 const box=w.document.querySelector('#targets tr[data-account-id="2"] [data-field="enabled"]');box.checked=true;box.dispatchEvent(new w.Event('change',{bubbles:true}));
 assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] [data-field="model"]').required,true);
 dom.window.close();
});
test('connection errors and stale host replies never look like a healthy monitor',async()=>{
 let fail=false;
 const {dom,w,state}=await setup({respond({m,response}){if(fail&&m.type==='plugin.status'){response.ok=false;response.error='宿主连接中断';}}});
 state.monitoring_state='connection_error';state.accounts_error='管理密钥缺少权限（HTTP 403）';state.connection={state:'error',next_action:'检查密钥的账号权限'};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('runPanel').dataset.status,'error');assert.match(w.document.getElementById('runTitle').textContent,/管理连接/);
 assert.equal(w.document.getElementById('lastError').hidden,false);
 fail=true;w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('connection').hidden,false);assert.match(w.document.getElementById('connection').textContent,/宿主/);
 assert.match(w.document.getElementById('runTitle').textContent,/无法同步/);assert.equal(w.document.getElementById('runNow').disabled,true);
 dom.window.close();
});
test('late status response from before save cannot overwrite committed target changes',async()=>{
 let hold=false,late;
 const {dom,w,state,config}=await setup({respond({m,deliver,config,state}){if(m.type==='config.save')state.targets=config.targets.map(t=>({...t}));if(hold&&m.type==='plugin.status'){hold=false;late=deliver;return false;}}});
 state.targets=config.targets.map(t=>({...t}));hold=true;w.document.getElementById('refresh').click();await tick();
 const box=w.document.querySelector('#targets tr[data-account-id="2"] [data-field="enabled"]');box.checked=true;box.dispatchEvent(new w.Event('change',{bubbles:true}));
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();await tick();
 // The delayed response contains the old one-target snapshot.
 late();await tick();
 assert.equal(w.document.querySelector('#targets tr[data-account-id="2"] [data-field="enabled"]').checked,true);
 assert.equal(config.targets.filter(t=>t.enabled).length,2);
 dom.window.close();
});
test('connection-only diagnostic success is not accepted as a manual execution receipt',async()=>{
 const {dom,w}=await setup({respond({m,response}){if(m.type==='config.test'){response.result={success:true,message:'连接正常',status_json:JSON.stringify({enabled:true,diagnostic:{kind:'connection'},state:{records:{},events:[]},accounts:[],target_count:1})};}}});
 w.document.getElementById('runNow').click();await tick();await tick();
 assert.match(w.document.getElementById('message').textContent,/未收到本次检测的入队回执/);
 dom.window.close();
});
test('search Enter never submits configuration and monitoring filters retain visible scope',async()=>{
 const {dom,w,calls}=await setup();
 const search=w.document.getElementById('accountSearch'),event=new w.KeyboardEvent('keydown',{key:'Enter',bubbles:true,cancelable:true});search.dispatchEvent(event);
 assert.equal(event.defaultPrevented,true);assert.equal(calls.filter(c=>c.type==='config.save').length,0);
 const filter=w.document.getElementById('monitorFilter');filter.value='held';filter.dispatchEvent(new w.Event('change'));
 assert.equal(w.document.querySelector('#records tr[data-account-id="1"]').hidden,false);
 w.document.getElementById('monitorSearch').value='不存在';w.document.getElementById('monitorSearch').dispatchEvent(new w.Event('input'));
 assert.equal(w.document.querySelector('#records tr[data-account-id="1"]').hidden,true);assert.match(w.document.getElementById('monitorSummary').textContent,/没有符合/);
 dom.window.close();
});
test('interrupted work and partial failures offer truthful recovery state',async()=>{
 const {dom,w,state}=await setup();
 state.batch={status:'completed_with_errors',errors:2,total:4,completed:4};w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('runPanel').dataset.status,'warning');assert.match(w.document.getElementById('runTitle').textContent,/重试/);
 state.state.manual={status:'interrupted'};w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.getElementById('runTitle').textContent,/已中断/);assert.match(w.document.getElementById('runHint').textContent,/重新发起/);
 dom.window.close();
});
test('independent recovery setting is explained when new automatic pauses are disabled',async()=>{
 const {dom,w,state}=await setup();state.auto_pause=false;state.auto_recover=true;w.document.getElementById('refresh').click();await tick();
 assert.match(w.document.getElementById('policySummary').textContent,/不新增暂停.*管理中暂停账号两轮通过后自动恢复/);
 dom.window.close();
});

test('pure connection actions clear a leftover manual nonce without submitting draft edits',async()=>{
 for(const id of ['diagnose','refreshAccounts','connectAccounts']){
  const {dom,w,calls,state,config}=await setup({config:{manual_request_id:'pending-old-manual'}});
  const timing=w.document.getElementById('normal_seconds');timing.value='2400';timing.dispatchEvent(new w.Event('input',{bubbles:true}));
  w.document.getElementById(id).click();await tick();await tick();
  const testIndex=calls.findIndex(c=>c.type==='config.test');assert(testIndex>=0,id);
  const preceding=calls.slice(0,testIndex).filter(c=>c.type==='config.save');assert(preceding.length>0,id);
  assert.equal(preceding.at(-1).config.manual_request_id,'',id);
  assert.equal(preceding.at(-1).config.normal_seconds,1800,id);
  assert.equal(config.manual_request_id,'',id);assert.equal(!!state.manual_busy,false,id);
  assert.equal(timing.value,'2400',id);assert.match(w.document.getElementById('saveState').textContent,/未保存/,id);
  assert(!w.document.getElementById('message').textContent.includes('检测已开始'),id);
  dom.window.close();
 }
});
test('connection diagnostics require a connection receipt even when the host reports success',async()=>{
 const {dom,w}=await setup({respond({m,response,state}){if(m.type==='config.test')response.result={success:true,message:'已加入检测队列',status_json:JSON.stringify({...state,diagnostic:{kind:'manual'},manual_request:{id:'other',accepted:true}})};}});
 w.document.getElementById('diagnose').click();await tick();await tick();
 assert.match(w.document.getElementById('message').textContent,/未收到连接诊断回执/);assert.equal(w.document.getElementById('message').className,'error');
 dom.window.close();
});
test('onboarding rejects unverified and stale connections but saved-empty targets do not trap a valid draft',async()=>{
 const {dom,w,state}=await setup({config:{targets:[]},state:{target_count:0,accounts_checked_at:'0001-01-01T00:00:00Z',connection:{state:'checking',code:'connection_unverified'},readiness:{state:'needs_connection',checks:[{code:'connection_unverified',state:'blocked',next_action:'重新加载账号'}]}}});
 const box=w.document.querySelector('#targets tr[data-account-id="1"] [data-field="enabled"]');box.checked=true;box.dispatchEvent(new w.Event('change',{bubbles:true}));
 assert.equal(w.document.getElementById('stepConnect').dataset.complete,'false');assert.equal(w.document.getElementById('startSetup').disabled,true);
 state.connection={state:'ready'};state.readiness={state:'choose_accounts',checks:[{code:'targets_required',state:'blocked',next_action:'勾选账号'}]};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('stepConnect').dataset.complete,'true');assert.equal(w.document.getElementById('startSetup').disabled,false);
 state.connection={state:'stale'};state.readiness={state:'needs_connection',checks:[{code:'accounts_stale',state:'blocked'}]};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('stepConnect').dataset.complete,'false');assert.equal(w.document.getElementById('startSetup').disabled,true);
 state.connection={state:'ready'};state.readiness={state:'needs_attention',checks:[{code:'api_bridge_missing',state:'blocked',account_ids:[2],next_action:'安装桥接'}]};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('startSetup').disabled,false,'an unrelated blocked account must not block the selected OAuth account');
 dom.window.close();
});
test('runtime failure stops sampling controls while preserving connection and configuration recovery',async()=>{
 const {dom,w,state,calls}=await setup();state.runtime_error='监测引擎重启与回滚失败，请修复状态目录后重新保存';state.connection={state:'ready'};
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('runPanel').dataset.status,'error');assert.match(w.document.getElementById('runTitle').textContent,/监测服务需要修复/);
 assert.match(w.document.getElementById('lastError').textContent,/重启与回滚失败/);assert.equal(w.document.getElementById('lastError').hidden,false);
 assert.equal(w.document.getElementById('runNow').disabled,true);assert.equal(w.document.getElementById('startSetup').disabled,true);
 assert.equal(w.document.getElementById('diagnose').disabled,false);assert(!w.document.getElementById('connection').textContent.includes('宿主状态不可用'));
 const timing=w.document.getElementById('normal_seconds');timing.value='2400';timing.dispatchEvent(new w.Event('input',{bubbles:true}));
 assert.equal(w.document.getElementById('save').disabled,false);w.document.getElementById('runNow').click();assert.equal(calls.filter(c=>c.type==='config.save').length,0);
 dom.window.close();
});
test('an owned hold removed from saved monitoring never promises an automatic retest',async()=>{
 const {dom,w,state}=await setup();
 const box=w.document.querySelector('#targets tr[data-account-id="1"] [data-field="enabled"]');box.checked=false;box.dispatchEvent(new w.Event('change',{bubbles:true}));
 assert(!w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent.includes('未参与监测'),'an unsaved deselection does not change runtime membership');
 w.document.getElementById('discard').click();
 state.targets=[{account_id:1,enabled:false,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'},{account_id:2,enabled:true,model:'gpt-6.1-sol',expected:'gpt-6-astra'}];state.target_count=1;
 w.document.getElementById('refresh').click();await tick();
 const next=w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent;
 assert.match(next,/未参与监测.*重新勾选并保存后复测.*保留暂停/);assert(!next.includes('小时后'));assert(!next.includes('确认恢复'));
 dom.window.close();
});

test('group config saves explicit IDs and templates without persisting dynamic targets',async()=>{
 const manual={account_id:1,enabled:true,model:'gpt-6.1-terra',expected:'gpt-6.1-terra'};
 const dynamic={account_id:2,enabled:true,model:'gpt-6.1-sol',expected:'gpt-6-astra'};
 const {dom,w,state,calls}=await setup({config:{group_ids:[12],group_model:'gpt-6.1-sol',group_expected:'gpt-6-astra',targets:[manual]},state:{targets:[dynamic],configured_targets:[manual],group_sync_pending_ids:[3]}});
 assert.match(w.document.getElementById('groupSyncStatus').textContent,/新成员等待配置：3/);
 assert(w.document.querySelector('#records tr[data-account-id="2"]'));
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/不在当前管理范围.*不恢复/);
 const input=w.document.getElementById('group_ids');input.value='12, 34 56';input.dispatchEvent(new w.Event('input',{bubbles:true}));
 state.targets=[];w.document.getElementById('refresh').click();await tick();
 assert.equal(input.value,'12, 34 56','refresh preserves group draft');
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
 const save=calls.find(c=>c.type==='config.save');
 assert.deepEqual(Array.from(save.config.group_ids),[12,34,56]);assert.equal(save.config.group_model,'gpt-6.1-sol');
 assert.deepEqual(Array.from(save.config.targets,t=>t.account_id),[1],'effective group members are never saved as manual configuration');
 dom.window.close();
});
test('group validation rejects invalid IDs and partial templates without host writes',async()=>{
 for(const invalid of ['0','12,12','9007199254740992']){
  const {dom,w,calls}=await setup();w.document.getElementById('group_ids').value=invalid;
  w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
  assert.equal(calls.filter(c=>c.type==='config.save').length,0);assert.match(w.document.getElementById('message').textContent,/分组/);dom.window.close();
 }
 const {dom,w,calls}=await setup();w.document.getElementById('group_ids').value='12';w.document.getElementById('group_model').value='model-only';
 w.document.getElementById('configForm').dispatchEvent(new w.Event('submit',{cancelable:true}));await tick();
 assert.equal(calls.filter(c=>c.type==='config.save').length,0);dom.window.close();
});

test('group-only onboarding can save without sampling and discard clears template drafts',async()=>{
 const {dom,w,calls}=await setup({config:{targets:[]},state:{target_count:0,targets:[],configured_targets:[]}});
 const ids=w.document.getElementById('group_ids');ids.value='12';ids.dispatchEvent(new w.Event('input',{bubbles:true}));
 assert.equal(w.document.getElementById('save').hidden,false);
 w.document.getElementById('group_model').value='draft';w.document.getElementById('group_expected').value='draft';
 w.document.getElementById('discard').click();
 assert.equal(ids.value,'');assert.equal(w.document.getElementById('group_model').value,'');assert.equal(w.document.getElementById('group_expected').value,'');
 assert.equal(calls.filter(c=>c.type==='config.save'||c.type==='config.test').length,0);
 dom.window.close();
});

test('group sync errors cannot appear as a ready recovery despite healthy connection',async()=>{
 const {dom,w,state}=await setup({config:{group_ids:[12]},state:{group_ids:[12],connection:{state:'ready'},group_sync_error:'宿主缺少分组元数据',targets:[{account_id:1,enabled:true}],configured_targets:[{account_id:1,enabled:true}]}});
 assert.equal(w.document.getElementById('runTitle').textContent,'分组同步需要处理');
 assert.match(w.document.getElementById('groupSyncStatus').textContent,/同步未确认/);
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/等待分组同步确认.*暂不恢复/);
 state.group_ids=[34];state.group_sync_error='';state.targets=[];
 w.document.getElementById('refresh').click();await tick();
 assert.equal(w.document.getElementById('group_ids').value,'34');
 assert.match(w.document.querySelector('#records tr[data-account-id="1"]').cells[3].textContent,/不在当前管理范围.*不恢复/);
 dom.window.close();
});
