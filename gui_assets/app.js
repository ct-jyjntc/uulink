'use strict';
// The same padded 24-unit grid is used in the toolbar, sidebar and content.
const icons={
 sidebar:'<rect x="3" y="5" width="18" height="14" rx="2.5"/><path d="M9 5v14"/>',
 devices:'<rect x="3" y="4.5" width="18" height="12" rx="2"/><path d="M8 20h8m-4-3.5V20"/>',
 link:'<path transform="rotate(-38 12 12)" d="M9 8H7a4 4 0 0 0 0 8h2m6-8h2a4 4 0 0 1 0 8h-2m-7-4h8"/>',
 logs:'<rect x="5" y="3" width="14" height="18" rx="2"/><path d="M8.5 8h7m-7 4h7m-7 4H13"/>',
 settings:'<path d="M10 3h4l.6 2.6 1.8 1 2.4-.8 2 3.4-1.9 1.7v2.2l1.9 1.7-2 3.4-2.4-.8-1.8 1L14 21h-4l-.6-2.6-1.8-1-2.4.8-2-3.4 1.9-1.7v-2.2L3.2 9.2l2-3.4 2.4.8 1.8-1L10 3Z"/><circle cx="12" cy="12" r="3"/>',
 refresh:'<path d="M20 10a8 8 0 1 0-.8 6M20 4.5V10h-5.5"/>',
 close:'<path d="m6 6 12 12m0-12L6 18"/>',
 plus:'<path d="M12 5v14M5 12h14"/>',
 mapping:'<path d="M4 8h15m-4-4 4 4-4 4m5 4H5m4-4-4 4 4 4"/>',
 shield:'<path d="M12 3.5 20 7v5c0 4.4-5.5 7.6-8 8.5-2.5-.9-8-4.1-8-8.5V7l8-3.5Z"/><path d="m8.5 12 2.3 2.3 4.7-4.7"/>',
 copy:'<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M15 9V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h3"/>',
 edit:'<path d="m13.8 6.2 4 4M4 20l4.6-1 11-11a2 2 0 0 0 0-2.8l-.8-.8a2 2 0 0 0-2.8 0l-11 11L4 20Z"/>',
 trash:'<path d="M4 6h16m-11 0V4h6v2m3 0-.8 12.5a1.6 1.6 0 0 1-1.6 1.5H8.4a1.6 1.6 0 0 1-1.6-1.5L6 6m4 4v6m4-6v6"/>',
 download:'<path d="M12 4v11m-4-4 4 4 4-4M4 15.5V19a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-3.5"/>'
};
const $=s=>document.querySelector(s),esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
function iconSVG(n){const glyph=Object.hasOwn(icons,n)?icons[n]:icons.link;return `<svg width="24" height="24" viewBox="0 0 24 24" preserveAspectRatio="xMidYMid meet" aria-hidden="true" focusable="false">${glyph}</svg>`;}
const icon=n=>`<span class="uu-glyph" data-icon="${n}" aria-hidden="true">${iconSVG(n)}</span>`;
const titles={overview:'连接',devices:'设备',mappings:'映射',logs:'日志',settings:'设置'};
let state=null,view='overview',devices=null,deviceError='',csrf='',logText='',logFilter='',loadingDevices=false,polling=false,toastTimer,logPaused=false,renderedSignature='';
const windowsDesktop=window.uuDesktopWindows===true, nativeBridge=window.webkit?.messageHandlers?.uulinkPreferences;
let nativePrefs=null,deviceFilter='online';const latencyPending=new Map(),latencyRequests=new Set();
let theme=localStorage.getItem('uulink-theme')||'system';
let reportedWindowsTheme=null,windowsThemeUpdate=Promise.resolve();
function syncWindowsTheme(value){if(!windowsDesktop||!csrf||reportedWindowsTheme===value)return;reportedWindowsTheme=value;windowsThemeUpdate=windowsThemeUpdate.catch(()=>{}).then(()=>action('/api/platform/preferences',{key:'appearance-dark',value:value==='dark'})).catch(()=>{reportedWindowsTheme=null;});}
const themeQuery=matchMedia('(prefers-color-scheme: dark)');
function applyTheme(){const value=theme==='system'?(themeQuery.matches?'dark':'light'):theme;document.documentElement.dataset.theme=value;syncWindowsTheme(value);window.webkit?.messageHandlers?.uulinkAppearance?.postMessage(theme);}
applyTheme();themeQuery.addEventListener('change',applyTheme);
function hydrateIcons(){document.querySelectorAll('.uu-glyph:empty').forEach(el=>el.innerHTML=iconSVG(el.dataset.icon));}
hydrateIcons();
async function request(path,options={}){const headers={'Content-Type':'application/json',...options.headers};if(csrf)headers['X-UULink-CSRF']=csrf;const response=await fetch(path,{...options,headers,cache:'no-store'});const body=await response.json();if(!response.ok){const e=new Error(body.error||'操作失败');e.status=response.status;throw e;}return body;}
async function action(path,body){return request(path,{method:'POST',body:JSON.stringify(body)});}
function toast(text){clearTimeout(toastTimer);$('#toast').textContent=text;$('#toast').hidden=false;toastTimer=setTimeout(()=>$('#toast').hidden=true,4000);}
function mappingReady(r){const p=r.protocol||'tcp';return r.enabled&&(p==='udp'?r.state==='UDP 就绪':p==='tcp'?r.state==='TCP 就绪':r.state==='TCP 就绪 · UDP 就绪');}
function status(){const enabled=state.Mappings.filter(r=>r.enabled),ready=enabled.filter(mappingReady).length,s=state.Service;if(s.State==='measuring')return{label:'测量中',tone:'wait',detail:'',dot:''};if(!enabled.length)return{label:state.Mappings.length?'无已启用映射':'尚未添加映射',tone:'',detail:'',dot:''};if(s.State==='running')return{label:ready===enabled.length&&ready?'转发就绪':'正在连接',tone:ready===enabled.length&&ready?'good':'wait',detail:`${ready} / ${enabled.length} 条映射就绪`,dot:''};return{label:enabled.length?'已停止':'尚未添加映射',tone:'',detail:s.Error||'',dot:''};}
async function refresh(){if(polling)return;polling=true;try{state=await request('/api/state');$('#panel-error').hidden=true;csrf=state.CSRF;syncWindowsTheme(document.documentElement.dataset.theme);$('#mapping-count').textContent=state.Mappings.length||'';if(!$('#modal').open){if(layoutSignature()!==renderedSignature)render();else updateRealtime();}if(view==='logs'&&!logPaused)await loadLogs();}catch(e){$('#panel-error').textContent='界面状态更新失败，请重新打开 UU Link。请检查应用是否仍在运行。';$('#panel-error').hidden=false;if(e.status===401){$('#main').innerHTML=`<div class="list uu-locked"><img src="/icon.svg" alt="" width="48" height="48"><h1>从本机打开 UU Link</h1><p>这个管理界面需要本机认证。请重新打开 UU Link 应用，或使用它保存的认证链接。</p><code>uulink gui</code></div>`;}else if(!state){$('#main').innerHTML=`<div class="list uu-empty"><h3>暂时无法读取状态</h3><p>${esc(e.message)}</p><button class="text action" data-action="refresh">重试</button></div>`;}}finally{polling=false;}}
function heading(title,description,buttons=''){return `<div class="uu-title-tools"><div class="row-head"><span class="label">${esc(title)}</span><span class="note">${esc(description)}</span></div><div class="uu-row-buttons">${buttons}</div></div>`;}
function shortID(id){return id?.length>12?id.slice(0,6)+'…'+id.slice(-4):id;}
function badge(text,tone=''){return `<span class="uu-state ${tone}">${tone?'<i></i>':''}${esc(text)}</span>`;}
function row(name,description,right='',extra=''){return `<div class="row pref ${extra}"><div class="who"><div class="name">${esc(name)}</div>${description?`<div class="sub">${description}</div>`:''}</div>${right}</div>`;}
function section(title,content,tools=''){return `<div class="row-head uu-section"><span class="label">${esc(title)}</span>${tools?`<div class="uu-row-buttons">${tools}</div>`:''}</div><div class="list prefs">${content}</div>`;}
function empty(title,text,button=''){return `<div class="list"><div class="uu-empty"><b>${esc(title)}</b>${text?`<p>${esc(text)}</p>`:''}${button}</div></div>`;}
function spark(history){if(history.length<2)return '<span class="note">等待回应</span>';const ceiling=Math.max(50,...history.map(p=>p.value)),pts=history.map((p,i)=>`${(i/(history.length-1)*130).toFixed(1)},${(22-p.value/ceiling*20).toFixed(1)}`).join(' ');return `<svg class="uu-spark" viewBox="0 0 132 24" preserveAspectRatio="none" role="img" aria-label="最近实际隧道延迟"><polyline points="${pts}"/></svg>`;}
function overview(){const st=status(),running=state.Service.State==='running';const buttons=running?`<button class="text action" data-service="restart">重新连接</button><button class="text" data-service="stop">断开</button>`:state.Mappings.some(r=>r.enabled)?`<button class="text action" data-service="start">连接</button>`:`<button class="text action" data-action="new-rule">添加映射</button>`;
 return heading('连接','',`<button class="text" data-view="mappings">管理映射…</button>`)+`<div class="list">${row('端口映射',esc(st.detail),`<span class="field">${badge(st.label,st.tone)}</span><div class="uu-row-buttons">${buttons}</div>`,'uu-connection-row')}</div>`+section('账户',state.Roles.filter(r=>r.Role==='controller').map(roleCard).join(''))+`<div class="row-head uu-section"><span class="label">映射</span><div class="uu-row-buttons"><button class="text" data-action="new-rule">${icon('plus')}<span>添加映射</span></button></div></div>${mappingTable(true)}`;
}
function roleCard(r){const control=r.Role==='controller';return row(control?'UU 账户':'被控端','',`${badge(r.LoggedIn?'已登录':'未登录',r.LoggedIn?'good':'')}<button class="text action" data-login="${r.Role}" ${!r.Writable?'disabled title="此身份需要管理员权限"':''}>${r.LoggedIn?'更换账户…':'登录…'}</button>`,'uu-role-row');}
function protocolLabel(r){return ({'tcp':'TCP','udp':'UDP','tcp+udp':'TCP + UDP'})[r.protocol||'tcp'];}
function lanAllowed(r){return r.allow_lan??!r.listen.startsWith('127.');}
function mappingTable(compact=false){const rows=compact?state.Mappings.slice(0,4):state.Mappings;
 if(!rows.length)return empty('暂无端口映射','将远端任意 TCP、UDP 服务端口映射到本机或局域网。',`<button class="text" data-action="new-rule">${icon('plus')}<span>添加映射</span></button>`);
 return `<div class="list">${rows.map(r=>row(r.name,`${esc(r.listen)} → ${esc(r.target)} · ${esc(r.device_name||shortID(r.device))}`,`<span class="uu-map-protocol">${protocolLabel(r)}</span><span class="uu-map-scope">${lanAllowed(r)?'局域网':'本机'}</span><div class="uu-map-enabled">${badge(mappingReady(r)?'就绪':r.state,mappingReady(r)?'good':r.enabled?'wait':'')}<button class="uu-toggle ${r.enabled?'on':''}" role="switch" aria-checked="${r.enabled}" data-toggle-rule="${esc(r.id)}" aria-label="${esc(r.enabled?'停用':'启用')} ${esc(r.name)}"></button></div><div class="uu-row-buttons"><button class="icon" data-edit="${esc(r.id)}" aria-label="编辑 ${esc(r.name)}">${icon('edit')}</button>${!compact?`<button class="icon" data-delete="${esc(r.id)}" aria-label="删除 ${esc(r.name)}">${icon('trash')}</button>`:''}</div>`,'uu-map-row')).join('')}</div>`;
}
function mappingsView(){return heading('映射','',`<button class="text" data-action="new-rule">${icon('plus')}<span>添加映射</span></button>`)+(state.RulesError?`<div class="uu-alert">${icon('shield')}<p>${esc(state.RulesError)}</p></div>`:'')+mappingTable();}
function deviceLatency(id){const point=state.Latencies?.[id],pending=latencyPending.get(id);if(pending&&point&&new Date(point.updated).getTime()>=pending){latencyPending.delete(id);}if(latencyPending.has(id)){if(Date.now()-pending<30000)return '测量中…';latencyPending.delete(id);return '未测得';}return point?`${Date.now()-new Date(point.updated).getTime()>15000?'上次 ':''}${Math.round(point.ms)} ms`:'未测量';}
function latencyTitle(id){const p=state.Latencies?.[id];return p?`UU 隧道往返延迟 · ${new Date(p.updated).toLocaleTimeString('zh-CN',{hour12:false})}`:'UU 隧道往返延迟';}
function deviceConnection(id){
 const c=state.DeviceConnections?.[id];
 if(c?.state==='local_connected')return badge('本机已连接','good');
 if(c?.state==='recently_busy'){
  const time=new Date(c.observed_at).toLocaleTimeString('zh-CN',{hour12:false});
  return `<span title="${esc('UU 最近一次拒绝连接：'+time)}">${badge('上次连接因设备被占用而失败','wait')}</span>`;
 }
 return '';
}
function deviceOnline(d){return ['online','connected','active','available'].includes(String(d?.status||'').trim().toLowerCase());}
function selectDeviceFilter(value){if(!['online','offline'].includes(value))return;deviceFilter=value;render();}
function devicesView(){
 const all=devices||[],onlineCount=all.filter(deviceOnline).length,filtered=all.filter(d=>deviceOnline(d)===(deviceFilter==='online'));
 let html=heading('设备','',`<button class="text" data-action="devices-refresh" ${loadingDevices?'disabled':''}>${loadingDevices?'读取中…':'刷新'}</button>`);
 html+=`<div class="warm-tabs uu-device-filters" role="tablist" aria-label="设备状态">${[['online','在线',onlineCount],['offline','离线',all.length-onlineCount]].map(([id,label,count])=>`<button id="devices-${id}" role="tab" aria-selected="${deviceFilter===id}" aria-controls="device-list" tabindex="${deviceFilter===id?0:-1}" data-device-filter="${id}" class="${deviceFilter===id?'on':''}">${label}<span class="uu-filter-count">${count}</span></button>`).join('')}</div>`;
 if(deviceError)html+=`<div class="uu-alert"><p>${esc(deviceError)}</p></div>`;
 const content=filtered.length?`<div class="list">${filtered.map(d=>{
  const id=esc(d.device_id),online=deviceOnline(d),raw=String(d.status||'').trim().toLowerCase(),label=online?'在线':['offline','disconnected'].includes(raw)?'离线':'状态未知';
  const latency=online?`<span class="uu-device-latency" data-device-latency="${id}" title="${esc(latencyTitle(d.device_id))}">${deviceLatency(d.device_id)}</span><button class="text" id="latency-${id}" data-measure="${id}" aria-label="测量 ${esc(d.alias||d.device_id)} 的隧道延迟" ${latencyPending.has(d.device_id)||latencyRequests.has(d.device_id)?'disabled':''}>测延迟</button>`:'';
  return row(d.alias||'未命名设备',deviceConnection(d.device_id),`${latency}${badge(label,online?'good':'')}<button class="icon" data-copy="${id}" aria-label="复制设备 ID">${icon('copy')}</button><button class="text action" data-device-rule="${id}">添加映射…</button>`,'uu-device-row');
 }).join('')}</div>`:empty(loadingDevices?'正在读取设备':deviceError?'暂时无法读取设备':devices?`暂无${deviceFilter==='online'?'在线':'离线'}设备`:'尚未读取设备','','');
 return html+`<div id="device-list" role="tabpanel" aria-labelledby="devices-${deviceFilter}" tabindex="0">${content}</div>`;
}
function logsView(){return heading('日志','来自 UU Link 连接引擎',`<button class="text" data-action="download-logs">${icon('download')}<span>导出…</span></button>`)+`<div class="uu-log-tools"><input class="find" id="log-search" type="search" placeholder="筛选日志…" aria-label="筛选日志" value="${esc(logFilter)}"><button class="text" data-action="pause-logs">${logPaused?'继续更新':'暂停更新'}</button></div><div class="list"><pre class="uu-log" id="logs">${esc(filteredLogs()||'暂无运行日志。')}</pre></div>`;}
let settingTab='general';
function settingSwitch(key,label,enabled,disabled=false){return `<button class="uu-toggle ${enabled?'on':''}" role="switch" aria-label="${label}" aria-checked="${!!enabled}" data-native-setting="${key}" ${disabled?'disabled':''}></button>`;}
function aggEnabled(){return (state.Connection.aggregation||0)>1;}
function aggCount(){return state.Connection.aggregation||1;}
async function loadNativePreferences(){if(!windowsDesktop&&!nativeBridge)return;try{nativePrefs=windowsDesktop?await request('/api/platform/preferences'):await nativeBridge.postMessage({action:'get'});if(view==='settings'&&!$('#modal').open)render();}catch(e){toast('无法读取应用设置');}}
function settingsView(){
 const tabs=`<div class="row-head uu-setting-tabs"><div class="warm-tabs">${[['general','通用'],['about','关于']].map(([id,t])=>`<button data-setting-tab="${id}" class="${settingTab===id?'on':''}">${t}</button>`).join('')}</div></div>`;
 if(settingTab==='general')return tabs+section('外观',row('主题','',`<div class="warm-tabs">${['system','light','dark'].map((t,i)=>`<button data-theme="${t}" class="${theme===t?'on':''}">${['跟随系统','浅色','深色'][i]}</button>`).join('')}</div>`))+(state.Network?.Enforced?section('网络',row('物理网卡直连','',`<span class="uu-value" title="${esc(state.Network.Error||state.Network.Address)}">${state.Network.Error?'无可用网卡':esc((state.Network.Name||state.Network.Interface)+' · '+state.Network.Interface)}</span>`,'uu-physical-network')):'')+section('启动与运行',row('登录时启动','',settingSwitch('login','登录时启动',nativePrefs?.login,!nativePrefs))+(nativePrefs?.loginApproval?row('等待系统允许','',`<button class="text action" data-action="login-settings">打开登录项设置</button>`):'')+row('打开时连接','',`<button class="uu-toggle ${state.Connection.connect_on_open?'on':''}" role="switch" aria-checked="${state.Connection.connect_on_open}" aria-label="打开时连接" data-action="connect-on-open"></button>`)+row('防止自动休眠','',settingSwitch('awake','防止自动休眠',nativePrefs?.awake,!nativePrefs))+(!windowsDesktop?row('在 Dock 中显示图标','',settingSwitch('dock','在 Dock 中显示图标',nativePrefs?.dock,!nativePrefs)):'')+row('关闭窗口后继续运行',windowsDesktop?'关闭后留在系统托盘，转发保持运行':'',settingSwitch('menuBar','关闭窗口后继续运行',nativePrefs?.menuBar,!nativePrefs)))+section('链路聚合',row('聚合开关',windowsDesktop?'并行多条隧道提升多线程上传/下载速度，重连后生效':'并行多条隧道提升多线程上传/下载速度，重连后生效',`<button class="uu-toggle ${aggEnabled()?'on':''}" role="switch" aria-checked="${aggEnabled()}" aria-label="聚合开关" data-action="agg-toggle"></button>`)+(aggEnabled()?row('并行隧道数',`当前 ${aggCount()} 条（1-16）`,`<div class="warm-tabs">${[2,3,4,6,8,12,16].map(n=>`<button data-agg-count="${n}" class="${aggCount()===n?'on':''}">${n}</button>`).join('')}</div>`):'')+row('游戏流量','',`<span class="uu-value">UDP 映射不参与聚合，低延迟特性保持不变</span>`,'uu-aggr-note'));
 return tabs+(windowsDesktop?section('应用',row('退出 UU Link','断开连接并结束后台运行',`<button class="text action" data-window-command="exit">退出软件</button>`)):'')+section('UU Link',row('版本','',`<span class="uu-value">0.3.5</span>`)+row('本机',esc(state.Hostname),`<span class="uu-value">${esc(state.Platform)}</span>`));
}
function layoutSignature(){return JSON.stringify([state.Roles,state.Service,state.Mappings,state.RulesError,state.ControllerRunning,state.Connection,state.Network,view==='devices'?state.DeviceConnections:null]);}
function updateRealtime(){if(view!=='devices')return;document.querySelectorAll('[data-device-latency]').forEach(el=>{el.textContent=deviceLatency(el.dataset.deviceLatency);el.title=latencyTitle(el.dataset.deviceLatency);});document.querySelectorAll('[data-measure]').forEach(b=>{b.disabled=latencyPending.has(b.dataset.measure)||latencyRequests.has(b.dataset.measure);});}
function render(){
 if(!state)return;
 const focus=document.activeElement,oldLog=$('#logs');
 if(view==='logs'&&focus?.id==='log-search')return;
 const oldScroll=oldLog?.scrollTop,follow=oldLog&&oldLog.scrollHeight-oldLog.scrollTop-oldLog.clientHeight<40;
 const focusID=focus?.id,focusAction=focus?.dataset.action;
 $('#main').innerHTML=({overview,mappings:mappingsView,devices:devicesView,logs:logsView,settings:settingsView}[view]||overview)();
 hydrateIcons();renderedSignature=layoutSignature();
 const newLog=$('#logs');if(newLog&&oldScroll!=null)newLog.scrollTop=follow?newLog.scrollHeight:oldScroll;
 if(focus&&focus!==document.body&&!focus.isConnected){const replacement=focusID?document.getElementById(focusID):focusAction?$('#main').querySelector(`[data-action="${focusAction}"]`):null;replacement?.focus({preventScroll:true});}
}
function setView(v){if(!titles[v])return;view=v;$('#page-title').textContent=titles[v];$('#crumb').textContent=titles[v];document.querySelectorAll('#nav [data-view],#toolbar-settings,#side-prefs').forEach(el=>{const current=el.dataset.view===v;el.classList.toggle('on',current);if(current)el.setAttribute('aria-current','page');else el.removeAttribute('aria-current');});render();if(v==='devices')loadDevices(true);if(v==='logs')loadLogs();if(v==='settings')loadNativePreferences();$('#main').scrollTop=0;$('#main').focus({preventScroll:true});syncNav();const main=$('#main');main.style.animation='none';void main.offsetWidth;main.style.animation='';}
async function loadDevices(silent=false){
 if(loadingDevices)return;
 const before=JSON.stringify([devices,deviceError]);loadingDevices=true;deviceError='';if(view==='devices'&&!silent)render();
 try{devices=await request('/api/devices');$('#device-count').textContent=devices.length||'';for(const id of latencyPending.keys()){if(!devices.some(d=>d.device_id===id&&deviceOnline(d)))latencyPending.delete(id);}}
 catch(e){deviceError=e.message;}
 finally{loadingDevices=false;if(view==='devices'){if(!silent||before!==JSON.stringify([devices,deviceError]))render();else{const b=$('[data-action="devices-refresh"]');if(b){b.disabled=false;b.textContent='刷新';}}}}
}
function filteredLogs(){const lines=logText.split('\n');return(logFilter?lines.filter(l=>l.toLowerCase().includes(logFilter.toLowerCase())):lines).slice(-250).join('\n');}
async function loadLogs(){try{logText=(await request('/api/logs')).text;const pre=$('#logs');if(pre){const atEnd=pre.scrollHeight-pre.scrollTop-pre.clientHeight<40;pre.textContent=filteredLogs()||'暂无运行日志。';if(atEnd)pre.scrollTop=pre.scrollHeight;}}catch(e){toast(e.message);}}
let modalOpener=null,modalBusy=false,modalCloseToken=0;
function setModalBusy(value){modalBusy=value;$('#modal-close').disabled=value;$('#modal').setAttribute('aria-busy',String(value));}
function showModal(title,body,eyebrow='UU Link'){
 modalOpener=document.activeElement;modalCloseToken++;const modal=$('#modal');modal.classList.remove('uu-closing');if(modal.open)modal.close();
 $('#modal-title').textContent=title;$('#modal-eyebrow').textContent=eyebrow;$('#modal-body').innerHTML=body;
 $('#modal-body').querySelectorAll('button.primary,button.secondary,button.danger').forEach(b=>b.classList.add('text'));hydrateIcons();setModalBusy(false);$('#modal').showModal();
 $('#modal-body').querySelector('input:not([type=hidden]):not([disabled]),select:not([disabled]),button.primary:not([disabled]),button.secondary:not([disabled])')?.focus();
}
function closeModal(){
 const modal=$('#modal');if(modalBusy||!modal.open||modal.classList.contains('uu-closing'))return;
 if(matchMedia('(prefers-reduced-motion: reduce)').matches){modal.close();return;}
 const token=++modalCloseToken;modal.classList.add('uu-closing');
 const finish=()=>{if(token!==modalCloseToken)return;modalCloseToken++;modal.removeEventListener('animationend',finish);modal.classList.remove('uu-closing');modal.close();};
 modal.addEventListener('animationend',finish);
 setTimeout(()=>{if(token===modalCloseToken)finish();},240);
}
function formError(text){let el=$('#form-error');if(!el){el=document.createElement('div');el.id='form-error';el.className='form-error';$('#modal-body').prepend(el);}el.textContent=text;$('#modal-body').scrollTop=0;}
function login(role){const r=state.Roles.find(x=>x.Role===role);showModal('登录 UU 账户',`<form method="dialog" id="login-form"><input type="hidden" name="role" value="${role}"><div class="form-field"><label for="login-name">本机设备名称</label><input id="login-name" name="name" maxlength="100" value="${esc(r.Name)}" placeholder="便于你辨认这台设备的名称"></div><div class="form-field"><label for="login-phone">UU 账户手机号</label><input id="login-phone" name="phone" type="tel" autocomplete="tel" required placeholder="请输入手机号"></div><div class="form-field"><label for="login-code">短信验证码</label><div class="inline-form"><input id="login-code" name="code" inputmode="numeric" autocomplete="one-time-code" placeholder="收到的验证码" required><button type="button" class="secondary" id="send-code">发送验证码</button></div></div><div class="form-actions"><button type="button" class="secondary" data-action="close-modal">取消</button><button class="primary" type="submit">登录并保存</button></div></form>`,'账户');}
async function serviceAction(a){if(a!=='start'){showModal(a==='stop'?'停止 UU 连接？':'重新连接 UU？',`<p class="sub">${a==='stop'?'将断开所有映射的现有连接。':'将重新建立所有连接。'}</p><div class="form-actions"><button class="secondary" data-action="close-modal">取消</button><button class="${a==='stop'?'danger':'primary'}" data-confirm-service="${a}">${a==='stop'?'停止连接':'重新连接'}</button></div>`);return;}try{await action('/api/connection',{action:a});toast('正在连接');await refresh();}catch(e){toast(e.message);}}
async function copy(text){try{await navigator.clipboard.writeText(text);toast('已复制');}catch(_){const box=document.createElement('textarea');box.value=text;document.body.append(box);box.select();const ok=document.execCommand('copy');box.remove();toast(ok?'已复制':'无法自动复制，请手动选择文本');}}
document.addEventListener('click',async e=>{const b=e.target.closest('button,a.brand');if(!b)return;if(b.dataset.nativeSetting){b.disabled=true;try{const key=b.dataset.nativeSetting,value=!nativePrefs[key];nativePrefs=windowsDesktop?await action('/api/platform/preferences',{key,value}):await nativeBridge.postMessage({action:'set',key,value});render();}catch(e){toast(e.message||'设置失败');b.disabled=false;}return;}if(b.dataset.deviceFilter){selectDeviceFilter(b.dataset.deviceFilter);return;}if(b.dataset.measure){const id=b.dataset.measure;if(!devices?.some(d=>d.device_id===id&&deviceOnline(d))||latencyPending.has(id)||latencyRequests.has(id))return;latencyRequests.add(id);latencyPending.set(id,Date.now());updateRealtime();try{await action('/api/latency/'+encodeURIComponent(id),{});}catch(e){latencyPending.delete(id);toast(e.message);loadDevices(true);}finally{latencyRequests.delete(id);updateRealtime();}return;}if(b.dataset.view){setView(b.dataset.view);return;}if(b.classList.contains('brand')){e.preventDefault();setView('overview');return;}if(b.dataset.login){login(b.dataset.login);return;}if(b.dataset.service){serviceAction(b.dataset.service);return;}if(b.dataset.theme){theme=b.dataset.theme;localStorage.setItem('uulink-theme',theme);applyTheme();render();return;}if(b.dataset.aggCount){b.disabled=true;try{await action('/api/preferences',{aggregation:parseInt(b.dataset.aggCount,10)});await refresh();toast('聚合条数已保存，重连后生效');}catch(err){toast(err.message);b.disabled=false;}return;}if(b.dataset.copy){copy(b.dataset.copy);return;}if(b.dataset.edit){ruleForm(state.Mappings.find(r=>r.id===b.dataset.edit));return;}if(b.dataset.deviceRule){ruleForm(null,b.dataset.deviceRule);return;}if(b.dataset.delete){const r=state.Mappings.find(r=>r.id===b.dataset.delete);showModal('删除这个映射？',`<p class="sub">将删除“${esc(r.name)}”，并关闭这条映射的已有连接。</p><div class="form-actions"><button class="secondary" data-action="close-modal">取消</button><button class="danger" data-confirm-delete="${esc(r.id)}">删除映射</button></div>`);return;}try{if(b.dataset.confirmDelete){await request('/api/rules/'+encodeURIComponent(b.dataset.confirmDelete),{method:'DELETE'});closeModal();toast('映射已删除');return;}if(b.dataset.confirmService){b.disabled=true;await action('/api/connection',{action:b.dataset.confirmService});closeModal();toast('连接操作已完成');return;}if(b.id==='send-code'){const f=$('#login-form'),v=Object.fromEntries(new FormData(f));b.disabled=true;b.textContent='正在发送…';await action('/api/login/code',{role:v.role,phone:v.phone,name:v.name});b.textContent='验证码已发送';toast('验证码已发送，请查看手机短信');setTimeout(()=>{if(b.isConnected){b.disabled=false;b.textContent='重新发送';}},60000);return;}
switch(b.dataset.action){case 'login-settings':await window.webkit.messageHandlers.uulinkPreferences.postMessage({action:'loginSettings'});break;case 'refresh':await refresh();break;case 'devices-refresh':await loadDevices();break;case 'new-rule':ruleForm();break;case 'close-modal':closeModal();break;case 'pause-logs':logPaused=!logPaused;render();break;case 'connect-on-open':b.disabled=true;await action('/api/preferences',{enabled:!state.Connection.connect_on_open});await refresh();toast('应用偏好已保存');break;case 'agg-toggle':{b.disabled=true;const next=aggEnabled()?0:4;await action('/api/preferences',{aggregation:next});await refresh();toast(aggEnabled()?'链路聚合已开启，重连后生效':'链路聚合已关闭，重连后生效');break;}case 'download-logs':{await loadLogs();if(window.webkit?.messageHandlers?.uulinkExport){window.webkit.messageHandlers.uulinkExport.postMessage(logText);break;}const blob=new Blob([logText],{type:'text/plain;charset=utf-8'}),url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download='uulink-'+new Date().toISOString().slice(0,10)+'.log';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);break;}}
}catch(err){if($('#modal').open)formError(err.message);else toast(err.message);b.disabled=false;if(b.id==='send-code')b.textContent='发送验证码';}});
document.addEventListener('submit',async e=>{if(e.target.getAttribute('id')!=='login-form')return;e.preventDefault();const v=Object.fromEntries(new FormData(e.target)),button=e.target.querySelector('[type=submit]');button.disabled=true;setModalBusy(true);try{await action('/api/login/verify',{role:v.role,code:v.code});setModalBusy(false);closeModal();devices=null;toast('账户已登录');}catch(err){setModalBusy(false);formError(err.message);button.disabled=false;}});
document.addEventListener('input',e=>{if(e.target.id==='log-search'){logFilter=e.target.value;const pre=$('#logs');if(pre)pre.textContent=filteredLogs()||'没有匹配的日志。';}});
$('#refresh').addEventListener('click',refresh);$('#theme-button')?.addEventListener('click',()=>{theme=document.documentElement.dataset.theme==='dark'?'light':'dark';localStorage.setItem('uulink-theme',theme);applyTheme();render();});$('#modal-close').addEventListener('click',closeModal);$('#modal').addEventListener('cancel',e=>{e.preventDefault();closeModal();});$('#modal').addEventListener('close',()=>{modalOpener?.isConnected&&modalOpener.focus({preventScroll:true});refresh();});
(async()=>{const hash=new URLSearchParams(location.hash.slice(1)),key=hash.get('key');if(key){try{await action('/api/session',{key});history.replaceState(null,'',location.pathname);}catch(e){toast(e.message);}}await refresh();await loadNativePreferences();setInterval(refresh,3000);setInterval(()=>{if(view==='devices'&&!$('#modal').open)loadDevices(true);},10000);})();

function syncNav(){const nav=$('#nav'),active=nav.querySelector('button.on'),thumb=nav.querySelector('.thumb');if(!thumb)return;thumb.hidden=!active||document.body.classList.contains('side-nav');if(!active)return;thumb.style.cssText=`left:${active.offsetLeft}px;top:${active.offsetTop}px;width:${active.offsetWidth}px;height:${active.offsetHeight}px`; }
addEventListener('resize',syncNav);
function layoutNav(){const side=localStorage.getItem('uulink-sidebar')==='true',top=$('#top');document.body.classList.toggle('side-nav',side);$('#side').hidden=!side;$('#crumb').hidden=!side;if(windowsDesktop)$('#crumb').textContent=titles[view];if(side){$('#side .side-top').append($('#sideToggle'),$('#brand'));$('#side .side-scroll').append($('#nav'));}else{top.prepend($('#brand'));top.prepend($('#sideToggle'));top.insertBefore($('#nav'),top.querySelector('.actions'));}syncNav();}
$('#sideToggle').addEventListener('click',()=>{localStorage.setItem('uulink-sidebar',String(!document.body.classList.contains('side-nav')));layoutNav();});layoutNav();
if(window.webkit?.messageHandlers?.uulinkAppearance){document.body.classList.add('mac');document.querySelectorAll('.top,.side-top').forEach(el=>el.addEventListener('mousedown',e=>{if(!e.target.closest('button,input,select,a'))window.webkit.messageHandlers.uulinkDrag?.postMessage('drag');}));}
document.addEventListener('click',async e=>{const b=e.target.closest('button');if(!b)return;if(b.dataset.settingTab){settingTab=b.dataset.settingTab;render();}if(b.dataset.toggleRule){const r=state.Mappings.find(x=>x.id===b.dataset.toggleRule);if(!r)return;b.disabled=true;try{await request('/api/rules/'+encodeURIComponent(r.id),{method:'PATCH',body:JSON.stringify({enabled:!r.enabled})});await refresh();toast(r.enabled?'映射已停用':'映射已启用');}catch(err){toast(err.message);b.disabled=false;}}});

// The local browser uses the same Magpie layout without native title-bar spacing.
if(windowsDesktop){
 document.body.classList.add('win-desktop');
 const controls=document.createElement('div');controls.className='win-caption-controls';controls.setAttribute('role','group');controls.setAttribute('aria-label','窗口控制');
 controls.innerHTML=`<button data-window-command="minimize" aria-label="最小化" title="最小化"><svg viewBox="0 0 12 12" aria-hidden="true"><path d="M1 6.5h10"/></svg></button><button data-window-command="maximize" aria-label="最大化" title="最大化"><svg class="win-maximize" viewBox="0 0 12 12" aria-hidden="true"><rect x="1.5" y="1.5" width="9" height="9"/></svg><svg class="win-restore" viewBox="0 0 12 12" aria-hidden="true"><path d="M3.5 3.5v-2h7v7h-2"/><rect x="1.5" y="3.5" width="7" height="7"/></svg></button><button data-window-command="close" aria-label="关闭窗口" title="关闭窗口"><svg viewBox="0 0 12 12" aria-hidden="true"><path d="m1.5 1.5 9 9m0-9-9 9"/></svg></button>`;
 $('#top').append(controls);
 const windowCommand=command=>action('/api/platform/preferences',{key:'window-'+command,value:true}).catch(()=>toast('窗口操作失败'));
 document.addEventListener('click',e=>{const button=e.target.closest('[data-window-command]');if(button)windowCommand(button.dataset.windowCommand);});
 window.uulinkWindowState=maximized=>{document.body.classList.toggle('win-maximized',maximized);const button=controls.querySelector('[data-window-command="maximize"]');button.title=maximized?'还原':'最大化';button.setAttribute('aria-label',button.title);};
 document.addEventListener('mousedown',e=>{
  if(window.uuNativeDrag||e.button!==0||e.target.closest('button,a,input,select,textarea,[role="separator"]'))return;
  if(!e.target.closest('#top,.side-top'))return;
  e.preventDefault();windowCommand(e.detail===2?'maximize':'drag');
 });
 const appLogo=document.querySelector('#brand .logo');appLogo.classList.remove('uu-glyph');appLogo.innerHTML='<img src="/icon.svg" alt="" width="22" height="22">';

}else if(!window.webkit?.messageHandlers?.uulinkAppearance)document.body.classList.add('web');
function measureScrollBar(){const main=$('#main');document.documentElement.style.setProperty('--bar',`${main.offsetWidth-main.clientWidth}px`);}
measureScrollBar();addEventListener('resize',measureScrollBar);
const sideEdge=$('#sideEdge');let sideDrag=null;
sideEdge.addEventListener('pointerdown',e=>{if(e.button!==0)return;sideDrag=e.pointerId;sideEdge.setPointerCapture(e.pointerId);document.body.classList.add('side-drag');});
sideEdge.addEventListener('pointermove',e=>{if(sideDrag!==e.pointerId)return;const width=Math.max(212,Math.min(280,e.clientX));document.body.style.setProperty('--side-w',`${width}px`);});
function endSideDrag(){if(sideDrag===null)return;sideDrag=null;document.body.classList.remove('side-drag');localStorage.setItem('uulink-sidebar-width',document.body.style.getPropertyValue('--side-w'));}
sideEdge.addEventListener('pointerup',endSideDrag);sideEdge.addEventListener('lostpointercapture',endSideDrag);
const storedWidth=parseInt(localStorage.getItem('uulink-sidebar-width'),10);if(storedWidth>=212&&storedWidth<=280)document.body.style.setProperty('--side-w',`${storedWidth}px`);
syncNav();

sideEdge.addEventListener('keydown',e=>{if(!['ArrowLeft','ArrowRight','Home','End'].includes(e.key))return;e.preventDefault();const old=$('#side').getBoundingClientRect().width,width=e.key==='Home'?212:e.key==='End'?280:Math.max(212,Math.min(280,old+(e.key==='ArrowRight'?8:-8)));document.body.style.setProperty('--side-w',`${width}px`);sideEdge.setAttribute('aria-valuenow',String(width));localStorage.setItem('uulink-sidebar-width',`${width}px`);});
window.uulinkMenuAction=async action=>{if(action==='open')setView('overview');else if(action==='connect')await serviceAction('start');else if(action==='disconnect')await serviceAction('stop');};

document.addEventListener('keydown',e=>{const b=e.target.closest('[data-device-filter]');if(!b||!['ArrowLeft','ArrowRight','Home','End'].includes(e.key))return;e.preventDefault();const next=e.key==='Home'?'online':e.key==='End'?'offline':b.dataset.deviceFilter==='online'?'offline':'online';selectDeviceFilter(next);document.getElementById('devices-'+next)?.focus();});
