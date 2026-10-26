const $ = (id) => document.getElementById(id);
// 小请求（登录、列表、断点查询）的容忍时间。正常局域网是 1~5ms，
// 20 秒纯粹是「宁可慢一点也不要永久挂住」的保险丝。
const API_TIMEOUT_MS = 20000;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const S = { roots: [], root: '', rootName: '', rootDrive: '', path: '', readonly: false, entries: [], queue: [], busy: false };
// 4MB：8MB 一片时，一次「回包丢失」就要白等几十秒并且重传 8MB；
// 切到 4MB 后单片失败代价减半，进度也更细。局域网 2-5ms 的 RTT 撑得起这个请求数（253MB≈64 片）。
const CHUNK = 4 * 1024 * 1024;

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.add('on');
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.remove('on'), 2000);
}

async function api(url, body, method) {
  const opt = { method: method || (body ? 'POST' : 'GET'), headers: {}, credentials: 'same-origin' };
  if (body) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  // 超时是**必须**的：iOS Safari 对同一 origin 只有 6 条连接，一旦有僵尸连接占住池子，
  // 小 GET 会静默排队、永远不返回，await 就把整条上传循环挂死在这里（界面停在 100%）。
  // 有超时至少能继续下一轮，或者报错让用户点重试。
  const ctl = new AbortController();
  opt.signal = ctl.signal;
  const killer = setTimeout(() => ctl.abort(), API_TIMEOUT_MS);
  let res;
  try {
    res = await fetch(url, opt);
  } catch (e) {
    throw new Error(e && e.name === 'AbortError' ? '请求超时（连接被占满）' : '连不上服务端');
  } finally {
    clearTimeout(killer);
  }
  let json;
  try { json = await res.json(); } catch (e) { throw new Error('服务端返回异常'); }
  if (json.code === 401) { showLogin(); throw new Error('未登录'); }
  return json;
}

function showLogin() { $('login').classList.add('on'); }

$('loginForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('loginErr').textContent = '';
  try {
    const r = await api('/api/login', { user: $('loginUser').value.trim(), pass: $('loginPass').value });
    if (r.code !== 0) { $('loginErr').textContent = r.msg; return; }
    $('login').classList.remove('on');
    await boot();
  } catch (err) { $('loginErr').textContent = err.message; }
});

function q(root, path) {
  return 'root=' + encodeURIComponent(root) + '&path=' + encodeURIComponent(path || '');
}

// ---------- 数据 ----------

async function loadRoots() {
  const r = await api('/api/fs/roots');
  if (r.code !== 0) return;
  S.roots = r.data || [];
  if (!S.roots.length) {
    $('list').innerHTML = '<div class="empty">还没有启用的根目录，请到管理台「根目录」里添加。</div>';
    return;
  }
  if (!S.root || !S.roots.some((x) => x.path === S.root)) {
    S.root = S.roots[0].path;
    S.rootName = S.roots[0].name;
    S.rootDrive = S.roots[0].drive || '';
    S.path = '';
  }
  renderRoots();
}

// 根名必须带盘符：「资料 / 软件 / 文档」这种名字认不出是哪个物理盘，
// 而用户本来就是靠 D: E: F: 区分盘的（重装系统后盘符还会变）
function rootTag(name, drive) { return drive ? name + ' ' + drive : name; }

function renderRoots() {
  const bar = $('rootsBar');
  bar.innerHTML = '';
  S.roots.forEach((r) => {
    const b = document.createElement('button');
    b.className = 'chip' + (r.path === S.root ? ' on' : '');
    b.textContent = rootTag(r.name, r.drive);
    b.addEventListener('click', () => {
      S.root = r.path;
      S.rootName = r.name;
      S.rootDrive = r.drive || '';
      S.path = '';
      renderRoots();
      loadList();
    });
    bar.appendChild(b);
  });
}

async function loadList() {
  const r = await api('/api/fs/list?' + q(S.root, S.path));
  if (r.code !== 0) {
    $('list').innerHTML = '<div class="empty">' + esc(r.msg) + '</div>';
    return;
  }
  S.entries = r.data.entries || [];
  S.readonly = !!r.data.readonly;
  S.path = r.data.path || '';
  renderList();
  renderCrumb(r.data.parent);
}

function renderCrumb(parent) {
  const box = $('crumb');
  box.innerHTML = '';
  box.style.display = S.path ? 'flex' : 'none';
  if (!S.path) { $('dirTitle').textContent = rootTag(S.rootName, S.rootDrive); }
  const parts = S.path ? S.path.split('/') : [];
  const home = document.createElement('span');
  home.className = 'crumb-item link';
  home.textContent = rootTag(S.rootName, S.rootDrive);
  home.addEventListener('click', () => { S.path = ''; loadList(); });
  box.appendChild(home);
  let acc = [];
  parts.forEach((p, i) => {
    acc.push(p);
    const sep = document.createElement('span');
    sep.className = 'crumb-sep';
    sep.textContent = '/';
    box.appendChild(sep);
    const it = document.createElement('span');
    it.className = 'crumb-item' + (i === parts.length - 1 ? '' : ' link');
    it.textContent = p;
    if (i < parts.length - 1) {
      const target = acc.slice().join('/');
      it.addEventListener('click', () => { S.path = target; loadList(); });
    }
    box.appendChild(it);
  });
  $('dirTitle').textContent = parts.length ? parts[parts.length - 1] : rootTag(S.rootName, S.rootDrive);
  // 副标题带完整磁盘路径（含盘符）：进到深层目录时，光看标题不知道这是哪个盘下的同名文件夹
  const diskPath = S.rootDrive + '\\' + (S.path ? S.path.split('/').join('\\') : '');
  $('dirSub').textContent = (S.readonly ? '只读 · ' : '') + diskPath + ' · ' + S.entries.length + ' 项';

  if (S.path) mountCrumbStar();
}

// 「收藏这个文件夹」的星标，放在面包屑最右端：它旁边就是"你在哪一层"，
// 收藏动作跟位置在同一行，不用去 ⋮ 菜单里翻（那里藏得太深，等于没有）。
function mountCrumbStar() {
  const box = $('crumb');
  const on = isFav(S.root, S.path);
  const b = document.createElement('button');
  b.className = 'crumb-star' + (on ? ' on' : '');
  b.title = on ? '取消收藏这个文件夹' : '收藏这个文件夹';
  b.innerHTML = '<svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M12 3.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8L3.6 9.7l5.8-.8z"/></svg>';
  b.addEventListener('click', async (ev) => {
    ev.stopPropagation();
    const fav = await toggleFav(S.root, S.path);
    if (fav === null) return;
    b.classList.toggle('on', fav);
    b.title = fav ? '取消收藏这个文件夹' : '收藏这个文件夹';
  });
  box.appendChild(b);
}

function iconFor(kind) {
  const c = { dir: '#378ADD', video: '#1D9E75', image: '#BA7517', audio: '#7F77DD', archive: '#888780', file: '#888780' }[kind] || '#888780';
  const g = { dir: 'M3 7h6l2 2h10v10H3z', video: 'M4 5h16v14H4z M10 9l6 3-6 3z', image: 'M4 5h16v14H4z M8 11l3 3 2-2 4 4H7z', audio: 'M9 18V7l8-2v11', archive: 'M4 6h16v4H4z M6 10h12v8H6z', file: 'M6 3h8l4 4v14H6z' }[kind] || 'M6 3h8l4 4v14H6z';
  return '<svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="' + c + '" stroke-width="1.6" stroke-linejoin="round"><path d="' + g + '"/></svg>';
}

function sizeText(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

function dateText(ts) {
  const d = new Date(ts * 1000);
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const p = (x) => String(x).padStart(2, '0');
  return sameDay ? p(d.getHours()) + ':' + p(d.getMinutes())
    : (d.getMonth() + 1) + '月' + d.getDate() + '日';
}

// ---------- 最近 / 收藏（存服务端，跟账号绑定） ----------
// 之前存在 localStorage：换个浏览器就空了，跟「跟账号走」不是一回事。
// 现在落 SQLite（selfnas.db），同一账号在任何设备上看到的都是同一份。
//
// 「最近」记**文件**：点进去先跳到文件所在目录并把它点亮，**不直接打开** ——
// 直接开播会让人失去"我在哪个文件夹"的位置感，退出来还得重新找路。
const FAV_KEY = (root, path) => root + '|' + path;

// 老版本的浏览器本地记录，留一份没意义了，进来先清掉，省得占着 key 让人误会
try { localStorage.removeItem('selfnas.recent.v1'); } catch (e) {}

S.favSet = new Set();   // 已收藏的 root|path，判断行内星标用
S.favItems = [];
S.panelTab = 'recent';

async function loadFavs() {
  try {
    const r = await api('/api/fs/fav');
    if (r.code === 0 && r.data) {
      S.favItems = r.data.items || [];
      S.favSet = new Set(S.favItems.map((x) => FAV_KEY(x.root, x.path)));
    }
  } catch (e) { clog('读收藏失败：' + (e && e.message)); }
}

function isFav(root, path) { return S.favSet.has(FAV_KEY(root, path)); }

// 打开文件就记一笔。失败不打扰用户 —— 记录是附带的，不能因为它挡着看片
function noteRecent(e) {
  if (!e || e.isDir) return;
  api('/api/fs/recent', { root: S.root, path: e.path }).catch(() => {});
}

async function toggleFav(root, path) {
  try {
    const r = await api('/api/fs/fav', { root: root, path: path });
    if (r.code !== 0) { toast(r.msg || '操作失败'); return null; }
    const on = !!(r.data && r.data.favorited);
    if (on) S.favSet.add(FAV_KEY(root, path));
    else S.favSet.delete(FAV_KEY(root, path));
    toast(r.msg);
    return on;
  } catch (err) {
    toast('操作失败：' + err.message);
    return null;
  }
}

function relTime(ts) {
  const t0 = ts > 1e11 ? ts : ts * 1000;   // 服务端给的是秒
  const d = Date.now() - t0;
  if (d < 60e3) return '刚刚';
  if (d < 3600e3) return Math.floor(d / 60e3) + ' 分钟前';
  if (d < 86400e3) return Math.floor(d / 3600e3) + ' 小时前';
  if (d < 7 * 86400e3) return Math.floor(d / 86400e3) + ' 天前';
  const t = new Date(t0);
  return (t.getMonth() + 1) + '月' + t.getDate() + '日';
}

function dirOf(path) {
  const segs = (path || '').split('/');
  segs.pop();
  return segs.join('/');
}

function whereText(e) {
  const d = dirOf(e.path);
  const rootName = rootTag(e.rootName || '根目录', e.rootDrive);
  return d ? rootName + ' / ' + d.split('/').join(' / ') : rootName;
}

async function openPanel(tab) {
  S.panelTab = tab || 'recent';
  $('panelTabs').querySelectorAll('[data-tab]').forEach((b) => {
    b.classList.toggle('on', b.dataset.tab === S.panelTab);
  });
  // 「清空」只对最近有意义；收藏是一条条取消的
  $('recentClear').style.display = S.panelTab === 'recent' ? '' : 'none';
  $('recent').classList.add('on');
  await renderPanel();
}

async function renderPanel() {
  const box = $('recentList');
  if (!box) return;
  box.innerHTML = '<div class="empty">读取中…</div>';
  let items = [];
  try {
    const r = await api(S.panelTab === 'fav' ? '/api/fs/fav' : '/api/fs/recent');
    if (r.code !== 0) { box.innerHTML = '<div class="empty">' + esc(r.msg) + '</div>'; return; }
    items = (r.data && r.data.items) || [];
    if (S.panelTab === 'fav') {
      S.favItems = items;
      S.favSet = new Set(items.map((x) => FAV_KEY(x.root, x.path)));
    }
  } catch (e) {
    box.innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    return;
  }

  box.innerHTML = '';
  if (!items.length) {
    box.innerHTML = '<div class="empty">' +
      (S.panelTab === 'fav'
        ? '还没有收藏。收藏文件夹：进去之后点路径行右边的 ☆；收藏文件：点 ⋮ 里的「收藏」，或在播放页按 ★'
        : '还没有看过文件') +
      '</div>';
    return;
  }

  // 收藏分两组摆：文件夹一类、文件一类。混在一起排，用户会以为「收藏」只对文件生效
  if (S.panelTab === 'fav') {
    const dirs = items.filter((x) => x.isDir);
    const files = items.filter((x) => !x.isDir);
    if (dirs.length) { box.appendChild(groupLabel('文件夹 (' + dirs.length + ')')); dirs.forEach((it) => box.appendChild(panelItem(it))); }
    if (files.length) { box.appendChild(groupLabel('文件 (' + files.length + ')')); files.forEach((it) => box.appendChild(panelItem(it))); }
    return;
  }
  items.forEach((it) => box.appendChild(panelItem(it)));
}

function groupLabel(text) {
  const d = document.createElement('div');
  d.className = 'ri-group';
  d.textContent = text;
  return d;
}

function panelItem(it) {
  // 用 div 不用 button：行内还要放「取消收藏」按钮，button 套 button 是非法结构，
  // 有些浏览器会直接把内层按钮甩到外面去
  const row = document.createElement('div');
  row.className = 'recent-item' + (it.missing ? ' missing' : '');
  row.setAttribute('role', 'button');
  row.tabIndex = 0;

  let tail = '<span class="ri-time">' + relTime(it.ts) + '</span>';
  if (S.panelTab === 'fav') {
    tail = '<button class="ri-fav" title="取消收藏">★</button>';
  } else if (it.missing) {
    tail = '<span class="ri-gone">已不在</span>';
  }

  row.innerHTML =
    '<div class="ico">' + iconFor(it.kind || (it.isDir ? 'dir' : 'file')) + '</div>' +
    '<div class="ri-main">' +
      '<span class="ri-name">' + esc(it.name) + '</span>' +
      '<span class="ri-path">' + esc(whereText(it)) + '</span>' +
    '</div>' +
    tail;

  const star = row.querySelector('.ri-fav');
  if (star) {
    star.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      const on = await toggleFav(it.root, it.path);
      if (on !== null) renderPanel();
    });
  }
  row.addEventListener('click', () => { gotoStored(it); });
  return row;
}

// 跳到记录所在的位置：
//   · 文件 → 进它所在目录 + 滚动高亮那一行，**然后把文件打开**（用户要求：定位完就打开）
//   · 文件夹（收藏才有）→ 直接进去
async function gotoStored(it) {
  let r = S.roots.find((x) => x.path === it.root);
  if (!r && it.rootName) r = S.roots.find((x) => x.name === it.rootName);
  if (!r) { toast('这个位置已经不在了'); return; }

  $('recent').classList.remove('on');
  S.root = r.path;
  S.rootName = r.name;
  S.rootDrive = r.drive || '';
  if (it.isDir) {
    S.path = it.path;
    renderRoots();
    await loadList();
    return;
  }
  S.path = dirOf(it.path);
  renderRoots();
  await loadList();

  const ent = S.entries.find((x) => x.name === it.name && !x.isDir);
  if (!ent) { toast('这个文件已经不在这了'); return; }

  locateRow(it.name);
  if (ent.kind === 'video' || ent.kind === 'image' || ent.kind === 'audio') {
    // 留 260ms 让高亮先画出来：播放器关掉时，人还得能看见自己落在哪一行
    setTimeout(() => openViewer(ent), 260);
    return;
  }
  toast('已定位到所在文件夹（这个类型要先下载才能打开）');
}

// 列表滚到某一行并闪一下。列表里找不到 = 文件被改名/移走了，如实说一句
function locateRow(name) {
  const rows = Array.from(document.querySelectorAll('#list .row'));
  const hit = rows.find((x) => x.dataset.name === name);
  if (!hit) { toast('这个文件已经不在这了'); return; }
  try { hit.scrollIntoView({ block: 'center', behavior: 'smooth' }); } catch (e) { hit.scrollIntoView(); }
  hit.classList.add('hl');
  setTimeout(() => hit.classList.remove('hl'), 2800);
}

// ---------- 排序 ----------
// 只有两个维度：名称、时间；每个维度各记自己的方向，切回来还是上次那个方向，
// 不用重新点一遍（「名称」你可能一直要正序，「时间」你多半要最新在前）。
const SORT_KEY = 'selfnas.sort.v1';

function loadSort() {
  let v = null;
  try { v = JSON.parse(localStorage.getItem(SORT_KEY) || 'null'); } catch (e) {}
  // by 不记忆：每次打开都落在「按文件名」上。上次为了找新片切到「时间」，
  // 结果下次一进来还是时间序，翻文件夹就乱了 —— 维度是当下的意图，方向才是习惯。
  const d = { by: 'name', dir: { name: 'asc', time: 'desc' } };
  if (v && v.dir) {
    if (v.dir.name === 'asc' || v.dir.name === 'desc') d.dir.name = v.dir.name;
    if (v.dir.time === 'asc' || v.dir.time === 'desc') d.dir.time = v.dir.time;
  }
  return d;
}

S.sort = loadSort();

function saveSort() {
  try { localStorage.setItem(SORT_KEY, JSON.stringify(S.sort)); } catch (e) {}
}

function renderSort() {
  const bar = $('sortBar');
  if (!bar) return;
  bar.querySelectorAll('[data-sort]').forEach((b) => {
    const k = b.dataset.sort;
    const on = k === S.sort.by;
    b.className = 'chip' + (on ? ' on' : '');
    // 箭头只画在选中的那个上：两个 chip 各带一个箭头会让人以为能分别控制
    b.textContent = (k === 'name' ? '名称' : '时间') + (on ? (S.sort.dir[k] === 'asc' ? ' ↑' : ' ↓') : '');
  });
}

// 文件夹恒在前：混排时刚建的空文件夹没有大小、没有时间感，混在文件里根本找不到
function sortEntries(list) {
  const by = S.sort.by;
  const sign = S.sort.dir[by] === 'asc' ? 1 : -1;
  const byName = (a, b) => a.name.localeCompare(b.name, 'zh-Hans-CN', { numeric: true, sensitivity: 'base' }) * sign;
  return list.slice().sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    if (by === 'time' && a.mtime !== b.mtime) return (a.mtime - b.mtime) * sign;
    // 同一秒的（批量拷进来的那批）拿名字兜底，否则每次刷新顺序都在跳
    return byName(a, b);
  });
}

function renderList() {
  const box = $('list');
  box.innerHTML = '';
  if (S.path) {
    const up = document.createElement('div');
    up.className = 'row';
    up.innerHTML = '<div class="ico">' + iconFor('dir') + '</div><div class="main"><div class="name">.. 上一级</div></div>';
    up.addEventListener('click', () => {
      S.path = S.path.split('/').slice(0, -1).join('/');
      loadList();
    });
    box.appendChild(up);
  }
  if (!S.entries.length) {
    box.innerHTML += '<div class="empty">这个文件夹是空的</div>';
    return;
  }
  sortEntries(S.entries).forEach((e) => {
    const row = document.createElement('div');
    row.className = 'row';
    row.dataset.name = e.name;   // 「最近/收藏」跳过来靠它定位到具体那一行
    row.innerHTML =
      '<div class="ico">' + iconFor(e.kind) + '</div>' +
      '<div class="main"><div class="name">' + esc(e.name) + '</div>' +
      '<div class="meta">' + (e.isDir ? '文件夹' : sizeText(e.size)) + ' · ' + dateText(e.mtime) + '</div></div>' +
      (isFav(S.root, e.path) ? '<span class="row-star" title="已收藏">★</span>' : '') +
      '<button class="more">⋮</button>';
    row.addEventListener('click', (ev) => {
      if (ev.target.closest('.more')) return;
      openEntry(e);
    });
    row.querySelector('.more').addEventListener('click', (ev) => { ev.stopPropagation(); openSheet(e); });
    box.appendChild(row);
  });
}

function openEntry(e) {
  if (e.isDir) {
    S.path = e.path;
    loadList();
    return;
  }
  if (e.kind === 'video' || e.kind === 'image' || e.kind === 'audio') {
    openViewer(e);
    return;
  }
  if (confirm('下载「' + e.name + '」到本机？')) {
    location.href = '/api/fs/download?' + q(S.root, e.path);
  }
}

// Safari 只认 MP4/MOV 这几种容器，MKV/AVI/TS 递上去就是黑屏转圈，连错误都不给。
const NATIVE_VIDEO = /\.(mp4|m4v|mov)$/i;
const REMUX_VIDEO = /\.(mkv|avi|ts|m2ts|mts|flv|wmv|rmvb|rm|divx|f4v|ogv|vob|mpg|mpeg|webm)$/i;

// 「让 App 去播」——VLC/Infuse/nPlayer 自带 ffmpeg 解封装和音频解码器，
// MKV/AC3/DTS/PGS 字幕都能吃，还能随便拖。浏览器做不到的，交给它们做。
// 外部播放器没有我们的 cookie，所以走服务端签发的短期签名直链。
const PLAYER_BTN = '<button class="btn sm" data-open-player title="用手机上的播放器打开">播放器打开</button>';
let viewerEntry = null;

async function openInPlayer() {
  const e = viewerEntry;
  if (!e) return;
  let r = null;
  try {
    r = await api('/api/fs/link?' + q(S.root, e.path));
  } catch (err) {
    toast('生成链接失败：' + err.message);
    return;
  }
  if (!r || r.code !== 0 || !r.data) {
    toast((r && r.msg) || '生成链接失败');
    return;
  }
  const url = r.data.url;
  let copied = false;
  try {
    await navigator.clipboard.writeText(url);
    copied = true;
  } catch (err) {}
  clog('生成播放器直链 ' + e.name + ' ttl=' + r.data.ttl);
  // iOS 上先试一把 VLC：装了就会直接打开并开播；没装则静默失败，链接已在剪贴板
  if (/iPhone|iPad|iPod/.test(navigator.userAgent)) {
    try { window.location.href = 'vlc-x-callback://x-callback-url/stream?url=' + encodeURIComponent(url); } catch (err) {}
    toast(copied ? '链接已复制：没装 VLC 就粘到播放器的「打开 URL」里' : '请手动复制链接');
    return;
  }
  toast(copied ? '直链已复制，粘到播放器的「打开网络串流」' : '请手动复制链接');
}

function openViewer(e) {
  viewerEntry = e;
  noteRecent(e);            // 打开就算"看过"，记一笔（服务端落库，跟账号绑定）
  syncViewerFav();
  $('viewerName').textContent = e.name;
  const body = $('viewerBody');
  const foot = $('viewerFoot');
  foot.innerHTML = '';
  const dl = '/api/fs/download?' + q(S.root, e.path);
  $('viewer').classList.add('on');
  if (e.kind === 'image') {
    body.innerHTML = '<img src="' + dl + '&inline=1" alt="">';
  } else if (e.kind === 'audio') {
    body.innerHTML = '<audio src="' + dl + '&inline=1" controls autoplay></audio>';
  } else if (e.kind === 'video') {
    if (REMUX_VIDEO.test(e.name) || !NATIVE_VIDEO.test(e.name)) {
      playRemux(e, dl);
      return;
    }
    foot.innerHTML = PLAYER_BTN;
    // 原生 mp4 也走自建控件：全屏/转屏的入口只有一套，别一半原生一半自建
    mountVideo(dl + '&inline=1');
  } else {
    body.innerHTML = '<div class="viewer-msg">这个类型不能预览</div>';
    foot.innerHTML = '<a class="btn" href="' + dl + '">下载到本机</a>';
  }
}

// 浏览器能不能解 HEVC —— 差异比想象的大：Safari/iPhone 能硬解，
// 但 Windows 上的 Chrome 没装 HEVC 扩展就**完全不能解**，表现是「有声音、黑屏、videoWidth=0」。
// 所以不能一律让服务端 copy，得先问自己这一句。
let hevcOK = null;
function canPlayHevc() {
  if (hevcOK !== null) return hevcOK;
  try {
    const v = document.createElement('video');
    hevcOK = !!(v.canPlayType('video/mp4; codecs="hvc1.1.6.L153.B0"') ||
      v.canPlayType('video/mp4; codecs="hev1.1.6.L153.B0"'));
  } catch (e) { hevcOK = false; }
  return hevcOK;
}

// 带计时回调的请求：prep 第一次要让 ffmpeg 把整部片换封装完（700MB 约 4 秒，
// 真转码可能要几分钟），比普通接口慢得多，所以单独给长超时 + 界面走秒。
async function apiWait(url, ms, onTick) {
  const ctl = new AbortController();
  let timer = null;
  const tk = onTick ? setInterval(onTick, 500) : null;
  timer = setTimeout(() => ctl.abort(), ms);
  try {
    const res = await fetch(url, { credentials: 'same-origin', signal: ctl.signal });
    const json = await res.json();
    if (json.code === 401) { showLogin(); throw new Error('未登录'); }
    return json;
  } finally {
    clearTimeout(timer);
    if (tk) clearInterval(tk);
  }
}

// MKV 这类容器 iOS 不认，但里面的编码（HEVC/H.264 + AAC）硬件都解得了。
// ---------- 系统全屏里让画面自己转过来 ----------
//
// iPhone 点原生全屏会跳进系统播放器（AVPlayerViewController），那是苹果的原生界面，
// 网页改不了它任何一个像素，也加不进按钮。唯一能影响它的东西是**视频源本身**：
// MP4 的 tkhd 里有一个 display matrix，播放器读到「转了 90°」就会自己转着放。
//
// 所以这里的做法是：进全屏的瞬间把 src 换成一份「带矩阵标记」的版本（像素没动，
// 服务端 copy 一下就好，700MB 的 4K 集 1 秒出结果），退出全屏再换回来。
function hookNativeRotate(v, fetchRotated) {
  let normalSrc = v.src;
  let rotatedNow = false;
  let busy = false;

  v.addEventListener('webkitbeginfullscreen', async () => {
    if (busy) return;
    // 判断条件就是用户说的那两条：竖着拿手机 + 片子本身是横的（16:9 这种）才转；
    // 9:16 的竖拍素材保持原样，转了反而要歪头看。
    if (!(innerHeight > innerWidth)) return;
    if (!(v.videoWidth > v.videoHeight)) return;
    busy = true;
    const at = v.currentTime;
    try {
      const u = await fetchRotated();
      if (!u) return;
      rotatedNow = true;
      v.src = u;
      // 换源后立刻设 currentTime 是无效的（还没 metadata），要等加载完再跳回去
      v.addEventListener('loadedmetadata', () => {
        try { v.currentTime = at; } catch (e) {}
        v.play().catch(() => {});
      }, { once: true });
      clog('系统全屏：已切到旋转 90° 的源');
    } catch (e) {
      clog('系统全屏切换旋转源失败：' + (e && e.message));
    } finally {
      busy = false;
    }
  });

  v.addEventListener('webkitendfullscreen', () => {
    if (!rotatedNow) return;
    rotatedNow = false;
    const at = v.currentTime;
    v.src = normalSrc;
    v.addEventListener('loadedmetadata', () => {
      try { v.currentTime = at; } catch (e) {}
      v.play().catch(() => {});
    }, { once: true });
  });
}


function mountVideo(src) {
  const host = $('viewerBody');
  host.innerHTML =
    '<video src="' + src + '" controls autoplay playsinline webkit-playsinline preload="metadata"></video>';
  const v = host.querySelector('video');
  v.dataset.normal = src;   // 手动转屏要能换回来
  v.addEventListener('error', () => {
    host.innerHTML = '<div class="viewer-msg">这个文件解不开（可能是损坏或不支持的编码）。</div>';
  });
  return v;
}

// 想要「像 App 一样能拖进度」，光靠流式推是不行的 —— 流里没有索引，浏览器不知道往哪跳。
// 所以服务端把结果**落盘成真正的 MP4（faststart）**，之后交给 ServeFile 走 Range：
// 拖哪播哪、秒开、第二遍播放完全不占 CPU。第一次要等几秒（copy 不解码不编码）。
async function playRemux(e, dl) {
  const body = $('viewerBody');
  const foot = $('viewerFoot');
  body.innerHTML = '<div class="viewer-msg" id="vmLoading">正在准备…</div>';
  foot.innerHTML = '';
  let info = null;
  try {
    const r = await api('/api/fs/playinfo?' + q(S.root, e.path));
    if (r && r.code === 0) info = r.data;
  } catch (err) {}
  if (!info) {
    body.innerHTML = '<div class="viewer-msg">问不到这个文件的信息</div>';
    foot.innerHTML = '<a class="btn" href="' + dl + '">下载到本机</a>';
    return;
  }
  if (!info.ffmpeg) {
    body.innerHTML = '<div class="viewer-msg">服务器上没有 ffmpeg，这个格式没法在线播放。<br>下载到手机用播放器打开吧。</div>';
    foot.innerHTML = '<a class="btn" href="' + dl + '">下载到本机</a>';
    return;
  }
  const hevcOK = canPlayHevc();
  const srcV = info.vcodec || '';
  // 这个客户端要的是 H.264 还是能直接吃源编码
  const need264 = srcV !== 'h264' && !(srcV === 'hevc' && hevcOK);
  clog('播放 ' + e.name + ' v=' + (srcV || '?') + ' a=' + (info.acodec || '?') +
    ' 客户端支持HEVC=' + (hevcOK ? 1 : 0) + ' 要h264=' + (need264 ? 1 : 0));

  const prepBase = '/api/fs/prep?' + q(S.root, e.path) + (need264 ? '&enc=h264' : '');
  const prep264 = '/api/fs/prep?' + q(S.root, e.path) + '&enc=h264';

  // 让服务端在后台把 H.264 版本转好（用户这次先流式看，下次就能拖）。
  // 返回 url 说明「已经有现成的转码缓存」——那就别再去流式转一遍了。
  async function startBgTranscode() {
    try {
      const r = await api(prep264 + '&mode=bg');
      if (r && r.code === 0 && r.data && r.data.ready && r.data.url) {
        clog('转码缓存已就绪，直接切缓存：' + e.name);
        return r.data.url;
      }
      clog('后台转码已排队：' + e.name);
    } catch (err) {}
    return null;
  }

  // ① 先问有没有现成的 MP4 缓存（有就直接秒开、能拖）
  let cached = null;
  try {
    const r = await api(prepBase + '&mode=nowait');
    if (r && r.code === 0 && r.data && r.data.ready && r.data.url) cached = r.data.url;
  } catch (err) {}

  if (cached) {
    // 命中哪一份取决于 prepBase 有没有带 enc=h264：need264 时命中的一定是转码版，
    // 标签得如实说「已转码」，也要跳过对 hvc1 的黑屏兜底检测。
    playFile(cached, need264);
    return;
  }

  // ② 没有缓存：能换封装的（几秒）就现场做，做出来就能拖
  if (!need264) {
    const t0 = Date.now();
    body.innerHTML = '<div class="viewer-msg" id="vmLoading">正在准备可拖动的版本…</div>';
    foot.innerHTML = '<span class="vf-tip">第一次播放需要整理一下，之后就能拖进度了。按「下载」也可以直接带走。</span>';
    let r = null;
    try {
      r = await apiWait(prepBase, 300000, () => {
        const el = $('vmLoading');
        if (el) el.textContent = '正在准备可拖动的版本… ' + Math.floor((Date.now() - t0) / 1000) + 's';
      });
    } catch (err) {
      clog('prep 失败：' + err.message);
    }
    if (r && r.code === 0 && r.data && r.data.url) {
      playFile(r.data.url, false);
      return;
    }
    if (r && r.data && r.data.stream) {
      playStream(need264);
      return;
    }
    // 生成失败（比如磁盘满）也不要卡死在这里，退回流式
    clog('prep 没结果，退回流式播放');
    playStream(need264);
    return;
  }

  // ③ 客户端解不了 HEVC（Windows 版 Chrome）：先流式看着，同时在后台把转码缓存做好，
  //    下次再点开就是能拖的版本。用户不用干等。
  const ready264 = await startBgTranscode();
  if (ready264) {
    playFile(ready264, true);
    return;
  }
  playStream(true);

  // 拿一份「带 90° 矩阵」的同内容版本（服务端只 copy 不重编码，一般 1 秒内）
  async function fetchRotated() {
    const e = viewerEntry;
    if (!e) return null;
    try {
      const r = await apiWait('/api/fs/prep?' + q(S.root, e.path) + '&rot=1&mode=sync', 60000);
      if (r && r.code === 0 && r.data && r.data.url) return r.data.url;
    } catch (err) {
      clog('取旋转源失败：' + (err && err.message));
    }
    return null;
  }

  function playFile(url, transcoded) {
    const tips = [transcoded ? '已转码' : '原画质', '可以拖进度'];
    foot.innerHTML = '<span class="vf-tip">' + esc(tips.join(' · ')) + '</span>' + PLAYER_BTN +
      '<a class="btn sm" href="' + dl + '">下载</a>';
    const v = mountVideo(url);
    hookNativeRotate(v, fetchRotated);
    // 先把旋转版备在后台，用户点全屏时就能秒切（copy 不重编码，一般 1 秒内）
    v.addEventListener('loadedmetadata', () => {
      if (innerHeight > innerWidth && v.videoWidth > v.videoHeight) {
        api('/api/fs/prep?' + q(S.root, e.path) + '&rot=1&mode=bg').catch(() => {});
      }
    }, { once: true });
    // 缓存里存的是 HEVC(hvc1)：浏览器若嘴上说支持、实际解不了，这里同样会黑屏。
    // 4 秒还没画面就切流式转码，并让后台把 H.264 版本慢慢做好（下次可拖）。
    if (srcV !== 'hevc' || transcoded) return;
    setTimeout(() => {
      if (!v.isConnected || v.error || v.videoWidth > 0) return;
      clog('缓存MP4黑屏兜底：切转码流 name=' + e.name + ' readyState=' + v.readyState);
      startBgTranscode().then((u) => {
        if (u) { playFile(u, true); return; }   // 转码缓存已经在了，直接换过去，还能拖
        playStream(true);
      });
    }, 4000);
  }

  // 流式：边转边推，能看但不能拖。只在「拿不到 MP4 缓存」时用
  function playStream(transcode) {
    const url = '/api/fs/play?' + q(S.root, e.path) + (transcode ? '&vc=h264' : '');
    const tips = [transcode ? '边转边播' : '原画质直通', transcode ? '这个模式不能拖进度（下次打开就能拖了）' : '这个模式不能拖进度'];
    foot.innerHTML = '<span class="vf-tip">' + esc(tips.join(' · ')) + '</span>' + PLAYER_BTN +
      '<a class="btn sm" href="' + dl + '">下载</a>';
    const v = mountVideo(url);
    if (transcode) return;

    // 兜底：canPlayType 只是「声明」，不等于真的能解。
    // Windows 版 Chrome 对 HEVC 会说能、然后黑屏有声音（videoWidth 一直是 0）。
    setTimeout(() => {
      if (!v.isConnected || v.error) return;
      if (v.videoWidth > 0) return;
      const at = Math.floor(v.currentTime || 0);
      clog('黑屏兜底：说能解 HEVC 但没画面，切转码重试 at=' + at + 's readyState=' + v.readyState);
      playStreamRaw('/api/fs/play?' + q(S.root, e.path) + '&vc=h264' + (at > 1 ? '&t=' + at : ''), at);
    }, 4000);
  }

  function playStreamRaw(url, at) {
    foot.innerHTML = '<span class="vf-tip">正在转码 · 这个模式不能拖进度</span>' + PLAYER_BTN +
      '<a class="btn sm" href="' + dl + '">下载</a>';
    mountVideo(url);
  }
}

$('viewerFoot').addEventListener('click', (ev) => {
  if (ev.target.closest('[data-open-player]')) openInPlayer();
});

$('viewerClose').addEventListener('click', closeViewer);

// 播放页的星标：边看边收藏，比退出再找那一行顺手
function syncViewerFav() {
  const e = viewerEntry;
  const on = !!(e && isFav(S.root, e.path));
  $('viewerFav').classList.toggle('on', on);
  $('viewerFav').title = on ? '取消收藏' : '收藏';
}

$('viewerFav').addEventListener('click', async () => {
  const e = viewerEntry;
  if (!e) return;
  const on = await toggleFav(S.root, e.path);
  if (on !== null) { syncViewerFav(); renderList(); }
});

// 手动转屏：自动那条路（进系统全屏时换源）依赖播放器认不认 MP4 的旋转矩阵，
// 万一某个设备不认，这里给一个能自己按下、立刻看到效果的开关。
let rotManual = false;
$('viewerRot').addEventListener('click', async () => {
  const v = viewerVideo();
  if (!v) return;
  const at = v.currentTime;
  if (!rotManual) {
    toast('正在准备旋转版…');
    const u = await fetchRotatedSource();
    if (!u) { toast('旋转版暂时生成不了'); return; }
    rotManual = true;
    v.src = u;
  } else {
    rotManual = false;
    v.src = v.dataset.normal;
  }
  v.addEventListener('loadedmetadata', () => {
    try { v.currentTime = at; } catch (e) {}
    v.play().catch(() => {});
  }, { once: true });
  $('viewerRot').classList.toggle('on', rotManual);
  toast(rotManual ? '已转 90°：现在点全屏，画面就是转好铺满的' : '已转回原方向');
});

// ---------- 全屏 / 转屏 ----------
//
// 注意「全屏了还是上下两片黑」不是全屏没生效：.viewer 本来就是 inset:0 铺满视口的。
// 真正的原因是①顶部标题栏 + 底部说明行一直占着位置，②16:9 的片子按宽度等比缩放后，
// 在 20:9 的竖屏里只能占中间那一条。所以这里的「全屏」做三件事：
//   1. 藏掉自己的 bar/foot，画面吃满整个视口
//   2. 能真全屏就真全屏（顺带把地址栏收掉），并尝试锁横屏
//   3. 锁不住方向时（iOS、或手机开了方向锁定）给一个「转 90 度」，竖着拿也能铺满
const V = $('viewer');
const isIOS = /iPhone|iPad|iPod/.test(navigator.userAgent);

function viewerVideo() { return $('viewerBody').querySelector('video'); }

// 取一份「带 90° 矩阵」的同内容版本。服务端只 copy 不重编码，一般 1 秒内。
async function fetchRotatedSource() {
  const e = viewerEntry;
  if (!e) return null;
  try {
    const r = await apiWait('/api/fs/prep?' + q(S.root, e.path) + '&rot=1&mode=sync', 60000);
    if (r && r.code === 0 && r.data && r.data.url) return r.data.url;
  } catch (err) {
    clog('取旋转源失败：' + (err && err.message));
  }
  return null;
}

function lockLandscape() {
  try {
    // 必须在全屏状态里才允许锁；不支持就静默放弃，交给「转 90 度」兜底
    if (screen.orientation && screen.orientation.lock) {
      screen.orientation.lock('landscape').catch(() => {});
    }
  } catch (e) {}
}

function unlockOrient() {
  try {
    if (screen.orientation && screen.orientation.unlock) screen.orientation.unlock();
  } catch (e) {}
}

function setRotate(on) {
  V.classList.toggle('rot', on);
  const b = $('viewerBody').querySelector('[data-act="rotate"]');
  if (b) b.classList.toggle('on', on);
  if (on) hideHint();
}

let hintTimer = 0;
function showHint() {
  $('viewerHint').classList.add('on');
  clearTimeout(hintTimer);
  hintTimer = setTimeout(hideHint, 4000);
}
function hideHint() {
  clearTimeout(hintTimer);
  $('viewerHint').classList.remove('on');
}

function enterFs() {
  V.classList.add('fs');
  clog('进全屏 真全屏API=' + (V.requestFullscreen ? 1 : 0) + ' iOS=' + (isIOS ? 1 : 0));
  if (V.requestFullscreen) {
    V.requestFullscreen().then(lockLandscape).catch(() => {});
  } else if (V.webkitRequestFullscreen) {
    try { V.webkitRequestFullscreen(); } catch (e) {}
    lockLandscape();
  }
  // iOS Safari 不给 div 用 Fullscreen API：上面的 catch 会落到这里，
  // 只能靠 CSS 覆盖 + 转屏，地址栏去不掉，但画面能铺满，够用。
  //
  // 竖着拿手机看横片，不转就一定留黑边。等 350ms 再判断：真全屏若锁上了横屏，
  // 视口会翻过来，这时候就不该再提示了。
  setTimeout(() => {
    if (!V.classList.contains('fs') || V.classList.contains('rot')) return;
    const v = viewerVideo();
    if (v && innerHeight > innerWidth && v.videoWidth > v.videoHeight) showHint();
  }, 350);
  // iPhone 的 Safari 至今不给网页元素真全屏（document.fullscreenEnabled 恒为 false），
  // 地址栏去不掉。唯一真正无边框的路子是「添加到主屏幕」—— 提示一次就好，别每次烦人。
  if (isIOS && !V.requestFullscreen && !localStorage.getItem('selfnas.pwa.hinted')) {
    localStorage.setItem('selfnas.pwa.hinted', '1');
    setTimeout(() => toast('想要完全无边框：Safari 分享 → 添加到主屏幕，之后从桌面打开'), 5000);
  }
}

function exitFs() {
  V.classList.remove('fs');
  setRotate(false);
  hideHint();
  if (document.fullscreenElement) {
    document.exitFullscreen().catch(() => {});
  } else if (document.webkitFullscreenElement) {
    try { document.webkitExitFullscreen(); } catch (e) {}
  }
  unlockOrient();
}

$('hintRotate').addEventListener('click', () => setRotate(true));

// 用户按 ESC / 手势退出浏览器全屏时，把自建的 class 一起收掉，别卡在半全屏状态
function onFsChange() {
  const on = document.fullscreenElement || document.webkitFullscreenElement;
  if (!on && V.classList.contains('fs')) {
    V.classList.remove('fs');
    setRotate(false);
    hideHint();
    unlockOrient();
  }
}
document.addEventListener('fullscreenchange', onFsChange);
document.addEventListener('webkitfullscreenchange', onFsChange);

function closeViewer() {
  exitFs();
  rotManual = false;
  $('viewerRot').classList.remove('on');
  const body = $('viewerBody');
  // 先把 src 摘掉再清 DOM：网络流不会自己断，不主动断的话服务端的 ffmpeg 会一直转下去
  body.querySelectorAll('video, audio').forEach((m) => {
    try { m.pause(); m.removeAttribute('src'); m.load(); } catch (e) {}
  });
  $('viewer').classList.remove('on');
  body.innerHTML = '';
  $('viewerFoot').innerHTML = '';
}

// ---------- 操作面板 ----------

let sheetTarget = null;
function openSheet(e) {
  sheetTarget = e;
  $('sheetTitle').textContent = e.name;
  // 标签跟着状态走：已经收藏的再点一次就是取消，别让人猜
  const favBtn = document.querySelector('#sheet [data-act="fav"]');
  if (favBtn) favBtn.textContent = isFav(S.root, e.path) ? '取消收藏' : '收藏';
  $('sheet').classList.add('on');
}
$('sheetCancel').addEventListener('click', () => $('sheet').classList.remove('on'));
document.querySelectorAll('#sheet .sheet-btns .btn').forEach((b) => {
  b.addEventListener('click', async () => {
    const act = b.dataset.act;
    const e = sheetTarget;
    $('sheet').classList.remove('on');
    if (!e) return;
    if (act === 'fav') {
      const on = await toggleFav(S.root, e.path);
      if (on !== null) renderList();
    } else if (act === 'download') {
      location.href = '/api/fs/download?' + q(S.root, e.path);
    } else if (act === 'rename') {
      askText('重命名', e.name, async (val) => {
        const r = await api('/api/fs/rename', { root: S.root, path: e.path, newName: val });
        toast(r.msg);
        if (r.code === 0) loadList();
        return r.code === 0;
      });
    } else if (act === 'move') {
      openMove(e);
    } else if (act === 'delete') {
      if (!confirm('删除「' + e.name + '」？\n' + (e.isDir ? '文件夹会连同里面所有内容一起删除，' : '') + '此操作不可恢复。')) return;
      const r = await api('/api/fs/delete', { root: S.root, path: e.path });
      toast(r.msg);
      if (r.code === 0) loadList();
    }
  });
});

// ---------- 移动到…（在同一个根目录里换文件夹） ----------
let mvEntry = null;
let mvPath = '';

function openMove(e) {
  mvEntry = e;
  mvPath = S.path;
  $('mvSrc').textContent = '把「' + e.name + '」移动到：';
  $('mvErr').textContent = '';
  $('mvSheet').classList.add('on');
  loadMv();
}

async function loadMv() {
  // 面包屑：根 / a / b，每一级都能点回去
  const segs = mvPath ? mvPath.split('/') : [];
  let html = '<span class="crumb-item link" data-mv="">' + esc(rootTag(S.rootName || '根目录', S.rootDrive)) + '</span>';
  let acc = '';
  segs.forEach((p) => {
    acc = acc ? acc + '/' + p : p;
    html += '<span class="crumb-sep">/</span><span class="crumb-item link" data-mv="' + esc(acc) + '">' + esc(p) + '</span>';
  });
  const crumb = $('mvCrumb');
  crumb.innerHTML = html;
  crumb.querySelectorAll('[data-mv]').forEach((el) => {
    el.addEventListener('click', () => { mvPath = el.dataset.mv; loadMv(); });
  });

  let list = [];
  try {
    const r = await api('/api/fs/list?' + q(S.root, mvPath));
    if (r.code === 0 && r.data) list = (r.data.entries || []).filter((x) => x.isDir);
  } catch (err) {
    $('mvErr').textContent = '读不到这个文件夹';
  }
  // 不能移进自己（文件夹）
  list = list.filter((x) => !(mvEntry && mvEntry.isDir && x.path === mvEntry.path));

  const box = $('mvList');
  box.innerHTML = list.length
    ? list.map((x) => '<div class="mv-row" data-p="' + esc(x.path) + '">' +
        '<div class="ico">' + iconFor('dir') + '</div><div class="main">' + esc(x.name) + '</div></div>').join('')
    : '<div class="mv-empty">' + (mvPath ? '这里没有子文件夹' : '根目录没有子文件夹') + '</div>';
  box.querySelectorAll('.mv-row').forEach((el) => {
    el.addEventListener('click', () => { mvPath = el.dataset.p; loadMv(); });
  });
}

$('mvCancel').addEventListener('click', () => $('mvSheet').classList.remove('on'));
$('mvOk').addEventListener('click', async () => {
  if (!mvEntry) return;
  if (mvPath === S.path) { $('mvErr').textContent = '已经在这个文件夹里了'; return; }
  $('mvErr').textContent = '';
  const r = await api('/api/fs/move', { root: S.root, path: mvEntry.path, destDir: mvPath });
  if (r.code !== 0) { $('mvErr').textContent = r.msg || '移动失败'; return; }
  $('mvSheet').classList.remove('on');
  toast(r.msg || '已移动');
  loadList();
});

let promptHandler = null;function askText(title, value, handler) {
  $('promptTitle').textContent = title;
  $('promptInput').value = value || '';
  $('promptErr').textContent = '';
  promptHandler = handler;
  $('prompt').classList.add('on');
  setTimeout(() => $('promptInput').focus(), 50);
}
$('promptCancel').addEventListener('click', () => $('prompt').classList.remove('on'));
$('promptOk').addEventListener('click', async () => {
  const val = $('promptInput').value.trim();
  if (!val) { $('promptErr').textContent = '不能为空'; return; }
  const ok = await promptHandler(val);
  if (ok) $('prompt').classList.remove('on');
  else $('promptErr').textContent = '操作失败';
});
$('promptInput').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('promptOk').click(); });

$('btnMkdir').addEventListener('click', () => {
  if (S.readonly) { toast('该目录为只读'); return; }
  askText('新建文件夹', '', async (val) => {
    const r = await api('/api/fs/mkdir', { root: S.root, dir: S.path, name: val });
    toast(r.msg);
    if (r.code === 0) loadList();
    return r.code === 0;
  });
});

// ---------- 上传 ----------

// 把手机端的关键动作回报到服务端日志，否则「点了勾没反应」只能靠猜
function clog(msg) {
  try {
    fetch('/api/fs/clientlog', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ msg: String(msg) }),
      credentials: 'same-origin',
      keepalive: true,
    }).catch(() => {});
  } catch (e) {}
}

// 用 label[for] 原生激活，不要用 JS 伪造 click（iOS 上会导致 change 不触发、选择器不关闭）
//
// ⚠️ 清空 value 只能放在「点开选择器之前」，绝不能放在选完之后。
// iOS 上选完再清空，会把 File 背后的安全书签释放掉，之后每次 t.file.slice() 都读不出字节：
// 表现为第一片 8MB 能传完，第二片请求头发出去了但 body 一个字节不来，进度永远停在原地（实测 22:17:59）。
// 「上传文件」和「从「文件」选」共用同一套等待状态与回调，只是开的是两个不同的 input。
function beginPick(inputId, tag) {
  if (S.readonly) { toast('该目录为只读'); return false; }
  clog('点击上传按钮[' + tag + '] readonly=' + S.readonly + ' busy=' + S.busy);
  // 只在没有任务在跑时清空：正在上传时清空会把当前文件的句柄一起放掉
  if (!S.busy) {
    try { $(inputId).value = ''; } catch (err) {}
    lastPickSig = '';
  }
  // 从「照片」里选大视频时，iOS 要先把原件导出成临时文件（iCloud 原件还得先下载），
  // 这段时间选择器会一直转圈、窗口关不掉，而且完全发生在 change 之前 —— 网页拿不到控制权。
  // 能做的只有：让按钮显示「读取中 Ns」，让用户知道在等、等多久。
  picking = true;
  pickStartAt = Date.now();
  paintPickButton();
  // 保险：用户取消选择时 change 不会触发，页面上就没有任何信号能收尾。
  // 2 分钟还没动静就把「读取中」收掉（真机实测最慢的一次导出是 38 秒，2 分钟够宽）；
  // 只影响按钮文字，不动 pickStartAt —— 万一 change 姗姗来迟，还想量到真实耗时。
  if (pickTimer) clearTimeout(pickTimer);
  pickTimer = setTimeout(() => {
    pickTimer = null;
    if (picking) { picking = false; paintPickButton(); }
  }, 120000);
  return true;
}

$('btnUpload').addEventListener('click', (e) => {
  if (!beginPick('filePicker', '照片/系统选择器')) e.preventDefault();
});

let lastHandledSig = '';
let lastHandledAt = 0;

function handleFiles(files, via) {
  if (!files.length) {
    clog('选文件回调[' + via + '] 拿到 0 个文件');
    return;
  }
  clog('选文件回调[' + via + '] 共 ' + files.length + ' 个: ' +
    files.map((f) => f.name + ':' + f.size).join(', '));
  // 有些 iOS 版本会把同一批文件派发两次，3 秒内同签名直接忽略
  const sig = sigOf(files);
  if (sig === lastHandledSig && Date.now() - lastHandledAt < 3000) {
    clog('重复派发，已忽略');
    return;
  }
  lastHandledSig = sig;
  lastHandledAt = Date.now();

  const empty = files.filter((f) => !f.size);
  if (empty.length && empty.length === files.length) {
    toast('文件还是空的（可能仍在从 iCloud 下载），请稍后重试');
    return;
  }
  files.forEach((f) => S.queue.push({ id: ++TASK_SEQ, file: f, done: false, size: f.size, err: '', doneBytes: 0, partial: {}, conc: 2 }));
  toast('已选择 ' + files.length + ' 个文件，开始上传');
  renderUpload();
  kick();
}

let TASK_SEQ = 0;

let lastPickSig = '';
// 「选择器已打开、系统还在准备文件」的状态。这段等待发生在系统进程里，页面只能呈现、不能加速。
let picking = false;
let pickStartAt = 0;
let pickTimer = null;

// 等待状态放在按钮文字上，不再弹框（2026-10-27 用户：「那个框没什么用」）。
// 原来那版在页面重新可见 350ms 后就被 harvest 收掉，正好是系统还在导出的时候，
// 于是它闪一下就没 —— 既没用又容易让人误会。按钮本来就是用户刚点的那个东西，
// 把「读取中 12s」写在它身上，既不用移动视线，也不会挡住列表。
const BTN_UPLOAD_TEXT = '上传文件';
function paintPickButton() {
  const b = $('btnUpload');
  if (!b || b.textContent === undefined) return;
  if (picking) {
    const s = pickStartAt ? Math.floor((Date.now() - pickStartAt) / 1000) : 0;
    b.textContent = s >= 1 ? '读取中 ' + s + 's' : '读取中…';
    b.classList.add('busy');
  } else {
    b.textContent = BTN_UPLOAD_TEXT;
    b.classList.remove('busy');
  }
}
function sigOf(files) {
  return Array.from(files).map((f) => f.name + ':' + f.size + ':' + f.lastModified).join('|');
}

function onPicked(e, tag) {
  let files = [];
  try {
    files = Array.from(e.target.files || []);
  } catch (err) {
    files = [];
  }
  // 量一下「点开选择器 → 系统把文件准备好」到底花了多久。这个数字只能由系统决定，
  // 记下来是为了区分「iOS 导出慢」和「我们的代码卡住」。
  if (pickStartAt) {
    clog('系统准备耗时 ' + ((Date.now() - pickStartAt) / 1000).toFixed(1) + 's[' + tag + ']，拿到 ' + files.length + ' 个文件');
    pickStartAt = 0;
  }
  picking = false;
  if (pickTimer) { clearTimeout(pickTimer); pickTimer = null; }
  paintPickButton();
  lastPickSig = sigOf(files);
  handleFiles(files, tag);   // 刻意不清空 value，见上面 beginPick 的注释
}

$('filePicker').addEventListener('change', (e) => onPicked(e, 'change'));

// 兜底：iOS 上 change 偶发不触发（尤其从「照片」里选大视频、选择器被系统回收时）。
// 页面重新可见 / 窗口重新获得焦点后主动去 input.files 里捡一次，避免白选。
function harvest() {
  const input = $('filePicker');
  const files = input && input.files;
  if (!files || !files.length) {
    // 这里**不要**因为「input 还是空的」就把「读取中」收掉。
    // 页面变可见比 change 早，而系统导出原件的那几十秒里 input.files 恰好也是空的 ——
    // 早先在这里收掉，占位就只闪一下消失，等真正难熬的 30 秒里反而什么都没有。
    // 收尾交给 pickTimer（2 分钟）或 change 自己；只影响按钮文字，不影响上传。
    paintPickButton();
    return;
  }
  const sig = sigOf(files);
  if (sig === lastPickSig) {
    // 还是上次那批文件（用户点了取消，input 保留旧值）：把「读取中」收掉，别一直挂着
    if (picking) { picking = false; paintPickButton(); }
    return;
  }
  lastPickSig = sig;
  picking = false;
  pickStartAt = 0;
  if (pickTimer) { clearTimeout(pickTimer); pickTimer = null; }
  paintPickButton();
  handleFiles(Array.from(files), 'focus-harvest');
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) {
    setTimeout(harvest, 350);
    // 从别的应用切回来：屏幕唤醒锁在页面不可见时会被系统自动释放，
    // 不重新申请的话屏幕会自己黑掉，黑屏再久一点页面就可能被系统回收、上传白传。
    // pump() 在还要接着传的时候会自己接上（busy 时只是排队，不会重入）。
    if (S.queue.some((t) => !t.done && !t.err)) { acquireWake(); pump(); }
  }
});
window.addEventListener('focus', () => setTimeout(harvest, 350));
window.addEventListener('pageshow', (e) => { if (e.persisted) setTimeout(harvest, 350); });

let wakeLock = null;
async function acquireWake() {
  try {
    if ('wakeLock' in navigator && !wakeLock) {
      wakeLock = await navigator.wakeLock.request('screen');
      wakeLock.addEventListener('release', () => { wakeLock = null; });
    }
  } catch (e) {}
}
function releaseWake() { try { if (wakeLock) wakeLock.release(); } catch (e) {} wakeLock = null; }

let pumpQueued = false;

// 点「重试」的时刻可能正好卡在上一轮 pump 收尾（S.busy 还是 true），
// 直接 pump() 会被 if (S.busy) return 吞掉，表现为「点了重试没反应」。排队补一次。
function kick() {
  if (S.busy) { pumpQueued = true; return; }
  pump();
}

async function pump() {
  if (S.busy) { pumpQueued = true; return; }
  S.busy = true;
  pumpQueued = false;
  await acquireWake();
  renderUpload();
  while (S.queue.some((t) => !t.done && !t.err)) {
    const task = S.queue.find((t) => !t.done && !t.err);
    // 单个文件出异常（比如 File 句柄失效导致 slice 抛错）不能让 pump 直接 reject：
    // 那样 S.busy 永远是 true，界面永远停在「上传中」，连重试按钮都不出来。
    try {
      await uploadOne(task);
    } catch (err) {
      task.err = '出错了：' + (err && err.message ? err.message : err);
      clog('上传异常 ' + task.file.name + ' : ' + task.err);
    }
    renderUpload();
  }
  releaseWake();
  S.busy = false;
  if (S.queue.every((t) => t.done)) {
    toast('全部上传完成');
    loadList();
    setTimeout(() => { S.queue = []; renderUpload(); }, 2500);
  } else {
    // 有失败项：明确提示，别让用户以为还在跑
    const bad = S.queue.filter((t) => t.err).length;
    if (bad) toast(bad + ' 个文件没传完，点「重试」从断点继续');
  }
  if (pumpQueued) { pumpQueued = false; pump(); }
}

// 发一片：字节交给系统（upload.onload）就算发送完成，**不等 HTTP 响应**。
//
// 为什么敢不等：真机实测（手机经 WiFi 传 253MB）这个接口的回包几乎每一片都收不到，
// 客户端每片白等 5 秒才放弃；而服务端日志显示 4MB 只用了 97ms 就收全写好。
// 也就是说 104 秒里绝大部分时间，是在等一个永远不会到达的 HTTP 响应。
// 既然服务端有权威的区间表，进度就完全以它为准 —— 发出去、放掉连接、去查表。
const SEND_IDLE_MS = 20000;    // 上传过程中连续 20 秒没有新字节才算假死
const SEND_FIRST_MS = 45000;   // 一个字节都还没发出时的容忍时间
// 字节发完之后等回包的时间。这段时间是**独占一条连接**的：iOS 上只有 6 条，
// 每次多等 700ms 就等于少跑 0.7s 的传输。局域网回包实测 1~5ms，服务端把 4MB
// 追加落盘也就几十毫秒，300ms 已是 6~10 倍余量。
// 万一真的超时也不丢数据：下一轮 status 会用服务端的区间表对账，缺了再补。
const SEND_TAIL_MS = 300;
// 硬超时：从 send() 起算，**不随 progress 续期**。前面三种计时器都是「相对最后一次进展」，
// 一旦某个 XHR 的事件不再触发（Safari abort 之后就是会这样），相对计时器就再也不会响，
// promise 永久 pending —— 整轮 sendRound 挂死，界面停在 100%。这道保险丝保证「一定会返回」。
const SEND_HARD_MS = 15000;

// 返回 'sent' | 'timeout' | 'error' | 'aborted'
function postChunk(url, blob, onSent) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url, true);
    xhr.responseType = 'text';
    let done = false;
    let timer = null;
    let bodySent = false;

    const fin = (why) => {
      if (done) return;
      done = true;
      if (timer) { clearTimeout(timer); timer = null; }
      if (hard) { clearTimeout(hard); hard = null; }
      // 主动断开：iOS Safari 对同一 origin 只有 6 条 HTTP/1.1 连接，
      // 不把「响应永不到」的连接掐掉，后面的分片就只能排队。
      if (xhr.readyState !== 4) { try { xhr.abort(); } catch (e) {} }
      resolve(why);
    };
    const arm = (ms) => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => fin(bodySent ? 'sent' : 'timeout'), ms);
    };

    let hard = setTimeout(() => fin(bodySent ? 'sent' : 'timeout'), SEND_HARD_MS);

    if (xhr.upload) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) onSent(e.loaded);
        arm(SEND_IDLE_MS);
      };
      xhr.upload.onload = () => { bodySent = true; arm(SEND_TAIL_MS); };
    }
    xhr.onload = () => fin('sent');
    xhr.onerror = () => fin('error');
    xhr.onabort = () => fin('aborted');

    arm(SEND_FIRST_MS);
    xhr.send(blob);
  });
}

async function statusOf(t, dir) {
  return api('/api/fs/upload/status?root=' + encodeURIComponent(S.root) +
    '&dir=' + encodeURIComponent(dir) + '&name=' + encodeURIComponent(t.file.name) +
    '&size=' + t.size);
}

async function uploadOne(t) {
  const dir = S.path;
  if (!t.size) {
    t.err = '文件大小为 0，可能没从 iCloud 下载完';
    clog('上传中止(大小为0) ' + t.file.name);
    return;
  }
  if (!t.startedAt) t.startedAt = Date.now();
  clog('开始上传 ' + t.file.name + ' size=' + t.size + ' dir=' + dir);

  // ① 问服务端还缺哪几段
  t.phase = '查询断点';
  let missing = [[0, t.size]];
  t.conc = 3;
  try {
    const st = await statusOf(t, dir);
    if (st.code === 0 && st.data) {
      t.conc = st.data.concurrency || 2;
      if (!st.data.exists && Array.isArray(st.data.missing) && st.data.missing.length) {
        missing = st.data.missing;
      }
      const left = missing.reduce((s, r) => s + (r[1] - r[0]), 0);
      t.doneBytes = Math.max(0, t.size - left);
    }
  } catch (e) {
    t.err = '无法获取断点信息';
    clog('上传失败(断点查询) ' + t.file.name + ' : ' + e.message);
    return;
  }

  // ② 把缺失区间切成 4MB 的片
  const chunks = [];
  for (const [a, b] of missing) {
    for (let p = a; p < b; p += CHUNK) chunks.push([p, Math.min(p + CHUNK, b)]);
  }
  if (!chunks.length) { t.done = true; return; }
  clog('待传 ' + chunks.length + ' 片 并发 ' + t.conc + ' 缺失段 ' + missing.length);

  // ③ 乐观发送 + 服务端对账。
  //    每一轮：把所有还缺的片并发发出去（不等回包）→ 等 260ms 让最后几片落盘
  //    → 查一次 status 拿到权威进度 → 还缺的下一轮再发。
  //    好处：回包丢不丢完全不影响；坏处：某片发丢了要等下一轮才发现（一轮也就几百毫秒）。
  // 并发**故意不占满 6**：iOS Safari 对同一 origin 只有 6 条 HTTP/1.1 连接，
  // 全给上传的话，每轮查断点的那条 GET 只能跟 6 条正在传的分片排队 —— 尤其每片
  // 收不到回包时我们会主动 abort，那些连接还占着池子。留 1 条空位，查询永远有路可走。
  // 实测 5 并发在限速 25MB/s 下能跑到 22.7MB/s（91%），不比 6 差。
  const MAXC = 5;
  let conc = Math.max(2, Math.min(MAXC, t.conc || 4));
  t.conc = conc;

  let pending = chunks;
  let stall = 0;
  let lastCovered = t.doneBytes;
  const tStart = Date.now();

  for (let round = 1; round <= 40 && pending.length; round++) {
    const sentCount = pending.length;
    t.round = round;
    t.pendingN = pending.length;
    await sendRound(t, dir, pending, conc);
    await sleep(260);

    let st = null;
    try { st = await statusOf(t, dir); } catch (e) { clog('查断点失败：' + e.message); }
    if (!st || st.code !== 0 || !st.data) {
      // 查询本身就带 20 秒超时了，连着两次拿不到就别再耗，直接让用户重试
      if (++stall >= 2) { t.err = '查不到服务端进度（查询超时），点「重试」继续'; return; }
      continue;
    }
    if (st.data.exists) {
      t.doneBytes = t.size;
      t.done = true;
      t.phase = '';
      clog('上传完成 ' + t.file.name + ' 用时=' + Math.round((Date.now() - tStart) / 1000) +
        's 平均 ' + (t.size / 1048576 / Math.max(1, (Date.now() - tStart) / 1000)).toFixed(2) + 'MB/s');
      return;
    }
    const miss = Array.isArray(st.data.missing) ? st.data.missing : [];
    const left = miss.reduce((s, r) => s + (r[1] - r[0]), 0);
    t.doneBytes = Math.max(0, t.size - left);
    if (!miss.length) {
      t.done = true;
      t.phase = '';
      clog('上传完成 ' + t.file.name + ' 用时=' + Math.round((Date.now() - tStart) / 1000) + 's');
      return;
    }
    // 还缺的重新切片（一次没落全的片会被服务端算成部分覆盖）
    pending = [];
    for (const [a, b] of miss) {
      for (let p = a; p < b; p += CHUNK) pending.push([p, Math.min(p + CHUNK, b)]);
    }
    if (t.doneBytes > lastCovered + 65536) { lastCovered = t.doneBytes; stall = 0; } else { stall++; }
    clog('第 ' + round + ' 轮：发 ' + sentCount + ' 片，服务端还缺 ' + pending.length +
      ' 片（' + sizeText(left) + '）');
    if (stall >= 3) { t.err = '服务端收不到数据了，点「重试」从断点继续'; return; }
  }

  // 轮次用完（或某一轮后 pending 变空）时，不要凭客户端自己的账报结果 ——
  // 再问一次服务端「文件到底落盘了没有」。真机事故里就是客户端以为自己没发完，
  // 服务端其实早就 finalize 了；这条核对能让它自己走完，不用人工点重试。
  //
  // 查 3 次、每次隔 1.2 秒：池子被占满是一时的（abort 掉的那几条连接要过一会儿
  // 才真正还回来），稍等再问往往就通了。宁可多等这两秒，也不要让用户盯着 100% 干瞪眼。
  t.phase = '核对中';
  renderUpload();
  let fin = null;
  for (let i = 0; i < 3; i++) {
    try {
      fin = await statusOf(t, dir);
      if (fin && fin.code === 0 && fin.data) break;
    } catch (e) {
      clog('最终核对第 ' + (i + 1) + ' 次失败：' + e.message);
    }
    fin = null;
    if (i < 2) await sleep(1200);
  }
  t.phase = '';
  if (fin === null && t.doneBytes >= t.size * 0.999) {
    // 三次都查不到，但客户端手里的字节已经发完了。用户此刻能做的只有「看一眼文件在不在」，
    // 所以不报失败，标完成并如实说明「未经服务端确认」，列表刷新会给出真相。
    t.done = true;
    t.doneBytes = t.size;
    toast('已发出，未能与服务端核对（刷新列表确认）');
    clog('上传完成(未核对) ' + t.file.name);
    return;
  }
  if (fin && fin.code === 0 && fin.data) {
    const fm = Array.isArray(fin.data.missing) ? fin.data.missing : [];
    if (fin.data.exists || !fm.length) {
      t.doneBytes = t.size;
      t.done = true;
      clog('上传完成(最终核对) ' + t.file.name + ' exists=' + !!fin.data.exists);
      return;
    }
    const fleft = fm.reduce((s, r) => s + (r[1] - r[0]), 0);
    t.doneBytes = Math.max(0, t.size - fleft);
  }
  t.err = '还差 ' + sizeText(Math.max(0, t.size - t.doneBytes)) + ' 没传完，点「重试」接着传';
}

// 一轮并发发送：每片只要字节交给系统就返回，不等回包。
async function sendRound(t, dir, list, conc) {
  let idx = 0;
  const run = async () => {
    while (true) {
      const i = idx++;
      if (i >= list.length) return;
      const [s, e] = list[i];
      const url = '/api/fs/upload?root=' + encodeURIComponent(S.root) +
        '&dir=' + encodeURIComponent(dir) + '&name=' + encodeURIComponent(t.file.name) +
        '&offset=' + s + '&size=' + t.size;
      t.partial[i] = 0;
      const t0 = Date.now();
      const why = await postChunk(url, t.file.slice(s, e), (loaded) => {
        t.partial[i] = loaded;
        t.lastMove = Date.now();
        scheduleRender();
      });
      delete t.partial[i];
      if (why === 'sent') {
        // 乐观计数：先按已发出算，进度条才会往前走；下一轮 status 会用权威值校正回来。
        // 但**封顶到 99%**：最后那 1% 只有服务端说「文件已经落盘」才算数。
        // 直接把条推满再卡住，用户只会以为程序死了（2026-10-27 就是这么被发现的）。
        t.doneBytes = Math.min(Math.floor(t.size * 0.99), t.doneBytes + (e - s));
        t.lastMove = Date.now();
        scheduleRender();
      } else {
        clog('片 ' + s + '→' + e + ' 未确认：' + why + ' 用时 ' + (Date.now() - t0) + 'ms');
      }
    }
  };
  // 单片抛异常不能带塌整轮：记录后继续发别的片，缺的下一轮补
  await Promise.all(Array.from({ length: Math.min(conc, list.length) }, () => run().catch((err) => {
    clog('发片异常：' + (err && err.message ? err.message : err));
  })));
}

function structKey() {
  return S.queue.map((t) => t.id + '|' + t.file.name + '|' + t.size).join('#');
}

let lastStruct = '';
let renderTimer = null;

// 进度事件一秒能来几十次，全量重绘会把 <i> 换掉、CSS 过渡失效。
// 所以：结构变了才重建 DOM，进度只改宽度和文字。
function scheduleRender() {
  if (renderTimer) return;
  renderTimer = setTimeout(() => { renderTimer = null; paintUpload(); }, 200);
}

function renderUpload() {
  const box = $('uploadBox');
  if (!S.queue.length) {
    box.innerHTML = '';
    box.classList.remove('on');
    lastStruct = '';
    return;
  }
  const key = structKey();
  if (key !== lastStruct) {
    lastStruct = key;
    let html = '<div class="up-head">上传队列 <span class="muted" id="upActive"></span>' +
      '<button class="icon-btn" id="upClose">\u00d7</button></div>';
    S.queue.forEach((t, i) => {
      html += '<div class="up-row">' +
        '<div class="up-name">' + esc(t.file.name) + '</div>' +
        '<div class="up-meta" data-meta="' + i + '"></div>' +
        '<div class="bar"><i data-bar="' + i + '" style="width:0%"></i></div>' +
        '<div class="up-act" data-act="' + i + '"></div>' +
        '</div>';
    });
    box.innerHTML = html;
    box.classList.add('on');
    $('upClose').addEventListener('click', () => {
      if (S.busy) { toast('正在上传中，请等当前文件传完'); return; }
      S.queue = [];
      lastStruct = '';
      renderUpload();
    });
    box.addEventListener('click', (e) => {
      const b = e.target.closest('[data-retry]');
      if (!b) return;
      const t = S.queue[Number(b.dataset.retry)];
      if (!t) return;
      t.err = '';
      t.phase = '重试中…';
      kick();
    });
  }
  paintUpload();
}

function paintUpload() {
  if (!S.queue.length) return;
  const now = Date.now();
  const active = S.queue.filter((t) => !t.done && !t.err).length;
  const a = $('upActive');
  if (a) a.textContent = active ? active + ' 个进行中' : (S.queue.every((t) => t.done) ? '已完成' : '有任务没传完');

  S.queue.forEach((t, i) => {
    const bar = document.querySelector('[data-bar="' + i + '"]');
    const meta = document.querySelector('[data-meta="' + i + '"]');
    const act = document.querySelector('[data-act="' + i + '"]');
    let at = t.doneBytes || 0;
    if (t.partial) { for (const k in t.partial) at += t.partial[k] || 0; }
    at = Math.min(at, t.size || at);
    // 没拿到服务端确认之前最多显示 99%：条推满了却不动，用户只会以为程序死了
    const pct = t.done ? 100 : (t.size ? Math.min(99, Math.floor((at / t.size) * 100)) : 0);

    if (bar) bar.style.width = (t.done ? 100 : pct) + '%';

    let txt;
    if (t.done) {
      txt = sizeText(t.size) + ' \u00b7 完成';
    } else if (t.err) {
      txt = t.err;
    } else {
      const el = t.startedAt ? (now - t.startedAt) / 1000 : 0;
      const spd = el > 1 ? at / el : 0;
      const idle = t.lastMove ? (now - t.lastMove) / 1000 : 0;
      txt = sizeText(at) + ' / ' + sizeText(t.size) + ' \u00b7 ' + pct + '%';
      if (spd > 0) txt += ' \u00b7 ' + sizeText(spd) + '/s';
      if (t.conc > 1) txt += ' \u00b7 ' + t.conc + ' 并发';
      if (t.phase === '核对中') txt += ' \u00b7 正在核对服务端\u2026';
      else if (t.phase === '重试中…') txt += ' \u00b7 重试中';
      else if (idle > 6) txt += ' \u00b7 等待设备读取\u2026';
    }
    if (meta) meta.textContent = txt;

    if (act) {
      const has = act.querySelector('button');
      if (t.err && !has) act.innerHTML = '<button class="btn sm" data-retry="' + i + '">重试</button>';
      else if (!t.err && has) act.innerHTML = '';
    }
  });
}

// 没有进度事件时也要走字，否则「等待设备读取…」永远不出现
setInterval(() => { if (S.busy) paintUpload(); }, 1000);
// 「读取中 Ns」单独一个 0.5 秒的节拍：跟着 1 秒的上传节拍走会滞后将近一秒，
// 用户盯着看会觉得数得不准（实测 4.5 秒时显示 3s）。
setInterval(() => { if (picking) paintPickButton(); }, 500);

// 上传期间每 10 秒往服务端打一条心跳。万一再次出现「卡在某个百分比」，
// 日志里有没有心跳就能一刀切开两种完全不同的病因：
//   有心跳 → JS 还活着，是网络/连接池的问题；没心跳 → 页面被系统挂起（锁屏、切后台）。
setInterval(() => {
  if (!S.busy) return;
  const t = S.queue.find((x) => !x.done && !x.err);
  if (!t) return;
  clog('心跳 ' + t.file.name + ' 第' + (t.round || 0) + '轮 待发' + (t.pendingN || 0) +
    ' 已确认' + sizeText(t.doneBytes || 0) + '/' + sizeText(t.size));
}, 10000);

// ---------- 其他 ----------

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

$('btnRefresh').addEventListener('click', () => loadList());

$('btnRecent').addEventListener('click', () => openPanel('recent'));
$('btnFav').addEventListener('click', () => openPanel('fav'));
$('panelTabs').querySelectorAll('[data-tab]').forEach((b) => {
  b.addEventListener('click', async () => {
    if (S.panelTab === b.dataset.tab) return;
    S.panelTab = b.dataset.tab;
    $('panelTabs').querySelectorAll('[data-tab]').forEach((x) => x.classList.toggle('on', x === b));
    $('recentClear').style.display = S.panelTab === 'recent' ? '' : 'none';
    await renderPanel();
  });
});
$('recentClose').addEventListener('click', () => $('recent').classList.remove('on'));
$('recentClear').addEventListener('click', async () => {
  if (!confirm('清空「最近」记录？只删记录，不动文件。')) return;
  try {
    const r = await api('/api/fs/recent/clear', {});
    toast(r.msg || '已清空');
    await renderPanel();
  } catch (e) { toast('清空失败：' + e.message); }
});
// 点遮罩空白处关掉（弹层本身是 .modal，点它不该关）
$('recent').addEventListener('click', (e) => {
  if (e.target === $('recent')) $('recent').classList.remove('on');
});


// 点同一个 chip = 翻方向；点另一个 chip = 换维度（沿用该维度上次的方向）
document.querySelectorAll('#sortBar [data-sort]').forEach((b) => {
  b.addEventListener('click', () => {
    const k = b.dataset.sort;
    if (S.sort.by === k) {
      S.sort.dir[k] = S.sort.dir[k] === 'asc' ? 'desc' : 'asc';
    } else {
      S.sort.by = k;
    }
    saveSort();
    renderSort();
    renderList();   // 只重排列表，不重新请求：排序是本地行为
  });
});
renderSort();


async function boot() {
  clog('文件页加载 standalone=' + (window.navigator.standalone ? 1 : 0) +
    ' dpr=' + (window.devicePixelRatio || 1) + ' w=' + window.innerWidth);
  try {
    await loadRoots();
    await loadFavs();      // 先拿一次收藏，列表里的 ★ 才不会漏画
    if (S.root) await loadList();
  } catch (e) {
    if (e.message !== '未登录') toast(e.message);
  }
}

boot();
