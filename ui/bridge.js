"use strict";
function validOrigin(value) {
  try {
    const url = new URL(value);
    return ["http:", "https:"].includes(url.protocol) && !url.username && !url.password ? url.origin : "";
  } catch { return ""; }
}

let parentOrigin = validOrigin(location.ancestorOrigins?.[0]) ||
  validOrigin(new URLSearchParams(location.hash.slice(1)).get("parent_origin")) ||
  validOrigin(document.referrer);

function postHost(payload) {
  parent.postMessage(payload, parentOrigin || "*");
}

function message(text, error=false) { $("message").setAttribute("role",error?"alert":"status"); $("message").textContent=text; $("message").className=error?"error":""; }
function send(type, payload={}) {
  if (!token || parent === window) return Promise.reject(new Error("请在 Sub2API 插件管理页打开此页面"));
  const id=crypto.randomUUID();
  return new Promise((resolve,reject) => {
    const timer=setTimeout(()=>{pending.delete(id);reject(new Error("宿主响应超时"));},["config.save","config.test"].includes(type)?120000:20000);
    pending.set(id,{resolve,reject,timer});
    postHost({source:"sub2api-plugin-ui",bridge_token:token,type,request_id:id,...payload});
  });
}
window.addEventListener("message",e=>{
  const m=e.data;
  if(e.source!==parent||!m||m.source!=="sub2api-plugin-host"||m.bridge_token!==token)return;
  if(parentOrigin && e.origin !== parentOrigin)return;
  const p=pending.get(m.request_id);if(!p)return;
  if(!parentOrigin)parentOrigin=validOrigin(e.origin);
  clearTimeout(p.timer);pending.delete(m.request_id);
  m.ok?p.resolve(m):p.reject(new Error(typeof m.error==="string"?m.error:(m.result?.message||"宿主拒绝了请求")));
});
function snapshot(result){
  const value=result?.status_json;if(!value)throw new Error(result?.message||"插件未运行，请在宿主中启用插件");
  const status=typeof value==="string"?JSON.parse(value):value;
  if(!status||typeof status!=="object"||Array.isArray(status))throw new Error("监测状态格式异常，请重试");
  return status;
}
async function refresh(force=false){
  if(polling&&!force)return;
  const sequence=++refreshSequence,epoch=stateEpoch;polling=true;
  try{
    const r=await send("plugin.status");
    if(sequence!==refreshSequence||epoch!==stateEpoch)return;
    const s=snapshot(r.result);runtimeReady=true;lastFetchError="";lastRefresh=Date.now();render(s,true);
  }catch(e){if(sequence===refreshSequence&&epoch===stateEpoch)setStale(e.message);}
  finally{if(sequence===refreshSequence)polling=false;}
}
async function testSavedConnection(){
  // A pure connection check must never consume a saved manual command.
  // Read current committed config, clear only its nonce, and leave UI drafts alone.
  const current=await send("config.load");
  if(current.config?.manual_request_id||current.config?.stop_request_id){
    const config={...current.config,manual_request_id:"",stop_request_id:""};
    const saved=await send("config.save",{config});
    savedConfig.manual_request_id=saved.config?.manual_request_id||"";
    savedConfig.stop_request_id=saved.config?.stop_request_id||"";
  }
  const response=await send("config.test"),result=response.result||{};
  const status=result.status_json?snapshot(result):null;
  if(status)render(status);
  if(!result.success)throw new Error(result.message||"管理连接未通过检查");
  if(status?.diagnostic?.kind!=="connection")throw new Error("未收到连接诊断回执，请刷新状态后重试");
  return result;
}
async function discover(button){
  if(busy)return;busy=true;stateEpoch++;button.setAttribute("aria-busy","true");updateControls();
  try{
    const result=await testSavedConnection();message(result.message||"管理连接检查完成");
    await refresh(true);
  }catch(e){message(e.message,true);await refresh(true);}
  finally{busy=false;button.removeAttribute("aria-busy");updateControls();}
}
