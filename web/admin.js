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

const TITLES = { overview: '概览', roots: '根目录', account: '账号安全', system: '系统设置', logs: '日志' };
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
}

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
  $('btnQrDown').href = r.data.png;
  const days = r.data.sessionDays;
  $('qrTip').textContent = '扫开后输一次后台密码即可 · 登录保持 ' +
    (days >= 365 ? (days / 365).toFixed(days % 365 ? 1 : 0) + ' 年' : days + ' 天');
  const ts = r.data.tailscale;
  $('qrNote').textContent = ts
    ? '局域网 ' + r.data.lan + ' · 外网可用 Tailscale ' + ts + ':' + r.data.port
    : '地址跟着本机局域网 IP 走，路由器换了 IP 就重新保存一次';
}

$('btnQr').addEventListener('click', loadQr);

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
