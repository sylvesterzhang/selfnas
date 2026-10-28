const $ = (id) => document.getElementById(id);
let stateData = null;

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.add('on');
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.remove('on'), 2200);
}

async function api(path, body, method) {
  const opt = { method: method || (body ? 'POST' : 'GET'), headers: {} };
  if (body) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let json;
  try { json = await res.json(); } catch (e) { throw new Error('服务端返回异常'); }
  if (json.code === 401) { showLogin(); throw new Error('未登录'); }
  return json;
}

function showLogin() {
  $('login').classList.add('on');
}
function hideLogin() {
  $('login').classList.remove('on');
}

$('loginForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('loginErr').textContent = '';
  try {
    const r = await api('/api/login', { user: $('loginUser').value.trim(), pass: $('loginPass').value });
    if (r.code !== 0) { $('loginErr').textContent = r.msg; return; }
    hideLogin();
    await loadState();
    toast('登录成功');
  } catch (err) {
    $('loginErr').textContent = err.message;
  }
});

$('btnLogout').addEventListener('click', async () => {
  await api('/api/logout', {}, 'POST');
  showLogin();
});

document.querySelectorAll('.nav button').forEach((b) => {
  b.addEventListener('click', () => switchTab(b.dataset.tab));
});

const TITLES = { overview: '概览', roots: '根目录', account: '账号安全', system: '系统设置', logs: '日志', about: '关于' };
function switchTab(tab) {
  document.querySelectorAll('.nav button').forEach((b) => b.classList.toggle('active', b.dataset.tab === tab));
  document.querySelectorAll('.panel').forEach((p) => p.classList.toggle('on', p.id === 'panel-' + tab));
  $('pageTitle').textContent = TITLES[tab] || '';
  if (tab === 'overview') {
    loadState();
    if ($('qrImg').classList.contains('dim') || !$('qrImg').src) loadQr();
  }
  if (tab === 'logs') loadLogs();
}

async function loadState() {
  const r = await api('/api/state');
  if (r.code !== 0) return;
  stateData = r.data;
  render();
}

function render() {
  const d = stateData;
  if (!d) return;

  $('verLine').textContent = 'v' + d.version;
  $('whoText').textContent = d.user + ' · ' + d.port + ' 端口';

  $('stUptime').textContent = d.uptime;
  $('stStarted').textContent = '启动于 ' + d.startedAt;

  const channel = d.tailscale && d.tailscale.up ? 'Tailscale' : '仅局域网';
  $('stChannel').textContent = channel;
  $('stLan').textContent = d.lanIP ? '局域网 ' + d.lanIP : '未获取到局域网地址';

  const enabled = (d.roots || []).filter((x) => x.enabled).length;
  $('stRoots').textContent = enabled + ' / ' + (d.roots || []).length;

  $('stTs').textContent = d.tailscale && d.tailscale.up ? '已连接' : '未连接';
  $('stTsIP').textContent = d.tailscale && d.tailscale.ip ? d.tailscale.ip : '外网访问通道';

  $('infoVersion').textContent = d.version;
  $('infoConfig').textContent = d.configPath;
  $('infoPort').textContent = d.port;
  // 80 是默认端口，写进地址里既长又多此一举
  const portPart = String(d.port) === '80' ? '' : ':' + d.port;
  $('infoLan').textContent = 'http://' + (d.lanIP || '127.0.0.1') + portPart + '/admin';
  $('infoGuard').textContent = d.settings.adminOnlyLocal ? '仅允许本机与 Tailscale 访问' : '允许局域网访问管理台';

  const ov = $('tblOverview').querySelector('tbody');
  ov.innerHTML = '';
  (d.roots || []).forEach((r) => {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td>${esc(r.name)}</td>
      <td class="mono">${esc(r.path)}</td>
      <td class="mono cap">${r.available ? esc(r.freeGB) + ' / ' + esc(r.totalGB) : '—'}</td>
      <td>${r.available
        ? (r.enabled ? '<span class="tag ok">可访问</span>' : '<span class="tag off">已停用</span>')
        : '<span class="tag warn">离线</span>'}</td>`;
    ov.appendChild(tr);
  });
  if (!(d.roots || []).length) {
    ov.innerHTML = '<tr><td colspan="4" class="muted">还没有任何根目录，去「根目录」页添加</td></tr>';
  }

  renderRoots();
  $('optAdminOnly').checked = !!d.settings.adminOnlyLocal;
  $('optConcurrency').value = d.settings.uploadConcurrency;
  $('optSession').value = d.sessionDays;
  paintAutostart(d.autostart);
  paintAutostartOverview(d.autostart);
  paintUpdate(d.update);
}

// 新版本提示：不自动查，用户点「检查更新」才查（结果存服务端 cache，重启后还在）。
// 侧栏「关于」的红点由检查结果驱动：有新版就一直亮，直到升级到那个版本。
function paintUpdate(u) {
  u = u || {};
  const btn = document.querySelector('[data-tab="about"]');
  if (btn) btn.classList.toggle('upd', !!u.hasUpdate);
  const el = $('aboutVer');
  if (!el) return;
  let html = '当前版本 <strong>v' + esc(u.current || '') + '</strong>';
  if (u.hasUpdate) {
    html += ' · 检测到新版本 <strong>v' + esc(u.latest) + '</strong> · ' +
      '<a href="' + esc(u.url || 'https://github.com/sylvesterzhang/selfnas/releases/latest') + '" target="_blank" rel="noopener">去下载更新</a>';
  } else if (!u.latest) {
    html += ' · 还没检查过更新';
  } else {
    html += ' · 已是最新版本';
  }
  if (u.checkedAt) html += ' <span class="muted">（' + esc(u.checkedAt) + ' 检查）</span>';
  el.innerHTML = html;
}

$('btnUpdateCheck').addEventListener('click', async () => {
  const btn = $('btnUpdateCheck');
  if (btn.disabled) return;
  btn.disabled = true;
  btn.textContent = '正在检查…';
  try {
    const r = await api('/api/update/check', {});
    if (r.code === 0 && r.data) {
      paintUpdate(r.data.update);
      if (!r.data.checked) {
        toast('没检查到结果（可能连不上 GitHub），缓存的检查结果保持不变');
      } else if (r.data.update.hasUpdate) {
        toast('检测到新版本 v' + r.data.update.latest + '，去下载更新吧');
      } else {
        toast('已经是最新版本');
      }
    } else {
      toast(r.msg || '检查失败，稍后再试试');
    }
  } catch (e) {
    toast('检查失败：' + (e.message || '网络不通'));
  }
  btn.disabled = false;
  btn.textContent = '检查更新';
});

// 概览页那一行自启提示：开没开一眼能看到；没开就给个链接，点了直接跳到系统设置。
// 很多用户根本不知道有自启这回事 —— 在概览里露出一次，比藏在使用说明里强。
function paintAutostartOverview(a) {
  a = a || {};
  const el = $('infoAutostart');
  if (!a.supported) {
    el.textContent = a.detail || '此系统不支持';
    return;
  }
  if (a.enabled) {
    el.textContent = a.mode === 'task' ? '已开启（计划任务，后台无窗口）' : '已开启（注册表启动项）';
    return;
  }
  el.innerHTML = '未开启 · <a href="#" id="goAutostart">去系统设置开启</a>';
  const link = document.getElementById('goAutostart');
  if (link) {
    link.addEventListener('click', (e) => {
      e.preventDefault();
      const tab = document.querySelector('[data-tab="system"]');
      if (tab) tab.click();
    });
  }
}

// ---------- 开机自启动 ----------

function paintAutostart(a) {
  a = a || {};
  const cb = $('optAutostart');
  const note = $('autostartNote');
  const hint = $('autostartHint');
  if (!a.supported) {
    cb.disabled = true;
    cb.checked = false;
    note.textContent = a.detail || '此系统暂不支持';
    hint.style.display = 'none';
    return;
  }
  cb.disabled = false;
  cb.checked = !!a.enabled;
  if (!a.enabled) {
    note.textContent = '未开启';
    hint.style.display = '';
    return;
  }
  let txt = a.detail || '已开启';
  // 任务指向的 exe 和当前跑的不是一个 → 提醒重开一次开关就能纠正
  if (a.exe && a.exe.toLowerCase() !== (a.currentExe || '').toLowerCase() && a.currentExe) {
    txt += '（指向：' + a.exe + '）';
  }
  note.textContent = txt;
}

$('optAutostart').addEventListener('change', async () => {
  const cb = $('optAutostart');
  const enable = cb.checked;
  cb.disabled = true;
  try {
    const res = await api('/api/autostart', { enable });
    if (res.code !== 0) {
      toast(res.msg || '设置失败');
      cb.checked = !enable; // 失败回弹，等 loadState 再纠正
    } else {
      toast(enable ? '已开启开机自启动' : '已关闭开机自启动');
    }
    if (res.data) paintAutostart(res.data);
  } catch (err) {
    toast('设置失败：' + err.message);
    cb.checked = !enable;
  } finally {
    cb.disabled = false;
  }
});

function renderRoots() {
  const tb = $('tblRoots').querySelector('tbody');
  tb.innerHTML = '';
  (stateData.roots || []).forEach((r) => {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td>${esc(r.name)}</td>
      <td class="mono">${esc(r.path)}</td>
      <td><label class="switch"><input type="checkbox" data-act="readonly" ${r.readonly ? 'checked' : ''}><span class="track"></span></label></td>
      <td><label class="switch"><input type="checkbox" data-act="enabled" ${r.enabled ? 'checked' : ''}><span class="track"></span></label></td>
      <td class="mono cap">${r.available ? esc(r.freeGB) + ' / ' + esc(r.totalGB) : '<span class="tag warn">离线</span>'}</td>
      <td><button class="btn sm danger" data-act="del">移除</button></td>`;
    tr.querySelectorAll('input[type=checkbox]').forEach((cb) => {
      cb.addEventListener('change', async () => {
        const body = { path: r.path };
        body[cb.dataset.act] = cb.checked;
        const res = await api('/api/roots/update', body);
        toast(res.msg);
        if (res.code === 0) loadState(); else cb.checked = !cb.checked;
      });
    });
    tr.querySelector('[data-act=del]').addEventListener('click', async () => {
      if (!confirm('确定从列表中移除「' + r.name + '」？\n只移除访问权限，不会删除任何文件。')) return;
      const res = await api('/api/roots/delete', { path: r.path });
      toast(res.msg);
      if (res.code === 0) loadState();
    });
    tb.appendChild(tr);
  });
  if (!(stateData.roots || []).length) {
    tb.innerHTML = '<tr><td colspan="6" class="muted">暂无目录</td></tr>';
  }
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ---------- 添加目录 ----------

let browsePath = '';

$('btnAddRoot').addEventListener('click', () => {
  $('addName').value = '';
  $('addPath').value = '';
  $('addReadonly').checked = false;
  $('addErr').textContent = '';
  browsePath = '';
  $('modalAdd').classList.add('on');
  loadBrowse('');
});

$('btnAddCancel').addEventListener('click', () => $('modalAdd').classList.remove('on'));

// 重新扫描磁盘：把「现在存在、配置里没有」的盘追加成根目录，已有的不动。
// 典型场景：双系统共用一份 config，另一个系统多出来的盘不会自动出现。
$('btnRescan').addEventListener('click', async () => {
  const btn = $('btnRescan');
  btn.disabled = true;
  btn.textContent = '扫描中…';
  try {
    const res = await api('/api/roots/rescan', {});
    if (res.code !== 0) {
      toast(res.msg || '扫描失败');
      return;
    }
    const added = (res.data && res.data.added) || [];
    toast(added.length ? '已新增根目录：' + added.join('、') : '没有发现新的磁盘');
    if (added.length) loadState();
  } finally {
    btn.disabled = false;
    btn.textContent = '重新扫描磁盘';
  }
});

async function loadBrowse(path) {
  const r = await api('/api/browse?path=' + encodeURIComponent(path));
  const box = $('browser');
  box.innerHTML = '';
  if (r.code !== 0) { $('browserPath').textContent = r.msg; return; }
  const d = r.data;
  browsePath = d.path || '';
  $('browserPath').textContent = d.path ? '当前：' + d.path : '选择磁盘';
  if (d.parent !== undefined && d.parent !== null && d.path) {
    const up = document.createElement('div');
    up.className = 'row';
    up.textContent = '.. 上一级';
    up.addEventListener('click', () => loadBrowse(d.parent));
    box.appendChild(up);
  }
  (d.items || []).forEach((it) => {
    const row = document.createElement('div');
    row.className = 'row';
    row.textContent = it.name;
    row.addEventListener('click', () => {
      $('addPath').value = it.path;
      loadBrowse(it.path);
    });
    box.appendChild(row);
  });
  if (!(d.items || []).length) {
    box.innerHTML = '<div class="row" style="color:var(--text-3)">（没有子目录）</div>';
  }
}

$('btnAddOk').addEventListener('click', async () => {
  $('addErr').textContent = '';
  const path = $('addPath').value.trim();
  if (!path) { $('addErr').textContent = '请填写目录路径'; return; }
  const r = await api('/api/roots/add', { name: $('addName').value.trim(), path, readonly: $('addReadonly').checked });
  if (r.code !== 0) { $('addErr').textContent = r.msg; return; }
  $('modalAdd').classList.remove('on');
  toast(r.msg);
  loadState();
});

// ---------- 设置 ----------

$('btnSettings').addEventListener('click', async () => {
  const r = await api('/api/settings', {
    adminOnlyLocal: $('optAdminOnly').checked,
    uploadConcurrency: parseInt($('optConcurrency').value, 10) || 2,
    sessionDays: parseInt($('optSession').value, 10) || 3650,
  });
  toast(r.msg);
  if (r.code === 0) loadState();
});

// ---------- 密码 ----------

$('btnPw').addEventListener('click', async () => {
  const err = $('pwErr');
  err.textContent = '';
  const oldP = $('pwOld').value;
  const n1 = $('pwNew').value;
  const n2 = $('pwNew2').value;
  if (n1.length < 6) { err.textContent = '新密码至少 6 位'; return; }
  if (n1 !== n2) { err.textContent = '两次输入的新密码不一致'; return; }
  const r = await api('/api/password', { old: oldP, new: n1 });
  if (r.code !== 0) { err.textContent = r.msg; return; }
  toast(r.msg);
  $('pwOld').value = $('pwNew').value = $('pwNew2').value = '';
  showLogin();
});


// ---------- 扫码连接 ----------

// 固定二维码：内容不变，所以不需要倒计时、不需要刷新
async function loadQr() {
  const img = $('qrImg');
  const r = await api('/api/qr/token', {});
  if (r.code !== 0) {
    $('qrTip').textContent = r.msg || '生成失败';
    return;
  }
  img.src = r.data.png;
  $('qrUrl').textContent = r.data.url;
  $('qrUrl').href = r.data.url;   // 概览页地址是可点的超链接，新开页面直达
  $('btnQrDown').href = r.data.png;
  const days = r.data.sessionDays;
  $('qrTip').textContent = '扫开后输一次后台密码即可 · 登录保持 ' +
    (days >= 365 ? (days / 365).toFixed(days % 365 ? 1 : 0) + ' 年' : days + ' 天');
  const ts = r.data.tailscale;
  $('qrNote').textContent = ts
    ? '局域网 ' + r.data.lan + ' · 外网可用 Tailscale ' + ts + ':' + r.data.port
    : '地址跟着本机局域网 IP 走，路由器换了 IP 就重新保存一次';
}

// 复制连接地址到剪贴板。
// ⚠️ 局域网访问是 http（非 secure context），navigator.clipboard 在那种环境下根本不存在，
// 所以 execCommand 兜底才是主路径；而 textarea 不能被 clip/1px 隐藏（选区宽度为 0 会复制到空串，
// execCommand 依然返回 true —— 假成功），必须用 opacity:0 但保持可选中布局。
function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.cssText = 'position:fixed;top:0;left:0;width:1px;height:1px;' +
    'padding:0;border:0;outline:none;box-shadow:none;background:transparent;opacity:0;z-index:-1;';
  document.body.appendChild(ta);
  const sel = document.getSelection();
  const prev = sel && sel.rangeCount ? sel.getRangeAt(0) : null;
  let ok = false;
  try {
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, text.length);
    ok = document.execCommand('copy');
  } catch (e) { ok = false; }
  ta.remove();
  if (prev && sel) { sel.removeAllRanges(); sel.addRange(prev); }
  return ok;
}

// 兜底：真的写不进剪贴板时，把地址选中，用户按 Ctrl+C / 长按即可
function selectNodeText(el) {
  try {
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  } catch (e) { /* 忽略 */ }
}

$('btnQrCopy').addEventListener('click', async () => {
  const url = $('qrUrl').href;
  if (!url || url === '#') { toast('地址还没生成，稍等一下'); return; }
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(url);
      toast('链接已复制，可以粘贴发给需要的人了');
      return;
    } catch (e) { /* 落到 execCommand */ }
  }
  if (legacyCopy(url)) {
    toast('链接已复制，可以粘贴发给需要的人了');
    return;
  }
  selectNodeText($('qrUrl'));
  toast('已选中地址，按 Ctrl+C（手机长按）复制');
});

// ---------- 日志 ----------

function fmtSize(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + ' ' + u[i];
}

async function loadLogs() {
  const r = await api('/api/logs/tail?n=300');
  if (r.code === 0) {
    const box = $('logBox');
    box.textContent = (r.data.lines || []).join('\n') || '（暂无日志）';
    box.scrollTop = box.scrollHeight;
    $('logMeta').textContent = r.data.total + ' 行 · ' + r.data.file.replace(/.*[\\/]logs[\\/]/, 'logs/');
  }
  const t = await api('/api/logs/transfers?n=50');
  if (t.code === 0) {
    const tb = $('tblTransfers').querySelector('tbody');
    const rows = t.data || [];
    tb.innerHTML = rows.length ? rows.map((x) => {
      const kind = { upload: '上传', download: '下载', delete: '删除', client: '客户端' }[x.kind] || x.kind;
      const size = x.kind === 'client' ? '—' : fmtSize(x.size);
      return `<tr>
        <td class="mono">${esc((x.t || '').slice(5))}</td>
        <td>${esc(kind)}</td>
        <td class="mono">${esc(x.path || '')}</td>
        <td class="mono cap">${esc(size)}</td>
        <td class="mono">${esc(x.ua || '')}${x.ip ? ' · ' + esc(x.ip) : ''}</td>
        <td>${x.ok ? '<span class="tag ok">成功</span>' : '<span class="tag warn">失败</span>'}</td>
      </tr>`;
    }).join('') : '<tr><td colspan="6" class="muted">还没有传输记录</td></tr>';
  }
}

document.querySelectorAll('[data-days]').forEach((b) => {
  b.addEventListener('click', () => { $('optSession').value = b.dataset.days; });
});

$('btnLogRefresh').addEventListener('click', loadLogs);
$('logAuto').addEventListener('change', (e) => {
  if (e.target.checked) {
    if (!logTimer) logTimer = setInterval(() => {
      if (document.querySelector('.panel.on').id === 'panel-logs') loadLogs().catch(() => {});
    }, 3000);
  } else {
    clearInterval(logTimer);
    logTimer = null;
  }
});
let logTimer = setInterval(() => {
  if (document.querySelector('.panel.on').id === 'panel-logs') loadLogs().catch(() => {});
}, 3000);

// ---------- 启动 ----------

(async function init() {
  try {
    await loadState();
    loadQr();
  } catch (e) {
    showLogin();
    return;
  }
  setInterval(() => {
    if (document.querySelector('.panel.on').id === 'panel-overview') {
      loadState().catch(() => {});
    }
  }, 5000);
})();
