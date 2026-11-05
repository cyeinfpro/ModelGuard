import {readFile} from 'node:fs/promises';
import {JSDOM} from 'jsdom';
const html=await readFile(new URL('../ui/index.html',import.meta.url),'utf8');
const scriptNames=Array.from(html.matchAll(/<script src="([^"]+)"><\/script>/g),match=>match[1]);
const scripts=await Promise.all(scriptNames.map(name=>readFile(new URL('../ui/'+name,import.meta.url),'utf8')));
const js=scripts.join('\n');
const tick=()=>new Promise(r=>setTimeout(r,20));
async function setup(options={}){
 const dom=new JSDOM(html,{url:options.url||'https://sub2api.invalid/api/v1/plugin-ui/test/index.html#bridge_token=token',...(options.referrer?{referrer:options.referrer}:{}),runScripts:'outside-only'});const w=dom.window;const calls=[],origins=[];const state={enabled:true,target_count:1,monitoring_state:'running',accounts:[{id:1,name:'自动读取的账号',type:'oauth',status:'active',schedulable:true,supported:true,suggested_model:'gpt-6.1-terra',suggested_expected:'gpt-6.1-terra'},{id:2,name:'另一个账号',type:'oauth',status:'active',schedulable:false,supported:true,suggested_model:'gpt-6.1-sol',suggested_expected:'gpt-6-astra'}],models:[{id:'gpt-6.1-terra',display_name:'Terra'}],state:{requests:9,records:{1:{account_id:1,status:'quarantined',hold:true,failures:3,analysis:{predicted:'<img src=x onerror=alert(1)>'},reason:'test',next_at:'2026-10-30T08:00:00Z'}},events:[]}};
 const config={enabled:true,auto_pause:true,auto_recover:true,pause_unavailable:false,admin_url:'http://127.0.0.1:8080',admin_key:'test-secret',data_dir:'/app/data/sub2api-account-guard',normal_seconds:1800,retry_seconds:120,quarantine_seconds:21600,timeout_seconds:180,daily_requests:2000,targets:[{account_id:1,model:'gpt-6.1-terra',expected:'gpt-6.1-terra',enabled:true}]};
 Object.assign(config,options.config||{});Object.assign(state,options.state||{});
 const host={postMessage(m,origin){calls.push(m);origins.push({type:m.type,origin});if(!['config.load','config.save','config.test','plugin.status'].includes(m.type))return;const response={source:'sub2api-plugin-host',bridge_token:'token',request_id:m.request_id,ok:true};if(m.type==='config.load')response.config=config;else if(m.type==='config.save'){Object.assign(config,m.config);response.config=config;}else if(m.type==='config.test'){if(config.manual_request_id){state.manual_busy=true;response.result={success:true,message:'检测已开始',status_json:JSON.stringify(state)};}else response.result={success:true,message:'连接正常',status_json:JSON.stringify({...state,diagnostic:{kind:'connection'}})};}else response.result={status_json:JSON.stringify(state)};const deliver=()=>w.dispatchEvent(new w.MessageEvent('message',{source:host,data:response,origin:options.hostOrigin||''}));if(options.respond?.({m,response,config,state,w,deliver})!==false)queueMicrotask(deliver);}};
 Object.defineProperty(w,'parent',{value:host});w.eval(js);await tick();return {dom,w,calls,host,state,config,origins};
}

export {setup,tick,html,js,scriptNames};
