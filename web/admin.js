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
    // 共享账号也能通过认证，但它进不了管理台：直接给说明页，别让它对着空面板
    const who = await api('/api/whoami').catch(() => null);
    if (!who || who.code !== 0 || !who.data || !who.data.admin) {
      showNotAdmin(r.data && r.data.user);
      return;
    }
    hideLogin();
    await loadState();
    loadQr();          // 登录后才拿得到二维码和 WebDAV 地址，不补这一下概览页会一直「加载中…」
    loadSmb();
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

const TITLES = { overview: '概览', roots: '根目录', musiclib: '手机听歌', account: '账号安全', system: '系统设置', logs: '日志', about: '关于' };
function switchTab(tab) {
  document.querySelectorAll('.nav button').forEach((b) => b.classList.toggle('active', b.dataset.tab === tab));
  document.querySelectorAll('.panel').forEach((p) => p.classList.toggle('on', p.id === 'panel-' + tab));
  $('pageTitle').textContent = TITLES[tab] || '';
  if (tab === 'overview') {
    loadState();
    if ($('qrImg').classList.contains('dim') || !$('qrImg').src) loadQr();
    loadSmb();
  }
  if (tab === 'logs') loadLogs();
  if (tab === 'account') loadScoped();
  if (tab === 'musiclib') loadMusicApp();
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
  paintTheme((d.settings && d.settings.theme) || 'light');
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
    const who = a.mode === 'task' ? '计划任务，后台无窗口'
      : a.mode === 'run' ? '注册表启动项'
      : a.mode === 'systemd' ? 'systemd 服务'
      : a.mode === 'launchd' ? 'LaunchDaemon'
      : '已安装';
    el.textContent = '已开启（' + who + '）';
    return;
  }
  el.innerHTML = '未开启 · <a href="#" id="goAutostart">' +
    (a.mode === 'systemd' || a.mode === 'launchd' ? '去复制安装命令' : '去系统设置开启') + '</a>';
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
  const box = $('autostartCmdBox');
  const sw = cb.closest('.switch');
  box.hidden = true;
  if (!a.supported) {
    cb.disabled = true;
    cb.checked = false;
    if (sw) sw.style.display = '';
    note.textContent = a.detail || '此系统暂不支持';
    hint.style.display = 'none';
    return;
  }

  // Linux / macOS：开关在这儿没有意义（我们不会替用户改系统），
  // 改成「给你命令，粘到终端里执行」。每条命令都带用途说明，空命令自动跳过。
  if (a.mode === 'systemd' || a.mode === 'launchd') {
    if (sw) sw.style.display = 'none';
    hint.style.display = 'none';
    note.textContent = a.detail || '';

    const cmds = [];
    if (a.enabled) {
      // 已安装：只给「关闭并卸载」命令，并明确标注用途
      cmds.push({
        label: '要关闭并卸载开机自启动，复制下面这条执行（不会影响你的文件）：',
        cmd: a.commandOff || '',
      });
    } else {
      // 未安装：① 安装并立即启动（带 --now）；② 仅现在启动一次（不开机自启）
      cmds.push({
        label: '① 安装并启用开机自启动（这一步会同时把服务立即启动起来）：',
        cmd: a.command || '',
      });
      cmds.push({
        label: '② 仅现在启动一次（不开机自启，下次重启不会自动拉起）：',
        cmd: a.commandStart || '',
      });
    }
    const list = cmds.filter((c) => c.cmd);
    if (!list.length) return; // 配置没生成成功，只显示 note 里的原因

    box.hidden = false;
    $('autostartCmdHint').innerHTML = a.enabled
      ? '执行完刷新本页确认。数据都在 config.json 里，卸载不会动它。'
      : '配置文件已经生成好了：<code>' + esc(a.configPath || '') + '</code>' +
        '<br>可以先 <code>cat</code> 看一眼内容，再整条复制执行（会要一次管理员密码）。';

    const wrap = $('autostartCmdList');
    wrap.innerHTML = '';
    list.forEach((c) => {
      const row = document.createElement('div');
      row.className = 'cmd-line';
      const lab = document.createElement('div');
      lab.className = 'cmd-label';
      lab.textContent = c.label;
      const inner = document.createElement('div');
      inner.className = 'cmd-row';
      const code = document.createElement('code');
      code.textContent = c.cmd;
      const btn = document.createElement('button');
      btn.className = 'btn sm';
      btn.type = 'button';
      btn.textContent = '复制';
      btn.addEventListener('click', () => copyCmd(c.cmd));
      inner.appendChild(code);
      inner.appendChild(btn);
      row.appendChild(lab);
      row.appendChild(inner);
      wrap.appendChild(row);
    });
    return;
  }

  // Windows
  if (sw) sw.style.display = '';
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

// 复制一段文本到剪贴板。okMsg 让调用方换掉提示语（自启命令说「去终端粘贴」，
// 服务器地址就是「去 App 里填」），不传按命令的老口径。
async function copyCmd(text, okMsg) {
  if (!text) return;
  const ok = okMsg || '命令已复制，去终端里粘贴执行';
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      toast(ok);
      return;
    } catch (e) { /* 落到 execCommand */ }
  }
  if (legacyCopy(text)) {
    toast(ok);
  } else {
    toast('复制失败，请手动选中' + (okMsg ? '' : '命令'));
  }
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

// 文件站皮肤：点卡片只改本地选中态，随「保存设置」一起提交。
let selTheme = 'light';
function paintTheme(t) {
  selTheme = t || 'light';
  // 后台固定经典外观（深侧栏 + 设备蓝），不跟随文件站主题 ——
  // 主题卡片管的是文件站的观感，后台是控制台，样式必须稳定：
  // 无论选哪个主题，左边的深色侧边栏和这套面板语言都一动不动。
  document.documentElement.setAttribute('data-theme', 'nas');
  document.querySelectorAll('#themeRow .theme-card').forEach((b) => {
    b.classList.toggle('on', b.dataset.theme === selTheme);
  });
}
document.querySelectorAll('#themeRow .theme-card').forEach((b) => {
  b.addEventListener('click', () => paintTheme(b.dataset.theme));
});

$('btnSettings').addEventListener('click', async () => {
  const r = await api('/api/settings', {
    adminOnlyLocal: $('optAdminOnly').checked,
    uploadConcurrency: parseInt($('optConcurrency').value, 10) || 2,
    sessionDays: parseInt($('optSession').value, 10) || 3650,
    theme: selTheme,
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
  // WebDAV 地址跟扫码地址同一个 host，只是换路径：管理台用哪个地址打开，这里就用哪个
  const dav = location.origin + '/dav/';
  $('davUrl').textContent = dav;
  $('davUrl').href = dav;
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

// 复制 WebDAV 地址：跟「复制链接」同一套兜底逻辑，只是文案不同
$('btnDavCopy').addEventListener('click', async () => {
  const url = $('davUrl').href;
  // 地址没生成时 href 还是当前页的 #，不能用 /^https?:/ 判断，否则会复制出「/admin#」这种假地址
  if (!/\/dav\/$/.test(url)) { toast('地址还没生成，稍等一下'); return; }
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(url);
      toast('WebDAV 地址已复制');
      return;
    } catch (e) { /* 落到 execCommand */ }
  }
  if (legacyCopy(url)) {
    toast('WebDAV 地址已复制');
    return;
  }
  selectNodeText($('davUrl'));
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
  // 先认身份再干活：共享账号能登录成功，但管理接口一律 403。
  // 不先问这一句，它会被 401 一路推回登录框 —— 输一遍又弹一次，等于进不去。
  const who = await api('/api/whoami').catch(() => null);
  if (!who || who.code !== 0) {
    showLogin(); // 未登录（api 里遇到 401 已经把登录框弹出来了）
    return;
  }
  if (!who.data.admin) {
    showNotAdmin(who.data.user);
    return;
  }
  try {
    await loadState();
    loadQr();
    loadSmb();
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

// 共享账号误入管理台：给一张说明卡，别让他对着空面板点。
function showNotAdmin(user) {
  hideLogin();
  $('naSub').textContent = '这是管理台，只有管理员能进';
  if (user) $('naSub').textContent += '（当前账号：' + user + '）';
  $('notAdmin').classList.add('on');
}

$('naLogout').addEventListener('click', async () => {
  await api('/api/logout', {}, 'POST').catch(() => {});
  $('notAdmin').classList.remove('on');
  showLogin();
});

// ---------- SMB（iPhone / iPad 的「文件」App 原生支持，WebDAV 不行） ----------
// 交互刻意做得很轻：默认只显示一行状态；要开启时才露出密码框（
// 因为 SMB 的 NTLM 需要 NT hash，而网页只存了 PBKDF2，必须再拿一次明文）。
let smbState = null;

function smbUrlOf(st) {
  const host = location.hostname;
  const port = st && st.port ? st.port : 445;
  return 'smb://' + host + (port === 445 ? '' : ':' + port) + '/';
}

function paintSmb(st) {
  smbState = st;
  const url = smbUrlOf(st);
  $('smbUrl').textContent = url;
  $('smbUrl').href = url;

  const btn = $('btnSmbToggle');
  btn.textContent = st.enabled ? '关闭' : '开启';
  btn.classList.toggle('primary', !st.enabled);

  // 小标写的是「默认状态」，但真开着的时候得说「已开启」——
  // 不然会出现「默认关闭」旁边配一个「关闭」按钮的自相矛盾画面
  const badge = $('smbBadge');
  if (badge) badge.textContent = st.enabled ? '已开启' : '默认关闭';

  const tip = $('smbTip');
  const passRow = $('smbPassRow');
  const takeRow = $('smbTakeoverRow');
  const restRow = $('smbRestoreRow');
  passRow.hidden = true;
  takeRow.hidden = true;
  restRow.hidden = true;

  if (st.running) {
    tip.innerHTML = 'iPhone：<b>「文件」App → 右上角「…」→ 连接服务器</b> → 填 ' +
      '<code>' + url + '</code> → 注册用户，账号 <b>' + st.user + '</b>、密码就是后台密码。<br>' +
      '共享：' + (st.shares || []).join(' / ') + '。' +
      '<span class="muted">锁屏、切 App 都会继续传。</span>' +
      (st.os === 'windows'
        ? '<br><span class="muted">445 是从 Windows 自带共享手里接过来的，所以本机的「文件共享 / 网络邻居」暂时用不了——' +
          '关掉这张卡会自动还回去。</span>'
        : '');
    // Windows 上系统 SMB 会在开机时抢回 445，这里留一个重新接管的入口，
    // 免得用户重启后发现手机连不上、不知道去哪修
    if (st.os === 'windows') takeRow.hidden = false;
    return;
  }
  if (st.enabled && !st.portFree) {
    // 这一行的措辞分平台：Windows 上我们能替他停（有完整的还原路径），
    // Linux/macOS 上只给命令、绝不代劳——服务名各发行版不一样，而且那可能是
    // 用户自己开给别人的 Samba 共享，我们擅自停掉就是砸场子。
    const why = st.error ? '<br><span class="muted">' + esc(String(st.error)).replace(/\n/g, '<br>') + '</span>' : '';
    if (st.os === 'windows') {
      tip.innerHTML = '端口 ' + st.port + ' 被 <b>Windows 自带的 SMB 服务</b>占着，我们起不来。' +
        '点下面「接管 ' + st.port + '」：会停掉系统自带的 SMB 并设成开机不启动（弹一次管理员授权），' +
        '之后 Windows 原来的「文件共享 / 网络邻居」就用不了了；' +
        '<span class="muted">关掉 SMB 时会自动还回去。</span>';
      takeRow.hidden = false;
      return;
    }
    const how = st.os === 'darwin'
      ? '<br>macOS：<b>系统设置 → 通用 → 共享 → 关掉「文件共享」</b>' +
        '<br>或终端执行 <code>sudo launchctl bootout system/com.apple.smbd</code>'
      : '<br>Ubuntu / Debian：<code>sudo systemctl stop smbd &amp;&amp; sudo systemctl disable smbd</code>' +
        '<br>Fedora / RHEL：<code>sudo systemctl stop smb &amp;&amp; sudo systemctl disable smb</code>';
    tip.innerHTML = '端口 ' + st.port + ' 被本机的 SMB 服务占着，我们起不来。' + why +
      '<br><b>这一步要你自己动手：</b>先把本机的 SMB 服务停掉，再回来把这个开关关掉重开一次。' +
      how +
      '<br><span class="muted">我们不会替你去停——那可能是你自己开给别人的共享。</span>';
    return;
  }
  if (st.enabled) {
    tip.innerHTML = '已开启但没跑起来：' + (st.error || '未知原因') +
      '<span class="muted">（可以关掉再开一次试试）</span>';
    return;
  }
  if (st.tookOver) {
    // 关了 SMB，但系统自带共享没还回去（多半是授权被取消了）——
    // 这时候用户的「网络邻居」是坏的，必须把恢复入口摆出来
    tip.innerHTML = '<b>Windows 自带的文件共享还没恢复。</b>' +
      'SMB 已经关了，但 445 还没还给系统，本机的「文件共享 / 网络邻居」现在用不了。' +
      '点下面按钮重试（会弹一次管理员授权）。';
    restRow.hidden = false;
    return;
  }
  tip.innerHTML = '默认关闭——只有 iPhone / iPad 需要它（「文件」App 不支持 WebDAV）。' +
    '开启后填 <code>' + url + '</code> 就能直接拖文件进出，锁屏也不断。' +
    '<span class="muted">安卓 / Windows / macOS 用上面的 WebDAV 就够了，不必开这个。</span>';
}

async function loadSmb() {
  try {
    const r = await api('/api/smb');
    if (r.code === 0) paintSmb(r.data);
  } catch (e) { /* 未登录时静默 */ }
}

async function smbToggle(on, pass) {
  const body = { enable: on };
  if (pass) body.pass = pass;
  const r = await api('/api/smb', body);
  if (r.code !== 0) { toast(r.msg || '操作失败'); if (r.data) paintSmb(r.data); return; }
  if (r.data) paintSmb(r.data);
  toast(r.msg || 'ok');
}

// 开启 SMB 是有代价的（抢 Windows 自带的 445，本机文件共享会停用），
// 所以动手前必须让人看清楚这句话，而不是点完才发现网络邻居坏了
function smbOnConfirm() {
  if (!smbState || smbState.os !== 'windows') return true;
  return confirm('开启 SMB 共享？\n\n' +
    '会接管 445 端口：Windows 自带的「文件共享 / 网络邻居」将被停掉，并设为开机不再启动。\n' +
    '（会弹一次管理员授权，点「否」就什么都不做）\n\n' +
    '以后关掉这张卡时，会把 445 和系统共享自动还回去。');
}

$('btnSmbToggle').addEventListener('click', async () => {
  if (!smbState) return;
  if (smbState.enabled) {
    const isWin = smbState.os === 'windows';
    if (!confirm(isWin
      ? '关闭 SMB 共享？\n\n445 会还给 Windows 自带的文件共享：服务改回自动启动并拉起（可能弹一次管理员授权）。'
      : '关闭 SMB 共享？\n\n445 会空出来，之后你可以自己打开本机的 SMB 服务。')) return;
    await smbToggle(false);
    return;
  }
  if (smbState.hasCred) {
    if (!smbOnConfirm()) return;
    await smbToggle(true);
    return;
  }
  // 第一次开启：要一次明文密码算 NT hash
  $('smbPassRow').hidden = false;
  $('smbPass').value = '';
  $('smbPass').focus();
});

$('btnSmbPassOk').addEventListener('click', async () => {
  const pass = $('smbPass').value;
  if (!pass) { toast('先填密码'); return; }
  if (!smbOnConfirm()) return;
  await smbToggle(true, pass);
});

$('smbPass').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') $('btnSmbPassOk').click();
});

$('btnSmbTakeover').addEventListener('click', async () => {
  toast('等管理员授权…');
  const r = await api('/api/smb/takeover', {});
  if (r.code !== 0) { toast(r.msg || '接管失败'); if (r.data) paintSmb(r.data); return; }
  if (r.data) paintSmb(r.data);
  toast(r.msg || '已接管');
});

$('btnSmbRestore').addEventListener('click', async () => {
  toast('等管理员授权…');
  const r = await api('/api/smb/restore', {});
  if (r.code !== 0) { toast(r.msg || '恢复失败'); if (r.data) paintSmb(r.data); return; }
  if (r.data) paintSmb(r.data);
  toast(r.msg || '已恢复');
});

$('btnSmbCopy').addEventListener('click', async () => {
  const url = $('smbUrl').href;
  if (!/^smb:\/\//.test(url)) { toast('地址还没生成，稍等一下'); return; }
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(url); toast('SMB 地址已复制'); return; } catch (e) { /* 落到 execCommand */ }
  }
  if (legacyCopy(url)) { toast('SMB 地址已复制'); return; }
  selectNodeText($('smbUrl'));
  toast('已选中地址，按 Ctrl+C（手机长按）复制');
});

// ---------- 共享账号（受限访问：SMB / WebDAV 只看白名单文件夹） ----------
// 账号存在 config 里、改动立即生效（SMB 认证器热刷新，不用重启服务）。
// 文件夹白名单每人一份；展示名取路径基名，重名自动加后缀，所以列表里直接显示完整路径。

async function loadScoped() {
  const r = await api('/api/scoped');
  if (r.code !== 0) return;
  renderScoped((r.data && r.data.accounts) || []);
}

function renderScoped(accounts) {
  const tb = $('tblScoped').querySelector('tbody');
  tb.innerHTML = '';
  if (!accounts.length) {
    tb.innerHTML = '<tr><td colspan="3" class="muted">还没有共享账号。点下面的「添加共享账号」创建一个，再勾选允许访问的文件夹。</td></tr>';
    return;
  }
  accounts.forEach((a) => {
    const tr = document.createElement('tr');
    const chips = (a.folders || []).map((f) =>
      `<span class="scoped-chip mono" title="${esc(f.path)}">` +
      `<button class="scoped-perm${f.readonly ? ' ro' : ''}" data-path="${esc(f.path)}" data-ro="${f.readonly ? 1 : 0}" ` +
      `title="点一下切换：现在是「${f.readonly ? '只读' : '可读写'}」">${f.readonly ? '只读' : '读写'}</button>` +
      `${esc(f.path)}` +
      `<button class="scoped-x" data-path="${esc(f.path)}" title="移除">×</button></span>`
    ).join('') || '<span class="muted">还没选文件夹——对方连上也只能看到空的「共享」</span>';
    tr.innerHTML = `<td><b>${esc(a.user)}</b></td>
      <td>${chips}<button class="btn sm scoped-addfolder" style="margin-top:4px">+ 添加文件夹</button></td>
      <td><button class="btn sm danger" data-act="del">删除账号</button></td>`;
    tr.querySelectorAll('.scoped-x').forEach((b) => {
      b.addEventListener('click', async () => {
        const res = await api('/api/scoped/folder?user=' + encodeURIComponent(a.user) +
          '&path=' + encodeURIComponent(b.dataset.path), {}, 'DELETE');
        toast(res.msg);
        if (res.code === 0) loadScoped();
      });
    });
    // 点权限药丸就地切换读写 —— 不用重新选路径，改的是这一个文件夹
    tr.querySelectorAll('.scoped-perm').forEach((b) => {
      b.addEventListener('click', async () => {
        const nextRo = b.dataset.ro !== '1';
        const res = await api('/api/scoped/perm',
          { user: a.user, path: b.dataset.path, readonly: nextRo }, 'POST');
        toast(res.msg);
        if (res.code === 0) loadScoped();
      });
    });
    tr.querySelector('.scoped-addfolder').addEventListener('click', () => {
      openScopedFolderPicker(a.user);
    });
    tr.querySelector('[data-act=del]').addEventListener('click', async () => {
      if (!confirm('确定删除共享账号「' + a.user + '」？\n对方的连接会立即失效，文件不受影响。')) return;
      const res = await api('/api/scoped?user=' + encodeURIComponent(a.user), {}, 'DELETE');
      toast(res.msg);
      if (res.code === 0) loadScoped();
    });
    tb.appendChild(tr);
  });
}

// 新建账号
$('btnScopedAdd').addEventListener('click', () => {
  $('scopedUser').value = '';
  $('scopedPass').value = '';
  $('scopedUserErr').textContent = '';
  $('modalScopedUser').classList.add('on');
  $('scopedUser').focus();
});
$('btnScopedUserCancel').addEventListener('click', () => $('modalScopedUser').classList.remove('on'));
$('btnScopedUserOk').addEventListener('click', async () => {
  $('scopedUserErr').textContent = '';
  const user = $('scopedUser').value.trim();
  const pass = $('scopedPass').value;
  if (!user) { $('scopedUserErr').textContent = '请填写用户名'; return; }
  if (pass.length < 4) { $('scopedUserErr').textContent = '密码至少 4 位'; return; }
  // 接口对「已存在的用户名 + 密码」的语义是「改密码」，这里先挡一下，
  // 免得手滑重名把别人的密码改了却毫无提示
  const list = await api('/api/scoped');
  const dup = list.code === 0 && (list.data.accounts || []).some(
    (a) => a.user.toLowerCase() === user.toLowerCase());
  if (dup) { $('scopedUserErr').textContent = '这个用户名已经有了，换个名字（要改密码请先删除再重建）'; return; }
  const r = await api('/api/scoped', { user, pass });
  if (r.code !== 0) { $('scopedUserErr').textContent = r.msg; return; }
  $('modalScopedUser').classList.remove('on');
  toast('已创建，接着给它勾选允许访问的文件夹');
  await loadScoped();
  // 顺手把选文件夹的弹窗带出来——新建账号八成下一步就是加文件夹
  openScopedFolderPicker(user);
});
$('scopedPass').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') $('btnScopedUserOk').click();
});

// 选文件夹：复用 /api/browse 逐层进入，逻辑和「添加目录」一样，只是 DOM 分开
let scopedBrowsePath = '';
let scopedFolderUser = '';

function openScopedFolderPicker(user) {
  scopedFolderUser = user;
  $('scopedFolderFor').textContent = '给账号「' + user + '」勾选';
  $('scopedPath').value = '';
  $('scopedFolderErr').textContent = '';
  // 每次打开都回到默认「可读写」，别把上一个文件夹选的只读带过来
  document.querySelectorAll('#scopedPermSeg .seg-btn').forEach((b) => {
    b.classList.toggle('on', b.dataset.ro === '0');
  });
  scopedBrowsePath = '';
  $('modalScopedFolder').classList.add('on');
  loadScopedBrowse('');
}
$('btnScopedFolderCancel').addEventListener('click', () => $('modalScopedFolder').classList.remove('on'));

// 权限分段：读写 / 只读，二选一
$('scopedPermSeg').addEventListener('click', (e) => {
  const b = e.target.closest('.seg-btn');
  if (!b) return;
  document.querySelectorAll('#scopedPermSeg .seg-btn').forEach((x) => x.classList.toggle('on', x === b));
});

async function loadScopedBrowse(path) {
  const r = await api('/api/browse?path=' + encodeURIComponent(path));
  const box = $('scopedBrowser');
  box.innerHTML = '';
  if (r.code !== 0) { $('scopedBrowserPath').textContent = r.msg; return; }
  const d = r.data;
  scopedBrowsePath = d.path || '';
  $('scopedBrowserPath').textContent = d.path ? '当前：' + d.path : '选择磁盘';
  if (d.parent !== undefined && d.parent !== null && d.path) {
    const up = document.createElement('div');
    up.className = 'row';
    up.textContent = '.. 上一级';
    up.addEventListener('click', () => loadScopedBrowse(d.parent));
    box.appendChild(up);
  }
  (d.items || []).forEach((it) => {
    const row = document.createElement('div');
    row.className = 'row';
    row.textContent = it.name;
    row.addEventListener('click', () => {
      $('scopedPath').value = it.path;
      loadScopedBrowse(it.path);
    });
    box.appendChild(row);
  });
  if (!(d.items || []).length) {
    box.innerHTML = '<div class="row" style="color:var(--text-3)">（没有子目录）</div>';
  }
}

$('btnScopedFolderOk').addEventListener('click', async () => {
  $('scopedFolderErr').textContent = '';
  const path = $('scopedPath').value.trim();
  if (!path) { $('scopedFolderErr').textContent = '请填写文件夹路径'; return; }
  const roBtn = document.querySelector('#scopedPermSeg .seg-btn.on');
  const readonly = roBtn ? roBtn.dataset.ro === '1' : false;
  const r = await api('/api/scoped/folder', { user: scopedFolderUser, path, readonly });
  if (r.code !== 0) { $('scopedFolderErr').textContent = r.msg; return; }
  $('modalScopedFolder').classList.remove('on');
  toast(r.msg);
  loadScoped();
});

// ---------- 手机听歌（Subsonic）----------
// 曲库目录的增删已经搬到文件站「音乐」页 —— 加哪个文件夹本来就是边听边决定的事，
// 在管理台做等于让人来回跑。这里只剩「手机怎么连」。

async function loadMusicApp() {
  const app = await api('/api/music/appinfo');
  if (app.code === 0) renderMusicApp(app.data);
}

// 「手机 App 连接」卡片：地址 + 复制按钮 + 密码副本是否就绪的提示。
// 副本缺失时 App 会一路「账号或密码错误」，这是最容易卡住用户的点，必须写明白。
let musicAppURL = '';

function renderMusicApp(d) {
  musicAppURL = d.url || '';
  $('mlAppURL').textContent = musicAppURL || '—';
  const hint = $('mlAppHint');
  // 地址尾注：多数 App 自己拼 /rest，个别要填全路径。两种服务端都认，如实说清楚。
  const tail = d.restURL
    ? ` 有的 App 要求填完整路径，那就填 ${d.restURL}（带不带 /rest 都认）。`
    : '';
  if (!d.adminReady) {
    hint.textContent = '还没生成 App 用的密码副本：先在网页上登录一次后台（或用管理台改一次密码），'
      + '副本就会补上。Subsonic 的 token 鉴权要算 md5(明文+盐)，而库里只存不可逆的哈希。';
    return;
  }
  // 受限账号是建号时就有副本；但老账号或手改过配置的会缺，缺了在 App 里只会显示密码错误
  if (d.pending) {
    hint.textContent = `有 ${d.pending} 个受限账号还没在网页上登录过，App 里用它们会报「账号或密码错误」——`
      + '让本人先用浏览器登录一次，或者你在这儿给他重设一次密码。';
    return;
  }
  hint.textContent = (d.scoped
    ? `账号密码和网页登录一样；受限账号也能连，只能看到他白名单文件夹里的曲目（当前 ${d.scoped} 个受限账号）。`
    : '账号密码和网页登录一样。') + tail;
}

$('mlAppCopy').addEventListener('click', () => copyCmd(musicAppURL, '地址已复制，去 App 的 Server / URL 里填'));

