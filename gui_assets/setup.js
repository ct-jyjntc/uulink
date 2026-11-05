'use strict';
function nextMappingPort(){const used=new Set(state.Mappings.map(r=>Number(r.listen.split(':').pop())));let p=12001;while(used.has(p)&&p<65535)p++;return p;}
function ruleForm(rule=null,deviceID=''){
 const r=rule||{id:'',name:'',device:deviceID,listen:`127.0.0.1:${nextMappingPort()}`,target:'127.0.0.1:22',enabled:true,protocol:'tcp+udp',allow_lan:false};
 const available=(devices||[]).slice();if(r.device&&!available.some(d=>d.device_id===r.device))available.push({device_id:r.device,alias:r.device_name||r.device});
 showModal(rule?'编辑映射':'添加映射',`<form method="dialog" id="mapping-form"><input type="hidden" name="id" value="${esc(r.id)}">
 <div class="form-field"><label for="map-name">映射名称</label><input id="map-name" name="name" maxlength="160" required value="${esc(r.name)}" placeholder="例如：远端服务"></div>
 <div class="form-field"><label for="map-device">远端设备</label>${available.length?`<select id="map-device" name="device" required><option value="">选择设备</option>${available.map(d=>`<option value="${esc(d.device_id)}" ${d.device_id===r.device?'selected':''}>${esc(d.alias||d.device_id)}</option>`).join('')}</select>`:`<input id="map-device" name="device" required value="${esc(r.device)}" placeholder="远端设备 ID">`}</div>
 <div class="form-grid"><div class="form-field"><label for="map-port">本机端口</label><input id="map-port" name="port" type="number" min="1" max="65535" required value="${Number(r.listen.split(':').pop())}"></div><div class="form-field"><label for="map-target">远端目标</label><input id="map-target" name="target" required value="${esc(r.target)}" placeholder="127.0.0.1:22"></div></div>
 <div class="form-field"><label for="map-protocol">传输协议</label><select name="protocol" id="map-protocol"><option value="tcp+udp" ${(r.protocol||'tcp')==='tcp+udp'?'selected':''}>TCP + UDP</option><option value="tcp" ${(r.protocol||'tcp')==='tcp'?'selected':''}>TCP</option><option value="udp" ${r.protocol==='udp'?'selected':''}>UDP</option></select></div>
 <div class="form-field"><label class="uu-option" for="map-lan"><input id="map-lan" name="allow_lan" type="checkbox" ${lanAllowed(r)?'checked':''}><span>允许局域网访问</span></label><small id="map-access-note"></small></div>
 <div class="form-field"><label for="map-enabled">状态</label><select name="enabled" id="map-enabled"><option value="true" ${r.enabled?'selected':''}>启用</option><option value="false" ${!r.enabled?'selected':''}>停用</option></select></div>
 <div class="form-actions"><button type="button" class="secondary" data-action="close-modal">取消</button><button type="submit" class="primary">保存映射</button></div></form>`,'端口映射');
 updateMappingAccess();
}
function updateMappingAccess(){if(!$('#mapping-form'))return;const p=$('#map-port').value,lan=$('#map-lan').checked;$('#map-access-note').textContent=lan?`${(state.LocalAddresses||[]).slice(0,3).map(ip=>ip+':'+p).join(' / ')||'本机 IP:'+p}`:`127.0.0.1:${p}`;}
document.addEventListener('change',e=>{if(['map-lan','map-port'].includes(e.target.id))updateMappingAccess();});
document.addEventListener('input',e=>{if(e.target.id==='map-port')updateMappingAccess();});
document.addEventListener('submit',async e=>{
 const f=e.target;if(f.getAttribute('id')!=='mapping-form')return;e.preventDefault();
 const v=Object.fromEntries(new FormData(f)),button=f.querySelector('[type=submit]'),lan=$('#map-lan').checked;button.disabled=true;setModalBusy(true);
 try{
  const d=(devices||[]).find(d=>d.device_id===v.device);
  await action('/api/rules',{id:v.id,name:v.name.trim(),device:v.device.trim(),device_name:d?.alias||'',listen:`${lan?'0.0.0.0':'127.0.0.1'}:${v.port}`,target:v.target.trim(),enabled:v.enabled==='true',protocol:v.protocol,allow_lan:lan});
  setModalBusy(false);closeModal();await refresh();toast('映射已保存');
 }catch(err){setModalBusy(false);formError(err.message);button.disabled=false;}
});
