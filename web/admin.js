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

const TITLES = { overview: '概览', roots: '根目录', remote: '外网访问', musiclib: '手机听歌', account: '账号安全', system: '系统设置', logs: '日志', about: '关于' };
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
  if (tab === 'remote') openNet();
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
  $('infoGuard').textContent = d.settings.adminOnlyLocal ? '仅本机能进' : '局域网与外网通道可进';

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
  guardOn = !!d.settings.adminOnlyLocal;
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
  const mbox = $('mountBox');
  if (mbox) mbox.hidden = true;
  if (!a.supported) {
    cb.disabled = true;
    cb.checked = false;
    if (sw) sw.style.display = '';
    note.textContent = a.detail || '此系统暂不支持';
    hint.style.display = 'none';
    return;
  }

  // 外接盘自动挂载是独立的一块：自启装没装都不影响它，所以在这里就画。
  // （Windows/macOS 系统自己挂，paintMount 会自己隐藏）
  paintMount(a);

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

    renderCmdList($('autostartCmdList'), list);
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

// renderCmdList 渲染一组「说明 + 命令 + 复制按钮」。
function renderCmdList(wrap, list) {
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
}

// paintMount 画「外接盘自动挂载」那一块。只在 Linux（systemd）下出现：
// Windows/macOS 系统自己会挂，用不上这东西。
//
// 跟「开机自启动」完全独立：两件事各装各卸，谁也不替谁做决定。
//
// 开关的语义：**它只反映系统真实状态，点它不会改变状态**（改 /etc 要 root，
// 本进程给不了）。点开的是「要做这件事该执行哪条命令」，执行完刷新页面开关才变。
// 所以点了以后立刻把勾选复位 —— 绝不出现「看着开了其实没开」。
let mhInfo = { installed: false, on: '', off: '' };
let mhOpen = false;

function paintMount(a) {
  const box = $('mountBox');
  if (!box) return;
  if (a.mode !== 'systemd') {
    box.hidden = true;
    return;
  }
  const cb = $('optMountHelper');
  const hint = $('mountHint');
  mhInfo = { installed: !!a.mountHelper, on: a.mountCmd || '', off: a.mountOffCmd || '' };
  box.hidden = false;
  if (cb) cb.checked = mhInfo.installed;

  if (mhInfo.installed) {
    hint.innerHTML =
      '<b>已开启</b>：插入就挂到 <code>/mnt/卷标</code>，拔出自动摘掉，拔了再插回来也自动恢复。' +
      '挂载由系统完成，程序只管读，<b>不需要任何特权</b>。';
  } else {
    hint.innerHTML =
      '<b>现在还不支持插盘即挂</b>。插进来的盘会停在「有文件系统、没挂载」的状态 —— ' +
      '不是没识别到，是本程序以普通用户运行、没有权限替你挂。<br>' +
      '开启之后：插入自动挂到 <code>/mnt/卷标</code>，拔出自动摘，程序只管读。';
  }

  // 命令默认收起，点开关才展开
  mhOpen = false;
  const wrap = $('mountCmdList');
  if (wrap) wrap.innerHTML = '';
  if (cb && !cb.dataset.bound) {
    cb.dataset.bound = '1';
    cb.addEventListener('change', onMountToggle);
  }
}

function onMountToggle() {
  const cb = $('optMountHelper');
  const wrap = $('mountCmdList');
  if (!cb || !wrap) return;
  // 开关点不动：立刻复位成真实状态，只用来触发「展开怎么做」
  cb.checked = mhInfo.installed;

  if (mhOpen) {
    mhOpen = false;
    wrap.innerHTML = '';
    return;
  }
  const want = !mhInfo.installed; // 用户想要的那个方向
  const cmd = want ? mhInfo.on : mhInfo.off;
  if (!cmd) return;
  mhOpen = true;
  renderCmdList(wrap, [
    {
      label: want
        ? '想让它支持：把下面这条复制到终端执行（会要一次管理员密码）。执行完刷新本页，开关就会变成「开」：'
        : '想关掉（已经挂着的盘不受影响，只是以后插盘不再自动挂）：把下面这条复制到终端执行，执行完刷新本页，开关就会变成「关」：',
      cmd: cmd,
    },
  ]);
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
    const mounted = (res.data && res.data.mounted) || [];
    let msg;
    if (added.length && mounted.length) msg = '已挂载 ' + mounted.join('、') + '，新增根目录：' + added.join('、');
    else if (mounted.length) msg = '已挂载：' + mounted.join('、');
    else if (added.length) msg = '已新增根目录：' + added.join('、');
    else msg = res.msg || '没有发现新的磁盘';
    toast(msg);
    if (added.length || mounted.length) loadState();
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

// 上一次加载时「只允许本机」是开是关 —— 用来判断这次是不是要打开它
let guardOn = false;

$('btnSettings').addEventListener('click', async () => {
  // 打开「只允许本机」会把当前这台设备立刻挡在外面（如果现在是别的设备访问的），
  // 而且改回来只能去那台机器上操作 —— 所以先说清楚，再让他点。
  if ($('optAdminOnly').checked && !guardOn) {
    const okGo = confirm(
      '打开后，只有在这台机器上（127.0.0.1 或本机自己的 IP）才能打开管理台：\n' +
        '局域网其它设备、Tailscale、Cloudflare 隧道都会立刻进不来。\n\n' +
        '你现在如果是从别的电脑 / 手机访问的，保存后就再也进不去了，' +
        '只能到那台机器上打开管理台改回来。\n\n确定要打开吗？'
    );
    if (!okGo) return;
  }
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


// ---------- 远程访问（内嵌 Tailscale 节点） ----------
// 客户端已经编进本程序（tailscale.com/tsnet），这台机器上不用装任何东西。
// 只有「第一次」需要出示一次授权：扫码/点链接 → 登录自己的 Tailscale 账号 → 自动上线，之后记住。
// 五态：未开启 / 正在接入 / 等待授权 / 待管理员批准 / 运行中；出错单独一档。
let tsTimer = null;
function tsStopPoll() { if (tsTimer) { clearTimeout(tsTimer); tsTimer = null; } }
function tsPoll(ms) {
  tsStopPoll();
  tsTimer = setTimeout(() => {
    const p = $('panel-remote');
    if (p && p.classList.contains('on')) loadTunnel();
    else tsStopPoll();
  }, ms);
}

async function loadTunnel() {
  tsStopPoll();
  const box = $('tsBox');
  if (!box) return;
  const r = await api('/api/tunnel/status');
  if (r.code !== 0) {
    box.innerHTML = '<p class="err">' + esc(r.msg || '读取状态失败') + '</p>';
    return;
  }
  const d = r.data || {};
  netDot('dotTs', d.state === 'running');
  switch (d.state) {
    case 'running': return renderTsOnline(box, d);
    case 'starting': return renderTsBusy(box, d);
    case 'needslogin': return renderTsLogin(box, d);
    case 'needsauth': return renderTsApprove(box, d);
    case 'error': return renderTsError(box, d);
    default: return renderTsOff(box, d);
  }
}

function tsHead(state, cls, meta) {
  return '<div class="ts-head"><span class="ts-dot ' + cls + '"></span><b>' + esc(state) + '</b>'
    + (meta ? '<span class="ts-meta">' + esc(meta) + '</span>' : '') + '</div>';
}

function tsHostField(d) {
  return '<div class="field" style="max-width:420px"><label>这台机器在网络里的名字</label>'
    + '<input type="text" id="tsHost" value="' + esc(d.hostname || 'selfnas') + '" '
    + 'placeholder="selfnas" autocomplete="off" spellcheck="false"></div>';
}

function tsKeyField() {
  return '<details style="margin:2px 0 4px"><summary class="hint" style="cursor:pointer">'
    + '有 authKey？用它免扫码（可选）</summary>'
    + '<div class="field" style="max-width:420px;margin-top:8px"><label>authKey</label>'
    + '<input type="text" id="tsKey" placeholder="tskey-auth-…" autocomplete="off" spellcheck="false"></div>'
    + '<p class="hint" style="margin:0 0 6px">控制台 Settings → Keys 生成。填了就直连，不用再扫码。</p>'
    + '</details>';
}

function renderTsOff(box, d) {
  const stopped = d.state === 'stopped';
  box.innerHTML = '<div class="ts-card">'
    + tsHead(stopped ? '已断开' : '未开启', '', '自己用 · 这台机器不用装任何东西')
    + '<p class="muted" style="margin:0 0 10px">开启后这台机器会作为一个独立节点加入你的 tailnet，'
    + '<b>只有你 tailnet 里的设备能访问</b>（端到端加密），地址永远固定（<code>100.x.x.x</code> 开头），'
    + '手机/电脑装上 Tailscale App 登同一账号就能连。这台机器上不用装任何软件；'
    + '第一次需要登录一次 Tailscale 账号（免费），之后每次开机自动恢复。</p>'
    + '<p class="hint" style="margin:0 0 12px"><b>用之前先知道：</b>它快不快取决于能不能「直连」——'
    + '笔记本带出门、家里路由器开 UPnP 时成功率高；<b>手机用流量时国内打洞常失败</b>，'
    + '失败就绕海外中继，可能慢到打不开。那种场景用上面的临时地址更实际。</p>'
    + tsHostField(d)
    + tsKeyField()
    + '<div style="margin-top:8px"><button class="btn primary" id="tsOn">开启远程访问</button>'
    + '<a class="btn ghost" style="margin-left:8px" href="https://login.tailscale.com/start" target="_blank" rel="noopener">还没有账号？</a></div>'
    + '<div class="err" id="tsErr"></div></div>';

  $('tsOn').onclick = async () => {
    const btn = $('tsOn');
    btn.disabled = true;
    btn.textContent = '正在开启…';
    $('tsErr').textContent = '';
    const rr = await api('/api/tunnel/up', {
      hostname: ($('tsHost') && $('tsHost').value || '').trim(),
      authKey: ($('tsKey') && $('tsKey').value || '').trim(),
    });
    if (rr.code !== 0) {
      $('tsErr').textContent = rr.msg || '开启失败';
      btn.disabled = false;
      btn.textContent = '开启远程访问';
      return;
    }
    loadTunnel();
  };
}

function renderTsBusy(box, d) {
  box.innerHTML = '<div class="ts-card">'
    + tsHead('正在接入 tailnet…', 'warn', '内置节点')
    + '<p class="muted" style="margin:0">正在和控制面协商，通常 1~3 秒，这页会自动刷新。</p></div>';
  tsPoll(1500);
}

function renderTsLogin(box, d) {
  const url = d.authUrl || '';
  box.innerHTML = '<div class="ts-card">'
    + tsHead('等待登录授权', 'warn', '内置节点 · 只这一次')
    + '<p class="muted" style="margin:0 0 12px">第一次用要授权一次：<b>用手机扫右边的码</b>'
    + '（或点链接），登录你的 Tailscale 账号，这台机器就自动加入了。授权完这里会自己变成「运行中」。</p>'
    + '<div class="ts-qr-flex">'
    + '<div class="cmd-line" style="flex:1 1 240px;min-width:0">'
    + '<div class="cmd-label">授权链接</div>'
    + '<div class="cmd-row"><code>' + esc(url || '（还没拿到，稍等几秒）') + '</code>'
    + '<button class="btn sm" id="tsCopyAuth">复制</button></div></div>'
    + (d.authQr ? '<img class="ts-qr" src="' + d.authQr + '" alt="登录授权二维码">' : '')
    + '</div>'
    + (url ? '<p class="hint" style="margin:10px 0 0">'
      + '<a href="' + esc(url) + '" target="_blank" rel="noopener">在这台电脑的浏览器里打开授权页</a>'
      + '（链接会过期，过期就点下面「重新获取」）</p>' : '')
    + '<div style="margin-top:12px"><button class="btn primary" id="tsRecheck">我已完成，刷新</button>'
    + '<button class="btn sm ghost" id="tsRestart">重新获取授权</button>'
    + '<button class="btn sm ghost" id="tsCancel">取消</button></div>'
    + '</div>';

  if ($('tsCopyAuth') && url) $('tsCopyAuth').onclick = () => copyCmd(url, '授权链接已复制');
  $('tsRecheck').onclick = loadTunnel;
  $('tsRestart').onclick = async () => {
    await api('/api/tunnel/down', {});
    await api('/api/tunnel/up', { hostname: d.hostname || '' });
    loadTunnel();
  };
  $('tsCancel').onclick = tsTurnOff;
  tsPoll(2500);
}

function renderTsApprove(box, d) {
  box.innerHTML = '<div class="ts-card">'
    + tsHead('等管理员批准', 'warn', d.dnsName || d.hostname || '')
    + '<p class="muted" style="margin:0 0 12px">这台设备已经进了 tailnet，但控制台开了「需手动批准设备」。'
    + '去控制台 Machines 里点一下这台机器右侧的 <b>…→ Approve</b>，然后刷新。</p>'
    + '<div><button class="btn primary" id="tsRecheck">已批准，刷新</button>'
    + '<a class="btn sm" href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener">打开控制台</a>'
    + '<button class="btn sm ghost" id="tsCancel">取消</button></div></div>';
  $('tsRecheck').onclick = loadTunnel;
  $('tsCancel').onclick = tsTurnOff;
  tsPoll(4000);
}

function renderTsError(box, d) {
  box.innerHTML = '<div class="ts-card">'
    + tsHead('接入失败', 'warn', d.hostname || '')
    + '<p class="hint" style="color:var(--danger);margin:0 0 12px">' + esc(d.error || '未知错误') + '</p>'
    + '<div><button class="btn primary" id="tsRetry">重试</button>'
    + '<button class="btn sm ghost" id="tsCancel">关闭</button></div>'
    + '<p class="hint" style="margin:10px 0 0">常见原因：这台机器上不了 controlplane.tailscale.com（被防火墙/代理拦住），'
    + '或者系统时间偏差太大。日志里会有 <code>[远程]</code> 开头的记录。</p></div>';
  $('tsRetry').onclick = async () => {
    await api('/api/tunnel/down', {});
    await api('/api/tunnel/up', { hostname: d.hostname || '' });
    loadTunnel();
  };
  $('tsCancel').onclick = tsTurnOff;
}

async function tsTurnOff() {
  const rr = await api('/api/tunnel/down', {});
  toast(rr.code === 0 ? '已断开' : (rr.msg || '断开失败'));
  loadTunnel();
}

function renderTsOnline(box, d) {
  const online = d.online !== false;
  const modeTxt = d.mode === 'relay' ? '中继（' + (d.relay || '自动选择') + '）'
    : d.mode === 'direct' ? '点对点直连' : '已连接';
  const state = online ? '运行中 · ' + modeTxt : '已授权，但当前不在线';
  const cls = !online ? 'warn' : (d.mode === 'relay' ? 'warn' : 'on');
  const meta = [d.hostname, d.dnsName, d.version ? 'v' + d.version : ''].filter(Boolean).join(' · ');

  box.innerHTML = '<div class="ts-card">'
    + tsHead(state, cls, meta)
    + (d.url
      ? '<div class="ts-qr-flex"><div class="cmd-line" style="flex:1 1 240px;min-width:0">'
        + '<div class="cmd-label">外网访问地址（手机 / 电脑要先登同一个 Tailscale 账号）</div>'
        + '<div class="cmd-row"><code>' + esc(d.url) + '</code><button class="btn sm" id="tsCopy">复制</button></div></div>'
        + (d.qr ? '<img class="ts-qr" src="' + d.qr + '" alt="外网访问地址二维码">' : '')
        + '</div>'
      : '')
    + (d.peers ? '<p class="hint" style="margin:10px 0 0">这个 tailnet 里另有 <b>' + d.peers + '</b> 台设备在线，可以直接访问。</p>' : '')
    + (d.mode === 'relay'
      ? '<p class="hint">现在是中继模式：能连，但流量绕了一圈，看 4K 可能慢。两端 NAT 允许时会自动转成直连。'
        + '<b>在外面用手机流量的话，切到「临时地址」标签更实用</b>——国内连 Tailscale 的中继很慢。</p>'
      : '')
    + '<div style="margin-top:12px"><button class="btn sm" id="tsDown">断开</button>'
    + '<a class="btn sm" href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener">打开控制台</a>'
    + '<button class="btn sm ghost" id="tsRecheck">刷新状态</button></div>'
    + '<p class="hint">家里还是走局域网 ' + esc(d.lanURL || '') + '；Tailscale 只解决「在外面」。'
    + '断开不会注销设备，下次打开立刻回来。</p>'
    + '</div>';

  if ($('tsCopy')) $('tsCopy').onclick = () => copyCmd(d.url, '地址已复制，发到手机上就能打开（手机要先登 Tailscale）');
  $('tsRecheck').onclick = loadTunnel;
  $('tsDown').onclick = tsTurnOff;
}

// ---------- 外网访问（Cloudflare Tunnel） ----------
// 两种模式：quick = 免注册临时地址（进程重启会换），named = 用户自己的域名 + 隧道 token。
// 文案上必须把「重启会变」这件事说死——用户最容易被这个坑到，见 renderCfOff 的 cf-warn。
let cfTimer = null;
let cfMode = 'quick';
let netTab = 'quick';

function cfStopPoll() { if (cfTimer) { clearTimeout(cfTimer); cfTimer = null; } }
function cfPoll(ms) {
  cfStopPoll();
  cfTimer = setTimeout(() => {
    const p = $('panel-remote');
    if (p && p.classList.contains('on')) loadCf();
    else cfStopPoll();
  }, ms);
}

// 外层三个 tab：quick / named / ts。cfMode 由 tab 决定，不再自己猜。
// 默认停在「临时地址」（2026-11-17 用户定：按国内真实体验，CF 零门槛且在外面真的好用）。
function netSetTab(t) {
  if (t !== 'quick' && t !== 'named' && t !== 'ts') t = 'quick';
  netTab = t;
  try { localStorage.setItem('netTab', t); } catch (e) {}
  document.querySelectorAll('.net-tab').forEach((b) => b.classList.toggle('on', b.dataset.net === t));
  $('pane-cf').classList.toggle('on', t !== 'ts');
  $('pane-ts').classList.toggle('on', t === 'ts');
  if (t === 'ts') { loadTunnel(); return; }
  cfMode = t;
  loadCf();
}

// tab 上的「运行中」小绿点。三条路可以同时活着（Tailscale + 一条 CF），
// 光看 tab 标题分不出哪个在跑，所以给每个都挂一个点。
function netDot(id, on) {
  const e = $(id);
  if (e) e.classList.toggle('on', !!on);
}

// 进来先看谁在跑：三次请求并行发，先把三个点画对，再决定停在哪个 tab。
// 落位规则（2026-11-17 用户定）：默认「临时地址」；上次自己选过的尊重；
// CF 正在跑时跳到对应 tab（用户多半就是来看它的）。
async function openNet() {
  let t = 'quick';
  try { t = localStorage.getItem('netTab') || 'quick'; } catch (e) {}
  const [rc, rt] = await Promise.all([api('/api/tunnel/cf/status'), api('/api/tunnel/status')]);
  let cfBusy = false;
  if (rc.code === 0) {
    const d = rc.data || {};
    cfBusy = d.state === 'running' || d.state === 'starting';
    netDot('dotQuick', d.state === 'running' && d.mode === 'quick');
    netDot('dotNamed', d.state === 'running' && d.mode === 'named');
    if (cfBusy && d.mode) t = d.mode;
  }
  if (rt.code === 0) netDot('dotTs', (rt.data || {}).state === 'running');
  netSetTab(t);
}

async function loadCf() {
  const box = $('cfBox');
  if (!box) return;
  const r = await api('/api/tunnel/cf/status');
  if (r.code !== 0) {
    box.innerHTML = '<p class="err">' + esc(r.msg || '读取状态失败') + '</p>';
    return;
  }
  const d = r.data || {};
  const busy = d.state === 'running' || d.state === 'starting';
  netDot('dotQuick', d.state === 'running' && d.mode === 'quick');
  netDot('dotNamed', d.state === 'running' && d.mode === 'named');

  // 隧道正用另一种模式跑着：别把「生成地址」按钮摆出来误导人
  if (busy && d.mode && d.mode !== cfMode) {
    const other = d.mode === 'named' ? '固定地址' : '临时地址';
    box.innerHTML = '<div class="cf-pane">'
      + '<div class="ts-head"><span class="ts-dot on"></span><b>' + other + '正在使用</b></div>'
      + '<p class="hint" style="margin:0 0 12px">这条隧道现在以「' + other + '」模式运行着'
      + (d.url ? '：<code>' + esc(d.url) + '</code>' : '') + '。要换成当前这个模式，得先把它关掉。</p>'
      + '<button class="btn sm" id="cfSwitchOff">关掉，改用当前这个</button></div>';
    $('cfSwitchOff').onclick = cfTurnOff;
    return;
  }

  box.innerHTML = '<div id="cfBody"></div>';
  const body = $('cfBody');
  if (d.state === 'running') return renderCfRunning(body, d);
  if (d.state === 'starting') return renderCfBusy(body, d);
  if (d.state === 'error') return renderCfError(body, d);
  renderCfOff(body, d);
}

function cfNoExe() {
  return '<div class="cf-warn"><b>这台机器上没有可用的 cloudflared。</b>'
    + '内置包只在 Windows / Linux / macOS 的 amd64、arm64 五个平台提供；'
    + '其它平台需要自己装一份 <code>cloudflared</code> 并放进 PATH。</div>';
}

function renderCfOff(box, d) {
  if (!d.bundled && d.exe !== 'system') { box.innerHTML = cfNoExe(); return; }

  if (cfMode === 'named') {
    box.innerHTML = '<div class="cf-pane">'
      + '<p class="cf-lead">用你自己的域名做一个固定入口，比如 <code>nas.example.com</code>。<strong>地址永远不变</strong>，发出去一次就够了。</p>'
      + '<div class="cf-warn"><b>先确认两件事：</b>'
      + '① 你有一个域名，而且<strong>域名已经托管在 Cloudflare</strong>（NS 改过去了）——一个域名只能有一家 DNS；'
      + '② 在 Cloudflare 控制台建好隧道，拿到 Token 和一条 CNAME。</div>'
      + '<ol class="cf-steps">'
      + '<li>Cloudflare 控制台 → <b>Zero Trust → Networks → Tunnels → Create a tunnel</b></li>'
      + '<li>建好后点进这条隧道，复制那串 <b>Tunnel Token</b>（以 <code>eyJ</code> 开头）</li>'
      + '<li>在 <b>Public Hostname</b> 里加一条：子域填你想要的（如 <code>nas</code>），域名选你的域名，'
      + 'Service 选 <code>HTTP</code>、地址填 <code>127.0.0.1:' + esc(String((d.target || '').split(':').pop() || '80')) + '</code></li>'
      + '<li>然后把这域名和 Token 填进下面，点开启</li></ol>'
      + '<div class="field" style="max-width:460px"><label>你的域名</label>'
      + '<input type="text" id="cfHost" placeholder="nas.example.com" value="' + esc(d.savedHost || '') + '" autocomplete="off" spellcheck="false"></div>'
      + '<div class="field" style="max-width:460px"><label>Tunnel Token</label>'
      + '<input type="text" id="cfTok" placeholder="eyJhIjoi..." autocomplete="off" spellcheck="false"></div>'
      + '<label class="ts-chk"><input type="checkbox" id="cfAuto" checked> 开机自动开启（机器重启后自动恢复这条隧道）</label>'
      + '<div style="margin-top:12px"><button class="btn primary" id="cfGoNamed">用这个域名固定下来</button></div>'
      + '<p class="hint">Token 相当于这条隧道的钥匙，只存在这台机器的 config.json 里（权限 0600），不会写进日志。'
      + '要撤销就去控制台删掉这条隧道。</p>'
      + '<div class="err" id="cfErr"></div></div>';
    $('cfGoNamed').onclick = async () => {
      const host = ($('cfHost').value || '').trim();
      const tok = ($('cfTok').value || '').trim();
      if (!host) { $('cfErr').textContent = '先填你的域名'; return; }
      if (!tok) { $('cfErr').textContent = '先填 Tunnel Token'; return; }
      const b = $('cfGoNamed');
      b.disabled = true; b.textContent = '正在连接…';
      const rr = await api('/api/tunnel/cf/start', {
        mode: 'named', host: host, token: tok, auto: !!($('cfAuto') && $('cfAuto').checked),
      });
      if (rr.code !== 0) {
        $('cfErr').textContent = rr.msg || '开启失败';
        b.disabled = false; b.textContent = '用这个域名固定下来';
        return;
      }
      $('cfTok').value = '';
      toast('正在连 Cloudflare，稍等几秒');
      cfPoll(1500);
    };
    return;
  }

  box.innerHTML = '<div class="cf-pane">'
    + '<p class="cf-lead">点一下，Cloudflare 会分给你一个临时网址，形如 '
    + '<code>https://xxx.trycloudflare.com</code>。对方用浏览器直接打开就能看，'
    + '<strong>不用装任何东西、不用注册</strong>。</p>'
    + '<div class="cf-warn"><b>但要知道这一点：</b>这个网址是 Cloudflare 临时分配的，'
    + '<strong>这台机器或本程序一重启，地址就会换一个新的</strong>，之前发出去的旧链接会失效。'
    + '要发给别人长期用，切到「固定地址」那个标签。</div>'
    + '<div style="margin-top:4px"><button class="btn primary" id="cfGo">立即生成地址</button></div>'
    + '<p class="hint">一般 5~10 秒出地址。走 Cloudflare 中转，看网页、下载文件都行，'
    + '传几十 G 的大文件就吃力了。在外面用手机流量时，这条通常比 Tailscale 的'
    + '海外中继快得多——国内连它的中继很慢。</p>'
    + '<label class="ts-chk"><input type="checkbox" id="cfAutoQ" checked> 开机自动开启（机器重启后自动重连，地址会换新的）</label>'
    + '<div class="err" id="cfErr"></div></div>';
  $('cfGo').onclick = async () => {
    const b = $('cfGo');
    b.disabled = true; b.textContent = '正在连接…';
    const rr = await api('/api/tunnel/cf/start', {
      mode: 'quick', auto: !!($('cfAutoQ') && $('cfAutoQ').checked),
    });
    if (rr.code !== 0) {
      $('cfErr').textContent = rr.msg || '开启失败';
      b.disabled = false; b.textContent = '立即生成地址';
      return;
    }
    toast('正在连 Cloudflare，稍等几秒');
    cfPoll(1500);
  };
}

function renderCfBusy(box, d) {
  box.innerHTML = '<div class="cf-pane">'
    + '<div class="ts-head"><span class="ts-dot warn"></span><b>正在连 Cloudflare…</b></div>'
    + '<p class="hint">一般是 5~10 秒（实测 6 秒上下）。要是超过半分多钟还没好，多半是这台机器连不上 Cloudflare（防火墙或 DNS）。</p>'
    + '<div style="margin-top:12px"><button class="btn sm ghost" id="cfCancel">取消</button></div></div>';
  $('cfCancel').onclick = cfTurnOff;
  // ⭐ 必须在这里续轮询：入口只排了一次 cfPoll(1500)，如果 1.5 秒后仍是 starting
  // （cloudflared 实测 6 秒左右才出地址），没有这次续拍页面就永远停在「正在连」——
  // 后台其实早就连上了，只是界面不知道。一直轮到状态变成 running / error 为止
  // （那两种渲染不再续拍，轮询自然停）。
  cfPoll(1200);
}

function renderCfRunning(box, d) {
  const named = d.mode === 'named';
  let html = '<div class="cf-pane">'
    + '<div class="ts-head"><span class="ts-dot on"></span><b>'
    + (named ? '固定地址已生效' : '临时地址已生效') + '</b>'
    + (d.exe ? '<span class="ts-meta">'
        + (d.exe === 'builtin' ? '内置 cloudflared ' + (d.version || '') : '系统 cloudflared') + '</span>' : '')
    + '</div>';

  if (d.changed && d.prevUrl) {
    html += '<div class="cf-warn"><b>地址变了。</b>这台机器重启过，Cloudflare 重新分了一个网址：'
      + '旧的 <code>' + esc(d.prevUrl) + '</code> 已经失效，之前发给别人的链接打不开了，需要用下面这个新的重发一次。</div>';
  }

  html += '<div class="ts-qr-flex"><div class="cmd-line" style="flex:1 1 240px;min-width:0">'
    + '<div class="cmd-label">' + (named ? '外网访问地址（永久固定）' : '外网访问地址（临时）') + '</div>'
    + '<div class="cmd-row"><code>' + esc(d.url || '') + '</code><button class="btn sm" id="cfCopy">复制</button></div></div>'
    + (d.qr ? '<img class="ts-qr" src="' + d.qr + '" alt="外网访问地址二维码">' : '')
    + '</div>';

  html += '<div style="margin-top:12px"><button class="btn sm" id="cfStop">关闭外网访问</button>'
    + (named ? '<a class="btn sm" href="https://one.dash.cloudflare.com/" target="_blank" rel="noopener">打开 Cloudflare 控制台</a>' : '')
    + '<button class="btn sm ghost" id="cfRecheck">刷新状态</button></div>';

  html += '<p class="hint">'
    + (named
      ? '别人打开这个地址只需要浏览器。这条隧道走 Cloudflare，适合「看」和「下载」；要传大文件还是走 Tailscale。'
      : '别人打开这个地址只需要浏览器，不用装任何东西。'
        + '关掉或重启后地址会换，要长期固定就切到「固定地址」那个标签。')
    + '</p></div>';

  box.innerHTML = html;
  if ($('cfCopy')) $('cfCopy').onclick = () => copyCmd(d.url, '地址已复制，发给别人就能打开');
  $('cfRecheck').onclick = loadCf;
  $('cfStop').onclick = cfTurnOff;
}

function renderCfError(box, d) {
  box.innerHTML = '<div class="cf-pane">'
    + '<div class="ts-head"><span class="ts-dot warn"></span><b>没连上 Cloudflare</b></div>'
    + '<p class="hint" style="color:var(--danger);margin:0 0 12px">' + esc(d.error || '未知错误') + '</p>'
    + '<div><button class="btn primary" id="cfRetry">重试</button>'
    + '<button class="btn sm ghost" id="cfCancel2">关闭</button></div>'
    + '<p class="hint" style="margin:10px 0 0">常见原因：这台机器上不了 Cloudflare（防火墙拦了 443，或者 DNS 解析不到 argotunnel.com）；'
    + '固定地址模式下还可能是 Token 填错、域名没托管在 Cloudflare。</p></div>';
  $('cfRetry').onclick = () => { cfMode = d.mode || cfMode; loadCf(); };
  $('cfCancel2').onclick = cfTurnOff;
}

async function cfTurnOff() {
  cfStopPoll();
  const rr = await api('/api/tunnel/cf/stop', {});
  toast(rr.code === 0 ? '已关闭' : (rr.msg || '关闭失败'));
  loadCf();
}

// tab 点击：一次绑好，切 tab 只是换面板，不重建按钮
document.querySelectorAll('.net-tab').forEach((b) => {
  b.onclick = () => netSetTab(b.dataset.net);
});
