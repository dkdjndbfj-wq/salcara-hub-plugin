(() => {
  'use strict';
  window.SalcaraOperations = ({ request, getEpoch, isAuthenticated, getRole }) => {
    const $ = id => document.getElementById(id);
    if (!$('page-operations')) return null;
    const el = (tag, text, cls) => { const n = document.createElement(tag); if (text) n.textContent = text; if (cls) n.className = cls; return n; };
    const button = (title, run, cls = 'secondary') => { const n = el('button', title, cls); n.type = 'button'; n.addEventListener('click', run); return n; };
    const panel = title => { const n = el('section', '', 'panel'); n.append(el('h2', title)); return n; };
    const note = el('p', '', 'notice inline-notice'); note.setAttribute('role', 'status');
    const healthPanel = panel('服务健康'), healthTitle = el('p', '正在读取', 'pill'), healthTime = el('p', '', 'muted small'), issues = el('div', '', 'ops-issues');
    healthPanel.append(healthTitle, healthTime, issues, note);
    const backupPanel = panel('备份与恢复'), backupState = el('p', '尚未读取备份记录', 'muted');
    const steps = el('ol', '', 'ops-steps');
    for (const value of ['安排维护时间并停止 Hub，保留原数据目录。', '使用离线工具创建加密备份，工具会检查独占锁并校验整个归档。', '将归档和备份密钥分开保存在独立存储中。', '先校验备份，再恢复到新目录，使用相同版本启动并检查配对、消息和图片。确认后切换数据卷。']) steps.append(el('li', value));
    const commands = el('pre', '', 'ops-command');
    commands.textContent = 'salcara-hub -backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key\nsalcara-hub -verify-backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key\n# 恢复时将 SALCARA_HUB_DATA_DIR 指向一个不存在的新目录\nsalcara-hub -restore-backup /backup/hub.scbackup -backup-key-file /keys/hub-backup.key';
    const details = el('details', '', 'info-details'); details.append(el('summary', '查看服务器维护命令'), commands, el('p', '路径为示例，Docker 部署请使用部署文档中的一次性维护容器。浏览器不接收备份密钥；工具不会覆盖现有目录。外部反代、FCM 凭据和独立 Edge 数据需分别备份。', 'muted small'));
    backupPanel.append(backupState, steps, details);
    const membersPanel = panel('协作与监控'), membersList = el('div', '', 'ops-members');
    const form = el('form', '', 'ops-member-form');
    const nameLabel = el('label', '', 'field'), nameInput = el('input'); nameInput.maxLength = 80; nameInput.required = true; nameInput.autocomplete = 'off'; nameInput.placeholder = '例如：值班同事'; nameLabel.append(el('span', '名称'), nameInput);
    const roleLabel = el('label', '', 'field'), roleInput = el('select');
    roleInput.setAttribute('aria-label', '角色');
    const roles = {operator:'运维',viewer:'只读',monitor:'监控接口'};
    for (const [value, title] of Object.entries(roles)) { const option = el('option', title); option.value = value; roleInput.append(option); }
    roleLabel.append(el('span', '角色'), roleInput);
    const create = el('button', '创建独立密钥', 'primary'); create.type = 'submit';
    form.append(nameLabel, roleLabel, create);
    membersPanel.append(el('p', '运维可查看状态、测量和管理设备；只读只能查看。服务器、线路、访问策略、更新和账号权限由主管理员管理。', 'muted small'), form, membersList);
    const monitorDetails = el('details', '', 'info-details');
    const monitorEndpoints = el('pre', '', 'ops-command'); monitorEndpoints.textContent = 'GET /salcara-hub/_monitor/v1/health\nGET /salcara-hub/_monitor/v1/metrics\nAuthorization: Bearer <监控密钥>';
    monitorDetails.append(el('summary', '接入外部监控与告警'), monitorEndpoints, el('p', '监控密钥仅可读取聚合健康和 Prometheus 指标，不能登录后台。建议每 60 秒采集；用部署文档附带的告警规则监测服务离线、磁盘不足与备份逾期。', 'muted small')); membersPanel.append(monitorDetails);
    $('page-operations').append(healthPanel, backupPanel, membersPanel);

    const auditPanel = panel('管理操作审计'), auditNotice = el('p', '', 'muted small');
    const auditForm = el('form', '', 'filters'), queryLabel = el('label', '', 'field'), query = el('input'); query.maxLength = 100; query.placeholder = '名称或操作'; queryLabel.append(el('span', '查找记录'), query);
    const resultLabel = el('label', '', 'field'), result = el('select');
    result.setAttribute('aria-label', '结果');
    for (const [value,title] of [['','全部结果'],['success','成功'],['failed','失败'],['pending','未确认']]) { const option = el('option', title); option.value = value; result.append(option); }
    resultLabel.append(el('span', '结果'), result);
    const search = el('button', '查询', 'secondary'); search.type = 'submit';
    const download = button('导出匹配记录', exportAudit);
    auditForm.append(queryLabel, resultLabel, search, download);
    const auditRows = el('div', '', 'audit'), more = button('加载更早记录', () => loadAudit(true)); more.hidden = true;
    auditPanel.append(el('p', '查看最近 30 天、最多 5,000 条管理 API 变更。记录操作者和执行结果；不记录密钥、订阅内容或聊天正文。未确认表示执行期间服务中断或结果未能写入，应先核对实际状态。', 'muted small'), auditForm, auditNotice, auditRows, more);
    $('page-audit').prepend(auditPanel);
    const secretDialog = el('dialog'), secretValue = el('textarea'); secretValue.readOnly = true; secretValue.rows = 3; secretValue.setAttribute('aria-label', '新管理密钥'); secretValue.autocomplete = 'off'; secretValue.spellcheck = false;
    const secretNote = el('p', '', 'muted small');
    secretDialog.append(el('h2', '保存独立密钥'), el('p', '密钥只显示这一次。请通过可信方式交给对应人员，遗失后撤销并重新创建。', 'muted'), secretValue, secretNote, button('复制密钥', async () => { try { await navigator.clipboard.writeText(secretValue.value); secretNote.textContent='已复制。'; } catch { secretValue.select(); secretNote.textContent='密钥已选中，请手动复制。'; } }), button('已保存，关闭', () => secretDialog.close()));
    secretDialog.addEventListener('close', () => { secretValue.value = ''; secretNote.textContent = ''; });
    const revokeDialog = el('dialog'), revokeTitle = el('p');
    const revokeYes = button('撤销权限', revoke, 'danger');
    revokeDialog.append(el('h2', '撤销协作权限'), revokeTitle, el('p', '已有管理会话和监控密钥将立即失效。不会删除设备和用户会话。', 'muted'), button('取消', () => revokeDialog.close()), revokeYes);
    document.body.append(secretDialog,revokeDialog);
    let visible = '', timer = null, serial = 0, auditSerial = 0, memberSerial = 0, next = 0, mutating = false, revokeTarget = null;
    const valid = epoch => isAuthenticated() && getEpoch() === epoch;
    function message(value, error = false) { note.textContent = value; note.className = 'notice inline-notice' + (error ? ' error' : ''); }
    function controls() { create.disabled = mutating; nameInput.disabled = mutating; roleInput.disabled = mutating; revokeYes.disabled = mutating; for (const b of membersList.querySelectorAll('button')) b.disabled = mutating; }
    async function loadHealth() {
      const epoch=getEpoch(), seq=++serial;
      try { const data=await request('operations/health'); if(!valid(epoch)||seq!==serial)return;
        healthTitle.textContent = {healthy:'运行正常',attention:'有待处理事项',critical:'需要立即处理'}[data.status] || '状态未知'; healthTitle.className='pill'+(data.status==='healthy'?' ok':data.status==='critical'?' bad':'');
        healthTime.textContent = `最近检查：${new Date(data.checked_at).toLocaleString()} · 此页可见时每 30 秒刷新`;
        issues.replaceChildren(); for (const issue of data.issues || []) { const row=el('article','','ops-issue '+issue.level);row.append(el('strong',issue.title),el('p',issue.detail,'muted small'));issues.append(row); }
        if(!issues.children.length) issues.append(el('p','当前未发现运维异常。','muted'));
        backupState.textContent=data.backup?.created_at?`最近校验备份：${new Date(data.backup.created_at).toLocaleString()} · ${data.backup.files} 个文件 · ${(data.backup.bytes/1048576).toFixed(1)} MiB`:'尚无经过工具校验的备份记录';
        message('');
      } catch(e) { if(valid(epoch)&&seq===serial){healthTitle.textContent='读取失败';healthTime.textContent='当前数据可能过期，请检查连接后重试。';message(e.message,true);} }
    }
    async function loadMembers() {
      if(getRole()!=='owner')return; const epoch=getEpoch(),seq=++memberSerial;
      try { const data=await request('operations/members');if(!valid(epoch)||seq!==memberSerial)return;membersList.replaceChildren();
        for(const m of data.members||[]) { const row=el('div','','ops-member');const info=el('div');info.append(el('strong',m.name),el('p',`${roles[m.role]||m.role} · 创建于 ${new Date(m.created_at).toLocaleDateString()}`,'muted small'));row.append(info,button('撤销',()=>{if(mutating)return;revokeTarget=m.id;revokeTitle.textContent=`撤销「${m.name}」的${roles[m.role]||''}权限？`;revokeDialog.showModal();}));membersList.append(row); }
        if(!membersList.children.length) membersList.append(el('p','尚未添加协作成员。主管理员密钥仍可照常使用。','muted'));controls();
      }catch(e){if(valid(epoch)&&seq===memberSerial)message(e.message,true);}
    }
    form.addEventListener('submit',async event=>{event.preventDefault();if(mutating)return;const epoch=getEpoch();mutating=true;controls();
      try{const data=await request('operations/members','POST',{action:'create',name:nameInput.value.trim(),role:roleInput.value,confirm:true});if(!valid(epoch))return;nameInput.value='';secretValue.value=data.key;secretNote.textContent=data.durability_warning?'目录同步异常，请检查数据卷并确认密钥已保存。':'';secretDialog.showModal();await loadMembers();}
      catch(e){if(valid(epoch)){message(`${e.message} 如响应中断，请先检查成员列表；无法取回的密钥应撤销后重新创建。`,true);await loadMembers();}}
      finally{if(valid(epoch)){mutating=false;controls();}}
    });
    async function revoke(){if(mutating||!revokeTarget)return;const epoch=getEpoch();mutating=true;controls();
      try{await request('operations/members','POST',{action:'revoke',id:revokeTarget,confirm:true});if(!valid(epoch))return;revokeDialog.close();revokeTarget=null;await loadMembers();message('权限已撤销。');}
      catch(e){if(valid(epoch))message(e.message,true);}finally{if(valid(epoch)){mutating=false;controls();}}
    }
    function auditQuery(before=0){return new URLSearchParams({query:query.value.trim(),result:result.value,...(before?{before:String(before)}:{})});}
    async function loadAudit(append=false){const epoch=getEpoch(),seq=++auditSerial;const params=auditQuery(append?next:0);search.disabled=true;more.disabled=true;
      try{const data=await request('operations/audit?'+params);if(!valid(epoch)||seq!==auditSerial)return;if(!append)auditRows.replaceChildren();for(const row of data.records||[]){const n=el('article','','ops-audit-row');n.append(el('strong',({users:'用户管理'}[row.action]||row.action)+(row.detail?' · '+({ping:'测量',ban:'封禁',unban:'解封',revoke:'撤销配对',disconnect:'断开','user-save':'更新资料与权限','user-assign':'变更设备归属','user-delete':'删除用户'}[row.detail]||row.detail):'')),el('p',`${row.name} · ${new Date(row.at).toLocaleString()} · ${row.status===0?'结果未确认':row.status<400?'成功':'失败（'+row.status+'）'}`,'muted small'));if(row.reason)n.append(el('p',row.reason,'muted small'));auditRows.append(n);}if(!auditRows.children.length)auditRows.append(el('p','没有匹配的管理操作。','muted'));next=data.before||0;more.hidden=!next;auditNotice.textContent=data.durability_warning?'记录持久化异常，请检查数据卷。':'记录按发生时间倒序排列。';}
      catch(e){if(valid(epoch)&&seq===auditSerial)auditNotice.textContent=e.message;}finally{if(valid(epoch)&&seq===auditSerial){search.disabled=false;more.disabled=false;}}
    }
    async function exportAudit(){const epoch=getEpoch();download.disabled=true;try{const data=await request('operations/audit/export?'+auditQuery(),'GET',undefined,false,undefined,'text');if(!valid(epoch))return;const url=URL.createObjectURL(new Blob([data],{type:'application/x-ndjson'}));const a=el('a');a.href=url;a.download='salcara-audit.ndjson';document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);auditNotice.textContent='已导出匹配的保留记录。';}catch(e){if(valid(epoch))auditNotice.textContent=e.message;}finally{if(valid(epoch))download.disabled=false;}}
    auditForm.addEventListener('submit',event=>{event.preventDefault();next=0;loadAudit();});
    function schedule(){clearTimeout(timer);if(visible==='operations'&&!document.hidden&&isAuthenticated())timer=setTimeout(async()=>{await loadHealth();schedule();},30000);}
    function refresh(){if(!isAuthenticated())return;if(visible==='operations'){loadHealth();membersPanel.hidden=getRole()!=='owner';loadMembers();schedule();}if(visible==='audit')loadAudit();}
    function setVisible(page){const changed=visible!==page;visible=page;if(changed){serial++;auditSerial++;memberSerial++;refresh();}schedule();}
    function reset(){visible='';serial++;auditSerial++;memberSerial++;clearTimeout(timer);mutating=false;next=0;revokeTarget=null;secretDialog.close();revokeDialog.close();secretValue.value='';nameInput.value='';query.value='';result.value='';issues.replaceChildren();membersList.replaceChildren();auditRows.replaceChildren();message('');healthTitle.textContent='尚未读取';backupState.textContent='尚未读取备份记录';controls();}
    document.addEventListener('visibilitychange',()=>{if(!document.hidden&&visible==='operations')loadHealth();schedule();});
    window.addEventListener('pagehide',()=>{secretValue.value='';secretDialog.close();clearTimeout(timer);});
    return {setVisible,reset,refresh};
  };
})();
