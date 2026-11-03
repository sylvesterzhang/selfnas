const $ = (id) => document.getElementById(id);
// 小请求（登录、列表、断点查询）的容忍时间。正常局域网是 1~5ms，
// 20 秒纯粹是「宁可慢一点也不要永久挂住」的保险丝。
const API_TIMEOUT_MS = 20000;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const S = { roots: [], root: '', rootName: '', rootDrive: '', path: '', readonly: false, entries: [], queue: [], busy: false, lastOpen: null, tags: {} };
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

function showLogin() {
  $('login').classList.add('on');
  // 登录遮罩压在 FAB 之上，菜单若还开着，重新登进来会看到一串残留在半空的按钮
  const fab = $('fsFab');
  if (fab) fab.classList.remove('on');
}

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

// 皮肤应用：管理台全局配置，恒为六套之一（旧版「跟随系统」已取消，空值兜底亮色）。
// 只在登录态加载时应用一次；切皮肤要去管理台改，改完刷新 files 页生效。
function applyTheme(t) {
  if (t !== 'light' && t !== 'blue' && t !== 'pink' && t !== 'ebook' && t !== 'green' && t !== 'dark') {
    t = 'light';
  }
  document.documentElement.setAttribute('data-theme', t);
}

async function loadRoots() {
  const r = await api('/api/fs/roots');
  if (r.code !== 0) return;
  // 没有可用根时后端给的是 {roots: null}，原来的 `|| r.data` 会把整个对象当数组存下来，
  // 后面 S.roots.map / forEach 全炸 —— 这里按数组兜一次
  const rd = r.data;
  S.roots = Array.isArray(rd) ? rd : (Array.isArray(rd && rd.roots) ? rd.roots : []);
  S.user = (r.data && r.data.user) || '';
  applyTheme((r.data && r.data.theme) || 'light');
  // 受限账号没有管理台权限，齿轮藏掉（点进去也只有 401）
  const adminLink = $('btnAdminLink');
  if (adminLink && S.user && S.user !== 'admin') adminLink.style.display = 'none';
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
  // 这里**不画**根栏：boot 还要去查「上次浏览的目录」，等最终确定选哪个根再画一次。
  // 先画默认再跳恢复目标，刷新时芯片会先亮 D 再跳到 E —— 用户看到的就是"闪一下"。
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
    b.title = rootTag(r.name, r.drive) + (r.readonly ? '（只读）' : '');
    b.innerHTML = esc(rootTag(r.name, r.drive)) +
      // 只读根在盘符后面挂个角标：根目录没有面包屑行，「只读 · N 项」看不到，
      // 不这样标一下用户要点了上传才知道不能写
      (r.readonly ? '<span class="chip-ro">只读</span>' : '');
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
    return r.code;   // boot 要靠这个分辨「权限被收走了」还是「服务端抽风」
  }
  S.entries = r.data.entries || [];
  S.readonly = !!r.data.readonly;
  S.path = r.data.path || '';
  renderList();
  renderCrumb(r.data.parent);
  loadTags();   // 目录里有音频才发请求；不 await，列表先出来再说
  saveLastDir();
  return 0;
}

// 上次浏览的目录：按账号存服务端（跟「最近 / 收藏」同一个库），刷新、换设备都回到原地。
// 每次成功落进一个目录就覆盖一次；停根目录也算位置（path 为空），root 为空才代表「没记录」。
function saveLastDir() {
  api('/api/fs/lastdir', { root: S.root, path: S.path }).catch(() => {});
}

// 音频标签单独拉一次：几百首歌的 ID3 不能让列表等着（跟缩略图同一个道理）。
// 拿到之后**就地改已有行**，不重绘整个列表 —— 重绘会把滚动位置打回顶部，
// 几百首的文件夹里用户刚滚到一半，跳回顶上会骂人。
async function loadTags() {
  const dir = S.path;
  const root = S.root;
  if (!S.entries.some((e) => e.kind === 'audio')) return;
  const key = root + '|' + dir;
  if (S.tags[key]) { applyTags(); return; }
  try {
    const r = await api('/api/fs/tags?' + q(root, dir));
    if (r.code !== 0) return;
    const map = {};
    (r.data || []).forEach((t) => { map[t.name] = t; });
    S.tags[key] = map;
    // 缓存只在一次会话里有用（翻回上一个目录不用再请求），别让它无限长：
    // 逛几十个音乐目录、每目录几百条，攒起来就是几 MB 的垃圾
    const keys = Object.keys(S.tags);
    if (keys.length > 30) delete S.tags[keys[0]];
    // 请求飞在路上时用户可能已经翻到别的目录了，别把 A 目录的标签贴到 B 目录
    if (S.root === root && S.path === dir) { applyTags(); if (MP.on) mpRender(); }
  } catch (e) { /* 标签拿不到就显示文件名，不影响任何功能 */ }
}

function tagOf(e) {
  if (!e || e.kind !== 'audio') return null;
  const map = S.tags[S.root + '|' + S.path];
  return (map && map[e.name]) || null;
}

// 把已经画好的音频行改成「歌名 · 歌手」（数据到了才调）
function applyTags() {
  document.querySelectorAll('#list .row[data-audio]').forEach((row) => {
    const e = S.entries.find((x) => x.name === row.dataset.name && !x.isDir);
    if (!e) return;
    const t = tagOf(e);
    if (!t || (!t.title && !t.artist)) return;
    const nameEl = row.querySelector('.name');
    if (nameEl && t.title) { nameEl.textContent = t.title; nameEl.title = e.name; }
    // 歌手小字：首次渲染时标签还没到，这里补建元素（插在收藏星前面，保持星在最后）
    if (t.artist) {
      let sub = row.querySelector('.row-sub');
      if (!sub) {
        sub = document.createElement('span');
        sub.className = 'row-sub';
        const line = row.querySelector('.name-line');
        if (line) line.insertBefore(sub, row.querySelector('.row-star'));
      }
      sub.textContent = t.artist;
    }
    const metaEl = row.querySelector('.meta');
    if (metaEl && t.artist) metaEl.textContent = t.artist + ' · ' + sizeText(e.size) + ' · ' + dateText(e.mtime);
  });
}

function renderCrumb(parent) {
  const box = $('crumb');
  box.innerHTML = '';
  const parts = S.path ? S.path.split('/') : [];
  const home = document.createElement('span');
  home.className = 'crumb-item' + (parts.length ? ' link' : '');
  // 盘符这一项恒写「资料 D:」：根目录时这整行是隐藏的，不用再为"跟标题重复"做特殊处理
  home.textContent = rootTag(S.rootName, S.rootDrive);
  if (parts.length) home.addEventListener('click', () => { S.path = ''; loadList(); });
  box.appendChild(home);
  let acc = [];
  parts.forEach((p, i) => {
    acc.push(p);
    const sep = document.createElement('span');
    sep.className = 'crumb-sep';
    sep.textContent = '/';
    box.appendChild(sep);
    const last = i === parts.length - 1;
    const it = document.createElement('span');
    // 当前这一层用「正文色」加重：一节节都是灰的，看不出哪个是"我现在在这儿"
    it.className = 'crumb-item' + (last ? ' on' : ' link');
    it.textContent = p;
    if (!last) {
      const target = acc.slice().join('/');
      it.addEventListener('click', () => { S.path = target; loadList(); });
    }
    box.appendChild(it);
  });
  // 根目录不显示这一行：面包屑只剩「资料 D:」，而盘符条已经选中了那个盘，说了等于没说的东西不占高度
  $('fsNav').hidden = !S.path;
  // 项数：只做个旁注，跟在面包屑右端（只读账号额外标出来）
  $('dirCount').textContent = (S.readonly ? '只读 · ' : '') + S.entries.length + ' 项';

  if (S.path) mountCrumbStar();
  else $('crumbStar').hidden = true;   // 根目录没有「收藏这个文件夹」可点（收藏的是子文件夹）
}

// 「收藏这个文件夹」的星标：常驻在路径下方的工具行左端，跟排序同一排。
// 收藏动作和「你在哪一层」上下贴着，不用去 ⋮ 菜单里翻（那里藏得太深，等于没有）。
// 光一颗灰星没人知道能点、更不知道是收藏功能（用户实测反馈），
// 所以做成带文字的药丸按钮：「☆ 收藏」→「★ 已收藏」，功能自己会说话。
const STAR_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M12 3.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8L3.6 9.7l5.8-.8z"/></svg>';

function mountCrumbStar() {
  const b = $('crumbStar');
  const on = isFav(S.root, S.path);
  b.hidden = false;
  b.classList.toggle('on', on);
  b.title = on ? '取消收藏这个文件夹' : '收藏这个文件夹';
  b.innerHTML = STAR_SVG + '<span>' + (on ? '已收藏' : '收藏') + '</span>';
  b.onclick = async () => {
    const fav = await toggleFav(S.root, S.path);
    if (fav === null) return;
    b.classList.toggle('on', fav);
    b.title = fav ? '取消收藏这个文件夹' : '收藏这个文件夹';
    b.querySelector('span').textContent = fav ? '已收藏' : '收藏';
  };
}

function iconFor(kind) {
  const c = { dir: '#378ADD', video: '#1D9E75', image: '#BA7517', audio: '#7F77DD', pdf: '#C0392B', archive: '#888780', file: '#888780' }[kind] || '#888780';
  const g = { dir: 'M3 7h6l2 2h10v10H3z', video: 'M4 5h16v14H4z M10 9l6 3-6 3z', image: 'M4 5h16v14H4z M8 11l3 3 2-2 4 4H7z', audio: 'M9 18V7l8-2v11', pdf: 'M6 3h8l4 4v14H6z M9 12h6 M9 16h6', archive: 'M4 6h16v4H4z M6 10h12v8H6z', file: 'M6 3h8l4 4v14H6z' }[kind] || 'M6 3h8l4 4v14H6z';
  return '<svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="' + c + '" stroke-width="1.6" stroke-linejoin="round"><path d="' + g + '"/></svg>';
}

// 缩略图地址。服务端按 修改时间 做缓存键，文件被替换会自动出新图。
function thumbURL(e) {
  // 带上 mtime：文件被替换（同名不同内容）时 URL 跟着变，浏览器不会一直拿旧缩略图。
  // 服务端不看这个参数，缓存键用的也是 mtime，两边一致
  return '/api/fs/thumb?' + q(S.root, e.path) + '&t=' + (e.mtime || 0);
}

function sizeText(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

// 网页端大文件提示线（不是硬上限）。口径（2026-10-30 定）：按 5MB/s、10 秒传完 ≈ 50MB。
// 2026-10-31 改口径：不再硬拦 50MB —— 上传期间会申请屏幕唤醒锁（不支持 Wake Lock 的
// 浏览器退化为静音音频保活），选超 50MB 时弹确认框说明风险并推荐 SMB/WebDAV，用户坚持就传。
// 后端 fsops.go 的 maxWebUpload 是防滥用的保险丝（10GB），与这里无关。
const WEB_HINT_SIZE = 50 * 1024 * 1024;
const WEB_HINT_TEXT = (n) => n + ' 个文件超过 ' + Math.round(WEB_HINT_SIZE / 1024 / 1024) + ' MB：网页上传请保持亮屏（已自动申请亮屏锁），熄屏/切后台可能导致中断。大文件更推荐 SMB / WebDAV（系统接管，锁屏也能传）。仍要用网页上传吗？';

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
        ? '还没有收藏。收藏文件夹：进去之后点路径行右边的「☆ 收藏」；收藏文件：点 ⋮ 里的「收藏」，或在播放页按 ★'
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
  if (ent.kind === 'video' || ent.kind === 'image' || ent.kind === 'audio' || ent.kind === 'pdf') {
    // 留 260ms 让高亮先画出来：播放器关掉时，人还得能看见自己落在哪一行
    setTimeout(() => openViewer(ent), 260);
    return;
  }
  if (isTextFile(ent.name)) {
    setTimeout(() => openTextViewer(ent, '/api/fs/download?' + q(S.root, ent.path)), 260);
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

// 「刚点开」标记：点了哪个文件，哪一行就一直挂着——播放器关掉、刷新页面、
// 翻别的目录再回来，都认得出刚才开的是哪个；点下一个文件才挪走。
// 服务端 SQLite 按账号存（跟进度同口径），不是 localStorage。
function markLastOpen(e) {
  if (!e || e.isDir) return;
  S.lastOpen = { root: S.root, path: e.path, name: e.name };
  paintLastOpen();
  api('/api/fs/lastopen', { root: S.root, path: e.path }).catch(() => {});
}
function paintLastOpen() {
  document.querySelectorAll('#list .row.last-open').forEach((x) => x.classList.remove('last-open'));
  if (!S.lastOpen || S.root !== S.lastOpen.root) return;
  document.querySelectorAll('#list .row').forEach((x) => {
    if (x.dataset.name === S.lastOpen.name && x.dataset.name) x.classList.add('last-open');
  });
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

// 排序状态只有一个按钮，当前状态直接写在按钮文字里（手机端只显示图标，靠 title + toast 交代）
function sortLabel() {
  return (S.sort.by === 'name' ? '名称' : '时间') + (S.sort.dir[S.sort.by] === 'asc' ? ' ↑' : ' ↓');
}
function renderSort() {
  const txt = $('sortTxt');
  if (txt) txt.textContent = sortLabel();
  const btn = $('btnSort');
  if (btn) btn.title = '排序：' + sortLabel();
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
  // 「上一级」永远占一行：根目录时置灰不可点。不占位的话，进根目录这行消失、
  // 退出来又出现，整页列表上下跳，手还停在原位就容易点错行
  const up = document.createElement('div');
  up.className = 'row' + (S.path ? '' : ' disabled');
  up.innerHTML = '<div class="ico">' + iconFor('dir') + '</div><div class="main"><div class="name">.. 上一级</div></div>';
  if (S.path) {
    up.addEventListener('click', () => {
      S.path = S.path.split('/').slice(0, -1).join('/');
      loadList();
    });
  }
  box.appendChild(up);
  if (!S.entries.length) {
    // 注意：必须用 appendChild，不能用 box.innerHTML += —— 后者会把整个列表重新
    // 序列化再解析，把上面 up 行直接绑在 DOM 上的 click 监听一起冲掉
    // （空文件夹时上一级点了没反应就是这个原因）。
    const empty = document.createElement('div');
    empty.className = 'empty empty-dir';
    // 空文件夹是唯一「没有内容可挡」的地方 —— 字标放这儿不抢戏，也让空态看起来是设计过的，
    // 不是没加载出来。顶栏和整屏水印都不做（理由见 style.css 里那段注释）。
    empty.innerHTML =
      '<div class="brandmark">Self<b>NAS</b></div>' +
      '<div class="hint">这个文件夹是空的<br>把文件拷进这个目录，或者点上传按钮</div>';
    box.appendChild(empty);
    return;
  }
  sortEntries(S.entries).forEach((e) => {
    const row = document.createElement('div');
    row.className = 'row';
    row.dataset.name = e.name;   // 「最近/收藏」跳过来靠它定位到具体那一行
    if (e.kind === 'audio') row.dataset.audio = '1';
    // 音频优先显示标签里的歌名；标签还没到（或没有标签）就先显示文件名，
    // 拿到之后由 applyTags 就地替换，列表不会重绘、滚动位置不会跳
    const tg = tagOf(e);
    const label = (tg && tg.title) || e.name;
    const metaText = (tg && tg.artist ? tg.artist + ' · ' : '') +
      (e.isDir ? '文件夹' : sizeText(e.size)) + ' · ' + dateText(e.mtime);
    // 「刚点开」标记：服务端记着账号最后一次点开的文件，建行时复原
    if (S.lastOpen && !e.isDir && S.root === S.lastOpen.root &&
        e.name === S.lastOpen.name && e.path === S.lastOpen.path) {
      row.classList.add('last-open');
    }
    // 图片/视频（有 ffmpeg 时）出缩略图；加载失败退回图标（HEIC 之类解不了的格式）
    const vis = e.thumb
      ? '<img class="thumb" loading="lazy" alt="" src="' + thumbURL(e) + '">'
      : iconFor(e.kind);
    const fav = isFav(S.root, e.path);
    row.innerHTML =
      '<div class="ico' + (e.thumb ? ' wide' : '') + '">' + vis + '</div>' +
      '<div class="main"><div class="name-line">' +
      '<span class="name" title="' + esc(e.name) + '">' + esc(label) + '</span>' +
      // 歌手挂在歌名后面：桌面端的 .meta 整行是隐藏的（信息在独立列里），
      // 不放这儿的话桌面上根本看不到歌手；手机端靠 .meta 显示，这个 span 会隐藏
      (tg && tg.artist ? '<span class="row-sub">' + esc(tg.artist) + '</span>' : '') +
      // 收藏星是可点的真按钮：已收藏=金色实心常显；未收藏=灰色空心，桌面端悬停到行上才浮现
      //（手机端不显示，免得滑动时误触收藏）。紧跟在文件名后面，不占右侧列、不影响列对齐
      '<button class="row-star' + (fav ? '' : ' off') + '" title="' + (fav ? '已收藏，点一下取消' : '收藏') + '">' +
      (fav ? '★' : '☆') + '</button>' +
      '</div>' +
      '<div class="meta">' + esc(metaText) + '</div></div>' +
      // col-size / col-time 只在桌面端显示（≥768px），手机端 display:none ——
      // 同一份 DOM，两种布局各取所需，不用维护两套渲染
      '<div class="col-size">' + (e.isDir ? '—' : sizeText(e.size)) + '</div>' +
      '<div class="col-time">' + dateText(e.mtime) + '</div>' +
      '<button class="more">⋮</button>';
    if (e.thumb) {
      row.querySelector('img.thumb').addEventListener('error', function () {
        this.parentElement.innerHTML = iconFor(e.kind);
      });
    }
    row.addEventListener('click', (ev) => {
      if (ev.target.closest('.more') || ev.target.closest('.row-star')) return;
      openEntry(e);
    });
    row.querySelector('.more').addEventListener('click', (ev) => { ev.stopPropagation(); openSheet(e); });
    // 行内收藏星：点一下就地收藏/取消收藏，不用进 ⋮ 菜单翻
    const rs = row.querySelector('.row-star');
    rs.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      const on = await toggleFav(S.root, e.path);
      if (on === null) return;
      rs.classList.toggle('off', !on);
      rs.textContent = on ? '★' : '☆';
      rs.title = on ? '已收藏，点一下取消' : '收藏';
    });
    // 桌面端习惯：右键就是「对这个文件能做什么」。手机上没有右键，所以不影响手机端
    row.addEventListener('contextmenu', (ev) => {
      ev.preventDefault();
      openSheet(e);
    });
    box.appendChild(row);
  });

  // 列表末尾的落款：滚到底才看得到，不占任何固定高度，也不跟文件名抢注意力
  const sign = document.createElement('div');
  sign.className = 'fs-sign';
  sign.innerHTML = '<span class="brandmark">Self<b>NAS</b></span><span>数据都在你自己机器上</span>';
  box.appendChild(sign);
}

function openEntry(e) {
  if (e.isDir) {
    S.path = e.path;
    loadList();
    return;
  }
  // 下载类的点击也标一下（看/听/文本在各自预览里标，这里补其余类型）
  if (e.kind === 'video' || e.kind === 'image' || e.kind === 'audio' || e.kind === 'pdf') {
    openViewer(e);
    return;
  }
  const dl = '/api/fs/download?' + q(S.root, e.path);
  if (isTextFile(e.name)) {
    openTextViewer(e, dl);
    return;
  }
  markLastOpen(e);
  if (confirm('下载「' + e.name + '」到本机？')) {
    location.href = dl;
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

// 复制文本：局域网访问是 http（非 secure context），navigator.clipboard 在那种环境下
// 根本不存在，execCommand 兜底才是主路径。textarea 必须 opacity:0 但保持可选中布局
// （不能 clip:rect 隐藏——选区宽 0 会复制到空串还返回 true，假成功）。
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(text); return true; } catch (e) {}
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;left:0;top:0;width:1px;height:1px;opacity:0;';
    document.body.appendChild(ta);
    ta.focus(); ta.select(); ta.setSelectionRange(0, text.length);
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  } catch (e) { return false; }
}

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
  clog('生成播放器直链 ' + e.name + ' ttl=' + r.data.ttl);
  const copied = await copyText(url);
  // iOS 上再试一把唤起 VLC：走 iframe 携带 x-callback——装了 VLC 会直接打开并开播；
  // 没装则静默失败。不能用 location.href：没装时 Safari 会弹「打不开该地址」的报错页，
  // 用户看到的就是「浏览器报错、链接打不开」。
  if (/iPhone|iPad|iPod/.test(navigator.userAgent)) {
    try {
      const f = document.createElement('iframe');
      f.style.cssText = 'position:fixed;left:-9999px;top:0;width:1px;height:1px;border:0;';
      f.src = 'vlc-x-callback://x-callback-url/stream?url=' + encodeURIComponent(url);
      document.body.appendChild(f);
      setTimeout(() => f.remove(), 3000);
    } catch (err) {}
    toast(copied ? '链接已复制：装了 VLC 会自动唤起，没装就粘到播放器的「打开 URL」' : '自动复制被浏览器拦了，长按链接手动复制');
    return;
  }
  toast(copied ? '直链已复制，粘到播放器的「打开网络串流」' : '请手动复制链接');
}

function openViewer(e) {
  viewerEntry = e;
  noteRecent(e);            // 打开就算"看过"，记一笔（服务端落库，跟账号绑定）
  markLastOpen(e);          // 同时把「刚点开」标记落库 + 挂到列表那一行
  syncViewerFav();
  $('viewerName').textContent = e.name;
  $('viewerRot').style.visibility = '';   // 文本预览会隐藏它，回到图片/视频得还原
  const body = $('viewerBody');
  const foot = $('viewerFoot');
  foot.innerHTML = '';
  const dl = '/api/fs/download?' + q(S.root, e.path);
  // 防御性复位：fs 是沉浸全屏态（会把顶栏和底部条一起藏掉）。
  // 万一有哪条路径加上去没退干净，新打开的预览就会「顶栏按钮消失」，这里一次性清掉。
  $('viewer').classList.remove('fs');
  $('viewer').classList.add('on');
  if (e.kind === 'image') {
    body.innerHTML = '<img src="' + dl + '&inline=1" alt="">';
  } else if (e.kind === 'audio') {
    mountAudio(e);
  } else if (e.kind === 'pdf') {
    // 桌面浏览器自带 PDF 查看器，iframe 内嵌就能翻页缩放；
    // iOS Safari 的 iframe 只显示第一页还滚不动，所以手机上要靠「新页面打开」
    // 让系统原生查看器接管（可滚动、可双指缩放）。
    body.innerHTML = '<iframe class="pdfview" src="' + dl + '&inline=1"></iframe>';
    foot.innerHTML = '<button class="btn" id="pdfTab">在新页面打开（手机端推荐）</button>';
    $('pdfTab').addEventListener('click', () => window.open(dl + '&inline=1', '_blank'));
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

// ---------- 音乐播放器 ----------
// 音频不再是一个裸 <audio>：点一首就把当前目录里的歌排成队列，播完自动接下一首，
// 同时把歌名 / 歌手喂给 MediaSession —— 桌面浏览器的标签页和系统媒体浮层、
// 手机锁屏，认的都是这一份元数据。
//
// 队列**只到当前目录**为止，不跨目录、不循环、不随机：循环和随机属于后面的
// 播放页，阶段一先把「点开一首能顺着听下去」这件事做好。
// seq 是「播放顺序」：顺序模式就是 0,1,2…，随机模式是打乱后的索引表。
// list 永远保持目录原顺序（队列列表照它显示），seq 只决定「下一首是谁」——
// 两件事分开，切随机时列表不会跟着乱跳。
const MP = {
  on: false, root: '', dir: '', list: [], i: -1, el: null, seeking: false,
  shuffle: false, seq: [], pos: 0, played: {},
  // 歌词：lyric 是解析好的 [{t, s}]，t<0 表示整份没时间轴；lyIdx 是当前高亮行
  lyric: [], lyIdx: -1, lyricTried: false, lyricToken: 0, qOpen: false,
  // 循环模式：0 顺序 / 1 列表循环（走完绕回开头）/ 2 单曲循环（这首放完重放这首）。
  // 循环作用在 MP.seq 上，随机模式下绕回的是洗牌后的第一首，不重新洗牌
  repeat: 0,
};

// 播放器图标：走 SVG 不用字符 —— ⏮ / ⏭ 这种符号在不同系统上字形差很多，
// 有的渲染成 emoji 彩色块，跟界面完全不搭
const MP_ICON = {
  play: '<svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><path d="M8.2 5.4v13.2L19 12z"/></svg>',
  pause: '<svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><rect x="6.8" y="5" width="3.8" height="14" rx="1.3"/><rect x="13.4" y="5" width="3.8" height="14" rx="1.3"/></svg>',
  prev: '<svg viewBox="0 0 24 24" width="20" height="20" fill="currentColor"><path d="M7 5h2.1v14H7zM19 5.8v12.4L9.9 12z"/></svg>',
  next: '<svg viewBox="0 0 24 24" width="20" height="20" fill="currentColor"><path d="M14.9 5H17v14h-2.1zM5 5.8v12.4L14.1 12z"/></svg>',
  shuffle: '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 3h5v5"/><path d="M4 20 21 3"/><path d="M21 16v5h-5"/><path d="M15 15l6 6"/><path d="M4 4l5 5"/></svg>',
  loopOne: '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 2l4 4-4 4"/><path d="M3 11v-1a4 4 0 0 1 4-4h14"/><path d="M7 22l-4-4 4-4"/><path d="M21 13v1a4 4 0 0 1-4 4H3"/><path d="M11.6 10.1 13.2 9.2v5.6"/></svg>',
  loop: '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 2l4 4-4 4"/><path d="M3 11v-1a4 4 0 0 1 4-4h14"/><path d="M7 22l-4-4 4-4"/><path d="M21 13v1a4 4 0 0 1-4 4H3"/></svg>',
  list: '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 7h11M4 12h11M4 17h7"/><path d="M18.5 10.5v7.2"/><circle cx="17" cy="18" r="1.8" fill="currentColor" stroke="none"/></svg>',
  note: '<svg viewBox="0 0 24 24" width="34" height="34" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M9 18V6.6l10-2v10.8"/><circle cx="6.5" cy="18" r="2.5"/><circle cx="16.5" cy="15.4" r="2.5"/></svg>',
};

function fmtTime(s) {
  if (!isFinite(s) || s < 0) return '0:00';
  s = Math.floor(s);
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

function mpLabel(e) {
  const t = tagOf(e);
  const L = {
    title: (t && t.title) || e.name,
    artist: (t && t.artist) || '',
    album: (t && t.album) || '',
  };
  // 没标签时把扩展名去掉当标题，比「夜间飞行.mp3」清爽
  if (!t || !t.title) L.title = e.name.replace(/\.[^.]+$/, '');
  return L;
}

function mountAudio(e) {
  const list = sortEntries(S.entries).filter((x) => x.kind === 'audio');
  MP.on = true;
  MP.root = S.root;
  MP.dir = S.path;
  MP.list = list;
  MP.i = Math.max(0, list.findIndex((x) => x.path === e.path));
  MP.seq = list.map((_, i) => i);   // 默认顺序播放
  MP.pos = MP.i;
  MP.shuffle = false;
  // 随机/循环是「听歌习惯」，记在浏览器本地 —— 每次打开都回到关着的状态很烦人。
  // 恢复随机要重洗序列（当前曲固定第一），逻辑跟点按钮完全一致
  MP.repeat = Math.min(2, Math.max(0, parseInt(localStorage.getItem('mp_repeat'), 10) || 0));
  if (localStorage.getItem('mp_shuffle') === '1') { MP.shuffle = true; mpShuffleSeq(); }
  MP.played = {};   // 新的一次播放会话：这本账只在「这次打开播放器」期间有效
  MP.lyric = [];
  MP.lyIdx = -1;
  MP.lyricTried = false;
  MP.lyricToken = 0;
  MP.qOpen = false;

  // 版面照网易云的播放页来：
  //   上  —— 左边一张转着的黑胶，右边歌词逐行高亮；
  //   下  —— 一条常驻播放条（进度 / 三键 / 歌名），队列收进右侧抽屉。
  // 队列不再常年占着中间：一个目录几十首歌时它会把「正在听什么」挤没，
  // 而想翻下一首听什么，点一下列表按钮就够。
  $('viewerBody').innerHTML =
    '<div class="mp" id="mpRoot">' +
      '<div class="mp-bg"><div class="mp-bg-img" id="mpBgImg"></div></div>' +
      '<div class="mp-stage">' +
        '<div class="mp-disc">' +
          '<div class="mp-disc-in">' +
            '<img class="mp-cover" id="mpCover" alt="">' +
            '<div class="mp-cover-ph">' + MP_ICON.note + '</div>' +
            '<span class="mp-hole"></span>' +
          '</div>' +
          '<div class="mp-arm"><i></i></div>' +
        '</div>' +
        '<div class="mp-lyric" id="mpLyric"><div class="mp-lyric-inner" id="mpLyricInner"></div></div>' +
      '</div>' +
      '<div class="mp-foot">' +
        // 进度条横跨整条底栏，时间贴在两端 —— 跟音乐 App 的底栏一致
        '<div class="mp-prog">' +
          '<span class="mp-t" id="mpCur">0:00</span>' +
          '<input class="mp-seek" id="mpSeek" type="range" min="0" max="1000" value="0" step="1" aria-label="播放进度">' +
          '<span class="mp-t" id="mpDur">--:--</span>' +
        '</div>' +
        '<div class="mp-ctl">' +
          '<div class="mp-now">' +
            '<div class="mp-mini"><img id="mpMini" alt=""><i>' + MP_ICON.note + '</i></div>' +
            '<div class="mp-info">' +
              '<div class="mp-title" id="mpTitle"></div>' +
              '<div class="mp-sub" id="mpSub"></div>' +
            '</div>' +
          '</div>' +
          '<div class="mp-btns">' +
            '<button class="mp-btn" id="mpPrev" title="上一首">' + MP_ICON.prev + '</button>' +
            '<button class="mp-btn primary" id="mpPlay" title="播放">' + MP_ICON.play + '</button>' +
            '<button class="mp-btn" id="mpNext" title="下一首">' + MP_ICON.next + '</button>' +
          '</div>' +
          '<div class="mp-right">' +
            '<button class="mp-mode" id="mpLoop" title="顺序播放">' + MP_ICON.loop + '</button>' +
            '<button class="mp-mode" id="mpShuffle" title="随机播放：关">' + MP_ICON.shuffle + '</button>' +
            '<button class="mp-mode" id="mpQueueBtn" title="播放列表">' + MP_ICON.list + '</button>' +
          '</div>' +
        '</div>' +
      '</div>' +
      '<aside class="mp-qpane" id="mpQpane">' +
        '<div class="mp-qhead">播放列表 <span class="mp-qn" id="mpQn"></span></div>' +
        '<div class="mp-queue" id="mpQueue"></div>' +
      '</aside>' +
      '<div class="mp-qmask" id="mpQmask"></div>' +
      // 音频元素本身不出声也不露面，只当解码器用
      '<audio id="mpAudio" preload="metadata"></audio>' +
    '</div>';

  MP.el = $('mpAudio');
  MP.seeking = false;
  $('mpPrev').addEventListener('click', () => mpStep(-1));
  $('mpNext').addEventListener('click', () => mpStep(1));
  $('mpShuffle').addEventListener('click', mpToggleShuffle);
  $('mpLoop').addEventListener('click', mpToggleLoop);
  $('mpQueueBtn').addEventListener('click', () => mpToggleQueue());
  $('mpQmask').addEventListener('click', () => mpToggleQueue(false));
  $('mpPlay').addEventListener('click', () => {
    if (!MP.el) return;
    if (MP.el.paused) MP.el.play().catch(() => {});
    else MP.el.pause();
  });
  // 播完自动接下一首：这是「连播」的全部实现，剩下交给浏览器
  MP.el.addEventListener('ended', () => {
    // 单曲循环：这首重放这首。不整走 mpPlay —— 同一首歌，标签/歌词/列表都不用动，
    // 也不用再问一次服务端进度，回零接着唱就完了
    if (MP.repeat === 2) { MP.el.currentTime = 0; MP.el.play().catch(() => {}); return; }
    if (MP.pos + 1 < MP.seq.length) { mpStep(1); return; }   // 播完自动接下一首（随机模式下就是序列里的下一首）
    if (MP.repeat === 1) {
      // 列表循环：序列走完绕回开头再来一遍 —— 随机模式下绕回的是洗牌后的第一首，
      // 不是重新洗牌（重新洗牌等于换了一个顺序，跟「循环」这个词对不上）
      mpSave(false);
      MP.pos = 0;
      MP.i = MP.seq[0];
      mpPlay();
      return;
    }
    mpSave(true);   // 最后一首放完，把进度清零，下次从头上听
  });
  MP.el.addEventListener('loadedmetadata', () => { mpRestore(); mpSeekSync(); });
  MP.el.addEventListener('timeupdate', () => {
    // 关掉播放器的瞬间元素还会甩出最后一两个 timeupdate，那时 MP.el 已经被置空 ——
    // 不挡的话控制台会冒一条 null.currentTime，看着像播放器炸了
    if (!MP.el) return;
    const t = MP.el.currentTime;
    mpTick(); mpSeekSync(); mpSyncLyric(t);
  });
  MP.el.addEventListener('play', mpPaintBtn);
  MP.el.addEventListener('pause', () => { mpPaintBtn(); mpSave(false); });
  // 拖动中只更新「已播时间」的显示，松手才真 seek —— 每动一像素就 seek 一次
  // 会把播放位置反复打断，播客/长音频上听起来就是卡顿
  $('mpSeek').addEventListener('input', () => {
    MP.seeking = true;
    const p = +$('mpSeek').value / 1000;
    $('mpSeek').style.setProperty('--p', (p * 100).toFixed(2) + '%');
    $('mpCur').textContent = fmtTime(p * (MP.el && isFinite(MP.el.duration) ? MP.el.duration : 0));
  });
  $('mpSeek').addEventListener('change', () => {
    const p = +$('mpSeek').value / 1000;
    if (MP.el && isFinite(MP.el.duration)) MP.el.currentTime = p * MP.el.duration;
    MP.seeking = false;
  });

  mpRender();
  mpPlay();
  hookMediaSession();
}

// 窗口尺寸变了，歌词居中的那点内边距要重算，否则切到横屏后整列歌词会偏上/偏下
let mpResizeTimer = 0;
window.addEventListener('resize', () => {
  if (!MP.on) return;
  clearTimeout(mpResizeTimer);
  mpResizeTimer = setTimeout(() => { mpPadLyric(); mpSyncLyric(MP.el ? MP.el.currentTime : 0, true); }, 120);
});

function mpPaintBtn() {
  const b = $('mpPlay');
  if (!b) return;
  const playing = MP.el && !MP.el.paused;
  b.innerHTML = playing ? MP_ICON.pause : MP_ICON.play;
  b.title = playing ? '暂停' : '播放';
  // 唱片转不转、唱针落不落在盘上，都只看这一个类
  const r = $('mpRoot');
  if (r) r.classList.toggle('playing', !!playing);
}

// 把当前播放位置同步到进度条与时间标签
function mpSeekSync() {
  const a = MP.el, sk = $('mpSeek');
  if (!a || !sk) return;
  const d = a.duration;
  const ok = isFinite(d) && d > 0;
  if (!MP.seeking) {
    const p = ok ? a.currentTime / d : 0;
    sk.value = String(Math.round(p * 1000));
    sk.style.setProperty('--p', (p * 100).toFixed(2) + '%');
    $('mpCur').textContent = fmtTime(a.currentTime);
  }
  $('mpDur').textContent = ok ? fmtTime(d) : '--:--';
}

// ---------- 封面 / 歌词 ----------
// 两样都走独立接口：只有真播到这一首才去拉，列表里一个字节都不带（跟 thumb 一个道理）。

function mpCoverURL(e) {
  return '/api/fs/cover?' + q(MP.root, e.path);
}

function mpTrackArt(e) {
  const url = mpCoverURL(e);
  const root = $('mpRoot'), bg = $('mpBgImg'), cov = $('mpCover'), mini = $('mpMini');
  if (!root) return;
  // 先摘掉上一首的脸，等新图 onload 了再贴 —— 否则封面慢半秒时，
  // 唱片上会挂着上一首的封面在转
  root.classList.remove('has-art');
  [bg, cov, mini].forEach((n) => n && n.classList.remove('on'));
  const probe = new Image();
  probe.onload = () => {
    if (MP.list[MP.i] !== e) return;   // 加载期间已经切歌，别把旧图贴到新歌上
    if (bg) bg.style.backgroundImage = 'url("' + url + '")';
    [cov, mini].forEach((n) => { if (n) { n.src = url; n.classList.add('on'); } });
    if (bg) bg.classList.add('on');
    root.classList.add('has-art');
  };
  probe.onerror = () => {
    if (MP.list[MP.i] !== e) return;
    if (bg) { bg.style.backgroundImage = ''; bg.classList.remove('on'); }
    [cov, mini].forEach((n) => { if (n) { n.removeAttribute('src'); n.classList.remove('on'); } });
    root.classList.remove('has-art');
  };
  probe.src = url;
}

async function mpLoadLyric(e) {
  const token = ++MP.lyricToken;
  MP.lyric = [];
  MP.lyIdx = -1;
  MP.lyricTried = false;
  mpRenderLyric();
  try {
    const r = await api('/api/fs/lyrics?' + q(MP.root, e.path));
    if (token !== MP.lyricToken || MP.list[MP.i] !== e) return;   // 期间切歌了，这份歌词作废
    MP.lyricTried = true;
    const raw = (r && r.data && r.data.lrc) || '';
    MP.lyric = raw ? mpParseLRC(raw) : [];
  } catch (err) {
    if (token !== MP.lyricToken) return;
    MP.lyricTried = true;
    MP.lyric = [];
  }
  mpRenderLyric();
}

// LRC 解析。行首可以连着挂好几个时间标签（副歌那句被复用的情况），
// 一个标签产出一行 —— 到点了各自高亮。
function mpParseLRC(text) {
  const out = [];
  const plain = [];
  const re = /\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]/g;
  const isMeta = /^\[(ti|ar|al|by|offset|length|re|ve|tool|encoding|kana)\s*:/i;
  String(text).replace(/^\uFEFF/, '').split(/\r?\n/).forEach((raw) => {
    const line = raw.trim();
    if (!line) return;
    const times = [];
    let end = 0, m;
    re.lastIndex = 0;
    while ((m = re.exec(line))) {
      if (m.index !== end) break;   // 时间标签必须顶格连着出现，句中的 [..] 不算
      const frac = m[3] || '';
      const ms = frac ? +(frac + '00').slice(0, 3) : 0;
      times.push(+m[1] * 60 + +m[2] + ms / 1000);
      end = re.lastIndex;
    }
    const s = line.slice(end).trim();
    if (!times.length) {
      // 整个文件都没时间标签的（内嵌 USLT 大多如此）也别丢，退化成静态文字
      if (!isMeta.test(line)) plain.push(line);
      return;
    }
    if (!s) return;
    times.forEach((t) => out.push({ t, s }));
  });
  if (!out.length) return plain.map((s) => ({ t: -1, s }));
  out.sort((a, b) => a.t - b.t);
  return out;
}

function mpRenderLyric() {
  const box = $('mpLyric'), inner = $('mpLyricInner');
  if (!box || !inner) return;
  const e = MP.list[MP.i];
  inner.innerHTML = '';
  inner.style.paddingTop = '';
  inner.style.paddingBottom = '';
  if (!e || !MP.lyric.length) {
    box.classList.add('empty');
    box.scrollTop = 0;
    if (e) {
      const L = mpLabel(e);
      const sub = [L.artist, L.album].filter(Boolean).join(' · ');
      inner.innerHTML =
        '<div class="mp-empty">' +
          '<div class="mp-empty-t">' + esc(L.title) + '</div>' +
          (sub ? '<div class="mp-empty-s">' + esc(sub) + '</div>' : '') +
          '<div class="mp-empty-h">' + (MP.lyricTried ? '暂无歌词' : '正在找歌词…') + '</div>' +
        '</div>';
    }
    return;
  }
  box.classList.remove('empty');
  const frag = document.createDocumentFragment();
  MP.lyric.forEach((ln) => {
    const d = document.createElement('div');
    d.className = 'mp-ly' + (ln.t < 0 ? ' plain' : '');
    d.textContent = ln.s;
    if (ln.t >= 0) {
      // 点哪句从哪句开始唱 —— 长前奏 / 找某一段时比拖进度条准
      d.addEventListener('click', () => { if (MP.el) MP.el.currentTime = ln.t; });
    }
    frag.appendChild(d);
  });
  inner.appendChild(frag);
  MP.lyIdx = -1;
  mpPadLyric();
  mpSyncLyric(MP.el ? MP.el.currentTime : 0, true);
}

// 上下各留半屏内边距，第一句和最后一句也能停在正中（不然滚动到头就卡在底部）
function mpPadLyric() {
  const box = $('mpLyric'), inner = $('mpLyricInner');
  if (!box || !inner) return;
  if (!MP.lyric.length || box.classList.contains('empty')) return;
  const pad = Math.max(10, Math.round(box.clientHeight / 2 - 24));
  inner.style.paddingTop = pad + 'px';
  inner.style.paddingBottom = pad + 'px';
}

function mpSyncLyric(t, force) {
  if (!MP.lyric.length || !MP.lyric[0] || MP.lyric[0].t < 0) return;   // 无时间轴，不跟着走
  let idx = -1;
  for (let i = 0; i < MP.lyric.length; i++) {
    if (MP.lyric[i].t > t + 0.2) break;
    idx = i;
  }
  if (idx === MP.lyIdx && !force) return;
  const box = $('mpLyric'), inner = $('mpLyricInner');
  if (!box || !inner) return;
  const all = inner.children;
  if (MP.lyIdx >= 0 && all[MP.lyIdx]) all[MP.lyIdx].classList.remove('on');
  MP.lyIdx = idx;
  if (idx < 0) { box.scrollTo({ top: 0, behavior: force ? 'auto' : 'smooth' }); return; }
  const el = all[idx];
  if (!el) return;
  el.classList.add('on');
  const top = el.offsetTop - (box.clientHeight - el.offsetHeight) / 2;
  box.scrollTo({ top: Math.max(0, top), behavior: force ? 'auto' : 'smooth' });
}

function mpToggleQueue(on) {
  MP.qOpen = on === undefined ? !MP.qOpen : !!on;
  const r = $('mpRoot');
  if (r) r.classList.toggle('q-open', MP.qOpen);
  if (!MP.qOpen) return;
  const q = $('mpQueue');
  const cur = q && q.querySelector('.mp-item.on');
  if (cur && q) q.scrollTop = Math.max(0, cur.offsetTop - q.clientHeight / 2);
}

function mpPlay() {
  const e = MP.list[MP.i];
  if (!e || !MP.el) return;
  // 收藏星、「刚点开」、进度条都跟着当前这一首走 —— viewerEntry 是它们的唯一来源
  viewerEntry = e;
  const L = mpLabel(e);
  $('viewerName').textContent = L.title;
  syncViewerFav();
  // 只有「这次打开播放器后第一次放到这首」才允许续播。
  // 听到一半切走、又切回来 —— 那是重新点开一首歌，从头上唱，
  // 不能拿服务端记着的旧位置把人拽回中间。
  MP.allowRestore = !MP.played[e.path];
  MP.played[e.path] = 1;
  MP.el.src = '/api/fs/download?' + q(MP.root, e.path) + '&inline=1';
  // 换歌时先把进度条归零、时长清空，等 loadedmetadata 到了再填真值 ——
  // 不重置的话切歌瞬间会看到上一首的时长和新歌的进度条位置对不上
  const sk = $('mpSeek');
  if (sk) { sk.value = '0'; sk.style.setProperty('--p', '0%'); }
  if ($('mpCur')) $('mpCur').textContent = '0:00';
  if ($('mpDur')) $('mpDur').textContent = '--:--';
  const p = MP.el.play();
  if (p && p.catch) p.catch(() => {});   // 浏览器拦自动播放时不报红，用户可以点控件
  mpPaintBtn();
  mpTrackArt(e);
  mpLoadLyric(e);
  mpRender();
  mpMediaMeta(L);
}

function mpStep(d) {
  let n = MP.pos + d;
  if (d < 0 && n < 0) { if (MP.el) MP.el.currentTime = 0; return; }   // 第一首按「上一个」= 重头放
  if (d > 0 && n >= MP.seq.length) {
    if (!MP.repeat) return;   // 顺序模式，走到末尾就停
    n = 0;                  // 开了循环，手动按「下一首」也绕回开头
  }
  mpSave(false);   // 离开当前曲目前先落一次进度
  MP.pos = n;
  MP.i = MP.seq[n];
  mpPlay();
}

// 随机：把「除当前这首以外」的索引洗一遍，当前这首永远排第一 ——
// 不然一点随机，正在听的歌会被换掉，很突兀
// 洗牌：把「除当前这首以外」的索引洗一遍，当前这首永远排第一 ——
// 不然一点随机，正在听的歌会被换掉，很突兀
function mpShuffleSeq() {
  const rest = MP.seq.filter((i) => i !== MP.i);
  for (let i = rest.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [rest[i], rest[j]] = [rest[j], rest[i]];
  }
  MP.seq = [MP.i, ...rest];
  MP.pos = 0;
}

function mpToggleShuffle() {
  MP.shuffle = !MP.shuffle;
  if (MP.shuffle) {
    mpShuffleSeq();
  } else {
    // 关掉随机：回到目录原顺序，并定位到当前正在听的这一首，
    // 不能简单回到 0，否则下一首会跑到别的歌手上
    MP.seq = MP.list.map((_, i) => i);
    MP.pos = MP.i;
  }
  localStorage.setItem('mp_shuffle', MP.shuffle ? '1' : '0');
  mpRender();
  toast(MP.shuffle ? '随机播放' : '顺序播放');
}

// 循环按钮：顺序 → 列表循环 → 单曲循环 → 顺序，三态轮换（跟网易云一个路子）
const MP_REPEAT_NAME = ['顺序播放', '列表循环', '单曲循环'];
function mpToggleLoop() {
  MP.repeat = (MP.repeat + 1) % 3;
  localStorage.setItem('mp_repeat', String(MP.repeat));
  mpRender();   // 图标要换成带 1 的那个，置灰状态也跟着变
  toast(MP_REPEAT_NAME[MP.repeat]);
}

let mpLastSave = 0;
function mpTick() {
  const now = Date.now();
  if (now - mpLastSave < 10000) return;   // 音乐动辄三五分钟，10 秒一次足够
  mpLastSave = now;
  mpSave(false);
}

function mpSave(reset) {
  const e = MP.list[MP.i];
  if (!e || !MP.el) return;
  if (reset) {
    api('/api/fs/progress', { root: MP.root, path: e.path, pos: 0, dur: 0 }).catch(() => {});
    return;
  }
  const d = MP.el.duration;
  const pos = MP.el.currentTime;
  // 一分钟以内的不记（跟视频那套判据一致），免得列表里全是「上次听到 0:12」
  if (!isFinite(d) || d < 60 || !isFinite(pos) || pos <= 0) return;
  api('/api/fs/progress', {
    root: MP.root, path: e.path,
    pos: Math.floor(pos), dur: Math.floor(d),
  }).catch(() => {});
}

async function mpRestore() {
  // 同一次会话里切回来的歌，从头放 —— 用户的原话是「切歌了进度就不存在，
  // 只有退出去再回来那种才该接着放」
  if (!MP.allowRestore) return;
  const e = MP.list[MP.i];
  if (!e || !MP.el || !isFinite(MP.el.duration) || MP.el.duration < 60) return;
  try {
    const r = await api('/api/fs/progress?' + q(MP.root, e.path));
    // 拉回来的时候可能已经切歌了，别把 A 的位置跳到 B 上
    if (r.code !== 0 || !r.data || MP.list[MP.i] !== e) return;
    const pos = +r.data.pos || 0;
    // 结尾 15 秒内不续播：基本等于听完了，再跳到尾巴上没意义
    if (pos > 20 && pos < MP.el.duration - 15) {
      MP.el.currentTime = pos;
      mpSyncLyric(pos, true);
      toast('从上次听到的地方继续');
    }
  } catch (err) { /* 拉不到就从头放，对用户无感 */ }
}

function mpRender() {
  if (!MP.on) return;
  const e = MP.list[MP.i];
  if (!e) return;
  const L = mpLabel(e);
  const tEl = $('mpTitle');
  if (tEl) {
    tEl.textContent = L.title;
    tEl.title = e.name;
    $('mpSub').textContent = [L.artist, L.album].filter(Boolean).join(' · ') || '未知歌手';
  }
  const q = $('mpQueue');
  if (q) {
    // 只在「条目数变了」时重建：切歌只挪高亮。
    // 每次都重建的话，抽屉里刚滚到的位置会被打回顶部，翻到一半就跳走。
    if (q.childElementCount !== MP.list.length) {
      q.innerHTML = '';
      MP.list.forEach((x, i) => {
        const it = document.createElement('div');
        it.className = 'mp-item';
        const t = mpLabel(x);
        // 序号和「跳动的小竖条」都常驻，用 CSS 按 .on 决定露哪一个 —— 这样切歌只改类名
        it.innerHTML =
          '<span class="mp-i"><b class="mp-eq"><i></i><i></i><i></i></b><em>' + (i + 1) + '</em></span>' +
          '<span class="mp-n">' + esc(t.title) + '</span>' +
          '<span class="mp-a">' + esc(t.artist) + '</span>';
        it.title = x.name;
        it.addEventListener('click', () => {
          if (i === MP.i) return;
          mpSave(false);
          MP.i = i;
          MP.pos = MP.seq.indexOf(i);   // 随机模式下点列表也要把播放序列的位置对齐
          mpPlay();
        });
        q.appendChild(it);
      });
    }
    [...q.children].forEach((it, i) => it.classList.toggle('on', i === MP.i));
    if (MP.qOpen) {
      const cur = q.querySelector('.mp-item.on');
      if (cur) q.scrollTop = Math.max(0, cur.offsetTop - q.clientHeight / 2);
    }
  }
  const qn = $('mpQn');
  if (qn) qn.textContent = '(' + MP.list.length + ')';
  // 到头就置灰：让人知道「已经是最前/最后」，而不是点了没反应。
  // 随机模式下按播放序列算边界，不是按列表顺序
  const pv = $('mpPrev'), nx = $('mpNext');
  // 循环（列表或单曲）开着时首/尾都能绕过去，按钮永远可点
  if (pv) pv.disabled = MP.pos <= 0 && !MP.repeat;
  if (nx) nx.disabled = MP.pos >= MP.seq.length - 1 && !MP.repeat;
  const sh = $('mpShuffle');
  if (sh) {
    sh.classList.toggle('on', MP.shuffle);
    sh.title = MP.shuffle ? '随机播放：开' : '随机播放：关';
  }
  const lp = $('mpLoop');
  if (lp) {
    lp.classList.toggle('on', MP.repeat > 0);
    lp.innerHTML = MP.repeat === 2 ? MP_ICON.loopOne : MP_ICON.loop;
    lp.title = MP_REPEAT_NAME[MP.repeat];
  }
}

// MediaSession：锁屏 / 系统媒体浮层 / 键盘媒体键。桌面也有用 ——
// Chrome 的标签页会显示歌名，Windows 音量浮层能直接暂停切歌。
let mpMsHooked = false;
function mpMediaMeta(L) {
  if (!('mediaSession' in navigator) || !window.MediaMetadata) return;
  try {
    navigator.mediaSession.metadata = new MediaMetadata({
      title: L.title, artist: L.artist, album: L.album,
    });
  } catch (err) { /* 老浏览器没有 MediaMetadata，忽略 */ }
}

function hookMediaSession() {
  if (mpMsHooked || !('mediaSession' in navigator)) return;
  mpMsHooked = true;
  const set = (a, fn) => { try { navigator.mediaSession.setActionHandler(a, fn); } catch (e) {} };
  set('play', () => { if (MP.el) MP.el.play().catch(() => {}); });
  set('pause', () => { if (MP.el) MP.el.pause(); });
  set('previoustrack', () => mpStep(-1));
  set('nexttrack', () => mpStep(1));
  set('seekbackward', (d) => { if (MP.el) MP.el.currentTime = Math.max(0, MP.el.currentTime - (d.seekOffset || 10)); });
  set('seekforward', (d) => { if (MP.el) MP.el.currentTime = Math.min(MP.el.duration || 0, MP.el.currentTime + (d.seekOffset || 10)); });
  set('seekto', (d) => { if (MP.el && d.seekTime != null) MP.el.currentTime = d.seekTime; });
}

// ---------- 文本预览 ----------
//
// txt / markdown / 代码 / 配置这类文件，点了直接在网页里看——手机上查个笔记、
// 看眼日志、翻翻配置，不用下载。只读不改：在外面改文件既容易误触保存覆盖原文件，
// 也没有在电脑上改顺手，需要改就下载或回到电脑上。
const TEXT_EXT = /\.(txt|md|markdown|log|json|yaml|yml|ini|conf|cfg|toml|env|properties|bat|cmd|sh|ps1|py|go|js|ts|css|html|htm|xml|csv|sql|gitignore|java|c|cpp|h|hpp|rs|php|rb|vue|jsx|tsx)$/i;
const TEXT_MAX = 5 * 1024 * 1024;   // 超过 5MB 不读进内存（手机上渲染大 DOM 会卡）；中篇 txt 小说够用

function isTextFile(name) {
  return TEXT_EXT.test(name) || !/\.[a-z0-9]{1,8}$/i.test(name);   // 无扩展名（README、Makefile）也试一把，二进制检测会拦住
}

// 够用的 Markdown 渲染：标题/列表/引用/代码块/粗斜体/行内代码/链接/分隔线。
// 先整体转义再打标记，原文里的 <script> 之类不会被当 HTML 执行
// Markdown → HTML。先整体 esc 再打标记（XSS 安全），只做白名单内的转换。
// resolveImg(src)：把图片地址换算成可访问的 URL —— http(s) 外链原样；
// 相对路径按 md 所在目录拼 /api/fs/download；「F:\…」这类带盘符的绝对路径落到对应根目录。
function renderMarkdown(src, resolveImg) {
  // ── 原生 HTML 白名单直通 ──
  // GitHub 风格的 README 大量用 <img>/<table>/<td width>/<sub>/<b>/<br> 排版，
  // 全部 esc 会变成一堆代码文本（用户看到的"图片是 HTML 代码"就是这个）。
  // 做法：先把白名单标签摘出来换成占位符，其余文本照旧 esc，最后原样还原。
  // 占位符用 \x00 包裹，esc 只转义 &<>"'，不会碰到它。
  // 白名单之外的标签（script/iframe/onclick 属性…）不占位，依旧被 esc 成代码显示。
  const store = [];
  const stash = (html, kind) => '\x00' + kind + (store.push(html) - 1) + '\x00';
  // 结构标签重新生成，只保留 td/th 的 width/align，onclick/style 之类全部剥掉
  const structural = (tag) => {
    const open = tag.match(/^<(td|th)\b([^>]*)>/i);
    if (open) {
      let attrs = '';
      const w = (open[2].match(/width\s*=\s*["']([^"']*)["']/i) || [])[1];
      const al = (open[2].match(/align\s*=\s*["']([^"']*)["']/i) || [])[1];
      if (w) attrs += ' width="' + esc(w) + '"';
      if (al) attrs += ' align="' + esc(al) + '"';
      return '<' + open[1].toLowerCase() + attrs + '>';
    }
    const m = tag.match(/^<(\/?)([a-zA-Z0-9]+)/);
    return '<' + (m[1] || '') + m[2].toLowerCase() + '>';
  };
  const attrVal = (attrs, name) => {
    const m = attrs.match(new RegExp(name + '\\s*=\\s*"([^"]*)"', 'i'))
      || attrs.match(new RegExp(name + "\\s*=\\s*'([^']*)'", 'i'))
      || attrs.match(new RegExp(name + '\\s*=\\s*([^\\s>]+)', 'i'));
    return m ? m[1] : '';
  };
  src = String(src)
    // <img>：重新拼一个干净的 img，src 走图片解析器；解析不了就不占位（留在原文里被 esc 显示）
    .replace(/<img\b([^>]*)>/gi, (all, attrs) => {
      const rawSrc = attrVal(attrs, 'src');
      const url = rawSrc && resolveImg ? resolveImg(rawSrc) : '';
      if (!url) return all;
      return stash('<img class="md-img" loading="lazy" src="' + url + '" alt="' + esc(attrVal(attrs, 'alt')) + '">', 'I');
    })
    .replace(/<(br|hr)\b[^>]*\/?>/gi, (t) => stash(structural(t), 'I'))
    .replace(/<(\/?)(table|thead|tbody|tfoot|tr|td|th|sub|sup|b|strong|i|em|u|center|kbd)\b[^>]*>/gi, (t) => {
      const block = /^<(\/?)(table|thead|tbody|tfoot|tr|td|th)\b/i.test(t);
      return stash(structural(t), block ? 'B' : 'I');
    });

  const lines = esc(src).split(/\r?\n/);
  // 链接 href 只放行 http(s)/站内/锚点，javascript: 之类直接降级成纯文本
  const safeHref = (u) => (/^(https?:|\/|#)/i.test(u) ? u : null);
  // 地址里允许一层成对括号（wiki 链接常见 .../foo_(bar)），否则 javascript:alert(1) 会漏出半个括号
  const TARGET = '((?:[^()\\s]|\\([^()\\s]*\\))+)';
  const imgRe = new RegExp('!\\[([^\\]]*)\\]\\(' + TARGET + '\\)', 'g');
  const linkRe = new RegExp('\\[([^\\]]+)\\]\\(' + TARGET + '\\)', 'g');
  const inline = (s) => s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    // 图片要先于链接处理，否则 ![alt](src) 会被链接规则吃掉只留下 "!"
    .replace(imgRe, (all, alt, src) => {
      const url = resolveImg ? resolveImg(src) : '';
      return url ? '<img class="md-img" loading="lazy" alt="' + alt + '" src="' + url + '">' : all;
    })
    .replace(linkRe, (all, text, href) => {
      const u = safeHref(href);
      return u ? '<a href="' + u + '" target="_blank" rel="noopener">' + text + '</a>' : text;
    })
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\*([^*]+)\*/g, '<em>$1</em>');
  // GFM 竖线表格：| a | b | 分隔行 | --- | --- |，整块转 <table>
  const tableCells = (row) => row.trim().replace(/^\|/, '').replace(/\|$/, '')
    .split('|').map((c) => inline(c.trim()));
  const isSep = (l) => /^\s*\|?[\s:|-]+\|?\s*$/.test(l) && /-/.test(l) && /\|/.test(l);

  // 一段行数组 → HTML。抽成函数是为了让引用块能递归：连续的 &gt; 行剥掉前缀后当普通文档再跑一遍，
  // 这样「> 里的代码块/列表」才能正常渲染（原来每行各包一个 <blockquote>，里面的 ``` 会原样露出来）
  const render = (arr) => {
    const out = [];
    let list = null, code = null;
    const closeList = () => { if (list) { out.push('</' + list + '>'); list = null; } };
    for (let i = 0; i < arr.length; i++) {
      const raw = arr[i];
      const t = raw.trim();
      // 原生 HTML 的行（整行都是结构占位符，可能夹杂图片占位符）：原样输出、不包 <p>，
      // 否则表格结构被段落拆散，浏览器渲染出来的版式会乱
      if (/^\x00B\d+\x00/.test(t) && !t.replace(/\x00[BI]\d+\x00/g, '').trim()) {
        closeList();
        out.push(raw);
        continue;
      }
      if (/^```/.test(t)) {
        if (code !== null) { out.push('<pre class="md-code">' + code.join('\n') + '</pre>'); code = null; }
        else { closeList(); code = []; }
        continue;
      }
      if (code !== null) { code.push(raw); continue; }
      if (!t) { closeList(); continue; }
      let m;
      if ((m = t.match(/^(#{1,6})\s+(.*)$/))) { closeList(); const lv = m[1].length; out.push('<h' + lv + '>' + inline(m[2]) + '</h' + lv + '>'); continue; }
      if (/^(-{3,}|\*{3,})$/.test(t)) { closeList(); out.push('<hr>'); continue; }
      // 引用块：把连续的 &gt; 行整段收进来，剥掉前缀递归渲染，再包一层 <blockquote>
      if (/^&gt;/.test(t)) {
        closeList();
        const inner = [];
        let j = i;
        while (j < arr.length && /^\s*&gt;/.test(arr[j])) {
          inner.push(arr[j].replace(/^\s*&gt;\s?/, ''));
          j++;
        }
        out.push('<blockquote>' + render(inner) + '</blockquote>');
        i = j - 1;
        continue;
      }
      if (/\|/.test(t) && i + 1 < arr.length && isSep(arr[i + 1])) {
        const head = tableCells(t);
        if (head.length) {
          const rows = [];
          let j = i + 2;
          while (j < arr.length && arr[j].trim() && /\|/.test(arr[j])) { rows.push(tableCells(arr[j])); j++; }
          closeList();
          out.push('<div class="md-tablewrap"><table><thead><tr>'
            + head.map((h) => '<th>' + h + '</th>').join('')
            + '</tr></thead><tbody>'
            + rows.map((r) => '<tr>' + r.map((c) => '<td>' + c + '</td>').join('') + '</tr>').join('')
            + '</tbody></table></div>');
          i = j - 1;
          continue;
        }
      }
      if ((m = t.match(/^[-*]\s+(.*)$/))) { if (list !== 'ul') { closeList(); out.push('<ul>'); list = 'ul'; } out.push('<li>' + inline(m[1]) + '</li>'); continue; }
      if ((m = t.match(/^\d+[.)]\s+(.*)$/))) { if (list !== 'ol') { closeList(); out.push('<ol>'); list = 'ol'; } out.push('<li>' + inline(m[1]) + '</li>'); continue; }
      closeList();
      out.push('<p>' + inline(t) + '</p>');
    }
    if (code !== null && code.length) out.push('<pre class="md-code">' + code.join('\n') + '</pre>');
    closeList();
    return out.join('');
  };

  // 还原白名单 HTML 占位符
  return render(lines).replace(/\x00([BI])(\d+)\x00/g, (m, k, i) => store[+i]);
}

// Markdown 里的图片地址 → 可访问 URL。
//   http(s) 外链                     → 原样
//   相对路径 ./x.png、img/x.png、../img/x.png → 按 md 所在目录拼 /api/fs/download
//   「F:\…」带盘符的绝对路径          → 找到对应根目录后拼 download（找不到就不渲染，保留原文）
// 都先经过 esc 了，地址里不会带引号，直接拼进 src 属性是安全的
function mdImgResolver(e) {
  return (src) => {
    let s = src.replace(/\\/g, '/');
    const dm = s.match(/^([a-zA-Z]):\/(.*)$/);
    if (dm) {
      const r = S.roots.find((x) => (x.drive || '').replace(/:/g, '').toLowerCase() === dm[1].toLowerCase());
      if (!r) return '';
      // 根目录若不是盘根（比如 F:\docs），要把这段前缀从图片路径里去掉
      const pre = (r.path || '').replace(/\\/g, '/').replace(/\/+$/, '').replace(/^[a-zA-Z]:/, '').replace(/^\/+/, '');
      let rel = dm[2];
      if (pre && rel.toLowerCase().startsWith(pre.toLowerCase() + '/')) rel = rel.slice(pre.length + 1);
      return '/api/fs/download?' + q(r.path, rel);
    }
    if (/^(https?:)?\/\//i.test(s)) return s;
    const segs = e.path.split('/').slice(0, -1).filter(Boolean);
    let p = s.replace(/^\.\//, '');
    while (/^\.\.\//.test(p)) { p = p.slice(3); segs.pop(); }
    p = p.replace(/^\/+/, '');
    return '/api/fs/download?' + q(S.root, segs.concat(p.split('/')).join('/'));
  };
}

async function openTextViewer(e, dl) {
  viewerEntry = e;
  noteRecent(e);
  markLastOpen(e);
  syncViewerFav();
  $('viewerName').textContent = e.name;
  $('viewerRot').style.visibility = 'hidden';   // 旋转是给图片用的，文本用不上
  const body = $('viewerBody');
  const foot = $('viewerFoot');
  foot.innerHTML = '<a class="btn" href="' + dl + '">下载到本机</a>';
  body.innerHTML = '<div class="viewer-msg">正在读取…</div>';
  $('viewer').classList.remove('fs');   // 同 openViewer：清掉可能残留的沉浸态
  $('viewer').classList.add('on');
  try {
    const res = await fetch(dl + '&inline=1', { credentials: 'same-origin' });
    if (!res.ok) throw new Error('读取失败（' + res.status + '）');
    const blob = await res.blob();
    if (blob.size > TEXT_MAX) {
      body.innerHTML = '<div class="viewer-msg">文件超过 5 MB，不做在线预览<br>点下面的「下载到本机」看</div>';
      return;
    }
    const buf = new Uint8Array(await blob.arrayBuffer());
    // 二进制检测：开头 8KB 出现 NUL 字节就当二进制，不硬猜编码
    let bin = false;
    for (let i = 0; i < Math.min(buf.length, 8192); i++) { if (buf[i] === 0) { bin = true; break; } }
    if (bin) {
      body.innerHTML = '<div class="viewer-msg">这是个二进制文件，不支持预览<br>点下面的「下载到本机」看</div>';
      return;
    }
    // 编码：先按 UTF-8 解；乱码字符多（GBK 中文文件常见）就换 GBK 再解一遍，替换符少的赢
    let text = new TextDecoder('utf-8').decode(buf);
    const bad8 = (text.match(/\uFFFD/g) || []).length;
    if (bad8 > 0) {
      try {
        const g = new TextDecoder('gbk').decode(buf);
        if ((g.match(/\uFFFD/g) || []).length < bad8) text = g;
      } catch (err) { /* 浏览器不支持 gbk 就用 utf-8 的结果 */ }
    }
    body.innerHTML = /\.(md|markdown)$/i.test(e.name)
      ? '<div class="textview mdview">' + renderMarkdown(text, mdImgResolver(e)) + '</div>'
      : '<pre class="textview">' + esc(text) + '</pre>';
  } catch (err) {
    body.innerHTML = '<div class="viewer-msg">读取失败：' + esc(err.message || '连不上服务端') + '</div>';
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
  saveCurrentProgress();   // 换源（转码兜底/换清晰度）前把旧 video 的进度存掉
  host.innerHTML =
    '<video src="' + src + '" controls autoplay playsinline webkit-playsinline preload="metadata"></video>';
  const v = host.querySelector('video');
  v.dataset.normal = src;   // 手动转屏要能换回来
  v.addEventListener('error', () => {
    host.innerHTML = '<div class="viewer-msg">这个文件解不开（可能是损坏或不支持的编码）。</div>';
  });
  hookProgress(v);
  return v;
}

// ---------- 播放进度记忆 ----------
//
// 进度存服务端库（跟账号走，换手机也能续上），与「最近 / 收藏」同一口径。
// 规则：
//   · 打开时拉一次：上次位置 > 30s 且离结尾 > 90s 才跳过去，别打断正常观看；
//   · 播放中每 5 秒节流上报一次，暂停 / 关掉播放器立刻上报；
//   · 看完（ended）或拖回开头（pos≤0）即删记录，下次从头放。
// 所有视频路径（原画质 / 转码缓存 / 流式转码）都走 mountVideo，钩子只挂这一处。

function hookProgress(v) {
  let lastSave = 0;
  v.addEventListener('loadedmetadata', async () => {
    const e = viewerEntry;
    // 一分钟以内的短片不值得记，看完就完了
    if (!e || !isFinite(v.duration) || v.duration < 60) return;
    try {
      const r = await api('/api/fs/progress?' + q(S.root, e.path));
      // 返回时可能已经切了别的文件，别把 A 片的位置跳到 B 片上
      if (r.code !== 0 || !r.data || !viewerEntry || viewerEntry.path !== e.path) return;
      const pos = +r.data.pos || 0;
      if (pos > 30 && pos < v.duration - 90) {
        try {
          v.currentTime = pos;
          toast('已从上次位置继续播放');
        } catch (err) { /* 个别浏览器 metadata 后立即 seek 会抛，忽略 */ }
      }
    } catch (err) { /* 拉不到就从头放，对用户无感 */ }
  }, { once: true });
  v.addEventListener('timeupdate', () => {
    const now = Date.now();
    if (now - lastSave < 5000) return;
    lastSave = now;
    saveVideoProgress(v);
  });
  v.addEventListener('pause', () => saveVideoProgress(v));
  v.addEventListener('ended', () => {
    const e = viewerEntry;
    if (e) api('/api/fs/progress', { root: S.root, path: e.path, pos: 0, dur: 0 }).catch(() => {});
  });
}

function saveVideoProgress(v) {
  const e = viewerEntry;
  if (!e || !v || !v.isConnected) return;
  if (!isFinite(v.currentTime) || v.currentTime <= 0) return;
  api('/api/fs/progress', {
    root: S.root, path: e.path,
    pos: Math.floor(v.currentTime), dur: Math.floor(v.duration || 0),
  }).catch(() => {});
}

function saveCurrentProgress() {
  const v = viewerVideo();
  if (v) saveVideoProgress(v);
}

// ffmpeg 引导安装的前端：发起 → 轮询进度 → 成功后重走 playRemux 续播。
// 失败时给出原因 + 手动安装说明（把 ffmpeg.exe / ffprobe.exe 放到 selfnas.exe 旁边即可）。
function startFFGuide(e, dl) {
  const box = $('ffBox');
  const fmtMB = (n) => (n / 1048576).toFixed(1) + 'MB';
  const btn = document.createElement('button');
  btn.className = 'btn primary';
  btn.textContent = '立即安装并播放';
  box.appendChild(btn);
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    btn.textContent = '正在发起安装…';
    try {
      const r = await api('/api/ffmpeg/install', {});
      if (r.code !== 0) throw new Error(r.msg || '未知错误');
    } catch (err) {
      box.innerHTML = '<div style="opacity:.65;font-size:13px">安装发起失败：' + err.message + '</div>';
      btn.disabled = false;
      btn.textContent = '重试安装';
      return;
    }
    // 轮询进度。running=false 且 stage=done 且无 error 才算成功，
    // 其余 running=false 的情况都是失败（stage 停在哪一步就是哪一步出的错）。
    for (;;) {
      await new Promise((ok) => setTimeout(ok, 700));
      let st;
      try {
        st = (await api('/api/ffmpeg/status')).data;
      } catch (err) { continue; } // 单次轮询抖动不终止，下一轮再看
      if (!st) continue;
      if (st.stage === 'download' && st.total > 0) {
        const pct = Math.min(100, Math.round((st.bytes * 100) / st.total));
        btn.textContent = '下载中 ' + pct + '%（' + fmtMB(st.bytes) + ' / ' + fmtMB(st.total) + '）';
      } else if (st.stage === 'extract') {
        btn.textContent = '解压中…';
      } else if (st.stage === 'verify') {
        btn.textContent = '校验中…';
      }
      if (st.running) continue;
      if (st.stage === 'done' && !st.error) {
        toast('ffmpeg 安装完成');
        playRemux(e, dl); // 重走一遍：这次 playinfo 会返回 ffmpeg=true，自动续播
        return;
      }
      box.innerHTML =
        '<div style="opacity:.65;font-size:13px">安装失败：' + (st.error || '未知原因') +
        '<br>可手动下载 ffmpeg，把 ffmpeg.exe 和 ffprobe.exe 放到 selfnas.exe 同目录即可。</div>';
      btn.disabled = false;
      btn.textContent = '重试安装';
      return;
    }
  });
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
    // 播放器内引导安装：服务器没 ffmpeg 时不把用户直接推去下载页，
    // 就在这个弹层里问一句「要不要装」——同意就自动下载 → 解压 → 校验 → 接着播。
    // 在疼的地方给药：用户点 MKV 播不了的那一刻才需要它，装一次永久生效。
    body.innerHTML =
      '<div class="viewer-msg">这个格式在线播放需要服务器安装 ffmpeg。<br>' +
      '<span style="opacity:.65;font-size:13px">约 80MB，下载一次永久生效，完成后自动继续播放</span>' +
      '<div id="ffBox" style="margin-top:14px"></div></div>';
    foot.innerHTML = '<a class="btn" href="' + dl + '">仅下载文件</a>';
    startFFGuide(e, dl);
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
  saveCurrentProgress();   // 关掉播放器前把进度存一下，下次接着看
  // 音乐队列要一起收掉：否则关掉弹层后 ended 回调还挂在旧 audio 上，
  // 下次打开别的歌会「上一首的队列」接着往下唱
  if (MP.on) {
    mpSave(false);
    MP.on = false; MP.list = []; MP.i = -1; MP.el = null;
  }
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

// 新建文件夹：底部栏（手机）和顶栏（桌面）各有一个按钮，靠 data-mkdir 一起绑
document.querySelectorAll('[data-mkdir]').forEach((b) => {
  b.addEventListener('click', () => {
    if (S.readonly) { toast('该目录为只读'); return; }
    askText('新建文件夹', '', async (val) => {
      const r = await api('/api/fs/mkdir', { root: S.root, dir: S.path, name: val });
      toast(r.msg);
      if (r.code === 0) loadList();
      return r.code === 0;
    });
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

// 用户在系统选择器里点了「取消」：iOS 上取消不触发 change，页面收不到任何信号，
// 按钮就卡在「读取中」直到 2 分钟超时——取消是用户明确的意图，必须立即收场。
function cancelPick(reason) {
  if (!picking) return;
  picking = false;
  if (pickTimer) { clearTimeout(pickTimer); pickTimer = null; }
  paintPickButton();
  clog('上传等待已收场[' + reason + ']');
}

// 上传入口同样有两份（手机悬浮按钮 / 桌面顶栏），共用同一个 filePicker
document.querySelectorAll('[data-upload]').forEach((b) => {
  b.addEventListener('click', (e) => {
    if (!beginPick('filePicker', '照片/系统选择器')) e.preventDefault();
  });
});

// ---------- 悬浮操作按钮（手机端） ----------
// 底部两个大按钮收成一个「+」：点开展开上传/新建，点页面其他地方收起。
// 桌面端整个 FAB 都不渲染（CSS 隐藏），操作在顶栏。
const fabRoot = $('fsFab');
$('fabBtn').addEventListener('click', (e) => {
  e.stopPropagation();
  fabRoot.classList.toggle('on');
});
// 点 FAB 以外的任何地方都收起（列表滚动、点文件、点顶栏都算）
document.addEventListener('click', (e) => {
  if (fabRoot.classList.contains('on') && !fabRoot.contains(e.target)) {
    fabRoot.classList.remove('on');
  }
});
// 展开菜单里的动作点完就收：选择器/弹窗弹出来后，背后不该留着一串按钮
document.querySelectorAll('.fab-menu [data-upload], .fab-menu [data-mkdir]').forEach((b) => {
  b.addEventListener('click', () => fabRoot.classList.remove('on'));
});

// ---------- 切换账号 / 退出 ----------
// 多账号之后退出是常规操作：换个账号登进来，看到的世界就跟着换。
$('btnLogoutFs').addEventListener('click', async () => {
  if (!confirm('退出当前账号？')) return;
  try { await api('/api/logout', {}, 'POST'); } catch (e) { /* 网络失败也照样回登录页 */ }
  showLogin();
  toast('已退出，可换一个账号登录');
});

// 桌面端拖拽上传：把文件从资源管理器拖进页面，松手就传，复用同一套分片队列。
// 手机上不存在拖拽事件，这段代码在手机端等于不存在。
(function initDragDrop() {
  const hint = document.createElement('div');
  hint.className = 'drop-hint';
  hint.innerHTML = '<div class="drop-hint-box">松手即可上传到当前目录</div>';
  document.body.appendChild(hint);
  let depth = 0;      // 子元素会连续触发 enter/leave，用计数避免提示闪
  const hasFile = (e) => !!(e.dataTransfer && Array.from(e.dataTransfer.types || []).indexOf('Files') >= 0);
  window.addEventListener('dragenter', (e) => {
    if (!hasFile(e)) return;
    depth++;
    hint.classList.add('on');
  });
  window.addEventListener('dragover', (e) => {
    if (!hasFile(e)) return;
    e.preventDefault();   // 不拦的话浏览器会直接打开文件
  });
  window.addEventListener('dragleave', () => {
    depth = Math.max(0, depth - 1);
    if (!depth) hint.classList.remove('on');
  });
  window.addEventListener('drop', (e) => {
    if (!hasFile(e)) return;
    e.preventDefault();
    depth = 0;
    hint.classList.remove('on');
    if (S.readonly) { toast('该目录为只读'); return; }
    const files = e.dataTransfer && e.dataTransfer.files;
    if (files && files.length) handleFiles(files, 'drop');
  });
})();

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
  // 大文件提示（2026-10-31 改口径）：不再跳过/拒绝，弹确认框说明亮屏要求，坚持就传。
  // confirm 只弹一次：整批里有大文件就问一遍，逐个问会把人烦死。
  const big = files.filter((f) => f.size > WEB_HINT_SIZE);
  if (big.length && !confirm(WEB_HINT_TEXT(big.length))) {
    toast('已取消上传。大文件建议用 SMB / WebDAV（系统接管，锁屏也能传完）');
    return;
  }
  toast(big.length
    ? '已选择 ' + files.length + ' 个文件开始上传；其中 ' + big.length + ' 个超过 50 MB，请保持亮屏（大文件更推荐 SMB / WebDAV）'
    : '已选择 ' + files.length + ' 个文件，开始上传');
  files.forEach((f) => S.queue.push({ id: ++TASK_SEQ, file: f, done: false, size: f.size, err: '', doneBytes: 0, partial: {}, conc: 2 }));
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
  // 所有上传按钮（手机底部栏 + 桌面顶栏）同步显示等待状态
  document.querySelectorAll('[data-upload]').forEach((b) => {
    if (picking) {
      const s = pickStartAt ? Math.floor((Date.now() - pickStartAt) / 1000) : 0;
      b.textContent = s >= 1 ? '读取中 ' + s + 's' : '读取中…';
      b.classList.add('busy');
    } else {
      b.textContent = BTN_UPLOAD_TEXT;
      b.classList.remove('busy');
    }
  });
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

// 取消收场之一：现代浏览器（iOS 16.4+ / Chrome 113+）在用户取消文件选择时会派发 cancel 事件，
// 监听到就立刻把「读取中」收掉。
$('filePicker').addEventListener('cancel', () => cancelPick('系统选择器取消'));

// 取消收场之二（兜底）：老 iOS 没有 cancel 事件，取消后页面重新可见但没有 change。
// 正常导出完成的路径里 change 发生在页面可见**之前**（选择器盖着页面导出，导完才回来），
// 所以「已经回到页面 1.5s 还没等到 change」基本就是取消——收掉。
// 代价可控：万一撞上「导出中页面短暂可见」的怪场景，也只是按钮文字提前恢复，
// change 来了照常入队上传，字节一个不少。
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'visible' || !picking) return;
  setTimeout(() => { if (picking) cancelPick('回前台 1.5s 无 change（判定为取消）'); }, 1500);
});

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
let keepOsc = null, keepCtx = null;   // Wake Lock 不可用时的音频保活退化方案

// 亮屏锁。**绝不阻塞上传主流程**：pump() 里是裸调用、不带 await。
// 2026-11-01 真机教训：旧实现让 pump await 本函数，iPhone Chrome 走 http（非安全
// 上下文）没有 Wake Lock → 落进音频保活 → AudioContext.resume() 在丢失手势链路的
// 异步调用里可能永远不落定 → pump 卡死在 await 上，队列永远 0B。
// 所以这里全部改成 fire-and-forget，任何 Promise 都只挂 .catch，不让调用方等。
function acquireWake() {
  try {
    // 注意别只判 'wakeLock' in navigator：有的环境属性在但值是 undefined
    //（2026-11-01 探针实测，这种形态下 .request 直接同步抛 TypeError 会炸穿 pump）
    if ('wakeLock' in navigator && navigator.wakeLock && typeof navigator.wakeLock.request === 'function') {
      if (wakeLock) return;
      navigator.wakeLock.request('screen').then((lock) => {
        wakeLock = lock;
        lock.addEventListener('release', () => { if (wakeLock === lock) wakeLock = null; });
      }).catch(() => { startAudioKeepAlive(); });   // 有 API 但不给用（如非安全上下文的变体）→ 也退化
      return;
    }
  } catch (e) {}
  startAudioKeepAlive();
}

function startAudioKeepAlive() {
  if (keepOsc) return;
  try {
    keepCtx = keepCtx || new (window.AudioContext || window.webkitAudioContext)();
    const osc = keepCtx.createOscillator();
    const gain = keepCtx.createGain();
    gain.gain.value = 0.0001;
    osc.connect(gain);
    gain.connect(keepCtx.destination);
    osc.start();   // suspended 状态下 start 也合法，resume 成功后自然「在播」（音量人耳不可闻）
    keepOsc = osc;
    if (keepCtx.state === 'suspended') keepCtx.resume().catch(() => {});
  } catch (e) {}
}

function releaseWake() {
  try { if (wakeLock) wakeLock.release(); } catch (e) {}
  wakeLock = null;
  try { if (keepOsc) { keepOsc.stop(); keepOsc.disconnect(); } } catch (e) {}
  keepOsc = null;
}

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
  acquireWake();   // 裸调用：亮屏锁绝不能阻塞上传（2026-11-01 真机教训，见函数注释）
  renderUpload();
  while (!abortAll && S.queue.some((t) => !t.done && !t.err)) {
    const task = S.queue.find((t) => !t.done && !t.err);
    // 单个文件出异常（比如 File 句柄失效导致 slice 抛错）不能让 pump 直接 reject：
    // 那样 S.busy 永远是 true，界面永远停在「上传中」，连重试按钮都不出来。
    try {
      await uploadOne(task);
    } catch (err) {
      if (abortAll) break;   // 中止引发的异常（XHR 被掐等）不用记成任务失败
      task.err = '出错了：' + (err && err.message ? err.message : err);
      clog('上传异常 ' + task.file.name + ' : ' + task.err);
    }
    renderUpload();
  }
  releaseWake();
  S.busy = false;
  if (abortAll) {
    // 用户主动停止：收尾（清队列、清服务端分片）由 stopAllUploads() 接管，
    // 这里什么都不说、不标失败 —— 标了 err 会闪出「重试」按钮。
    // abortAll 只在这里复位（pump 万一卡在慢查询上，标志保持 true 才能拦住新分片）。
    abortAll = false;
    // 停止期间点过重试/新加了文件的话，队列里还有活就补一泵；
    // 正常停止后队列已被 stopAllUploads 清空，这里自然不会泵。
    if (pumpQueued && S.queue.some((t) => !t.done && !t.err)) { pumpQueued = false; pump(); }
  } else if (S.queue.every((t) => t.done)) {
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

// 「停止上传」完整流程：掐在途分片 → 等 pump 退出 → 清服务端分片缓存（.part/.part.meta）
// → 清队列。abortAll 标志由 pump 收尾时复位，这里不碰。
async function stopAllUploads() {
  if (!S.busy) {   // 没在传（或刚好传完了）：直接清队列就行
    S.queue = [];
    lastStruct = '';
    renderUpload();
    return;
  }
  abortAll = true;
  liveChunks.forEach((x) => { try { x.abort(); } catch (e) {} });
  // 等 pump 退出：各层循环最迟在下一个检查点返回，sleep/statusOf 都有超时兜底。
  // 10 秒保险丝：万一有意外挂点，也照常往下走（pump 退出后自己会复位标志）。
  for (let i = 0; i < 50 && S.busy; i++) await sleep(200);
  // 清服务端分片缓存。已完成的任务成品已落盘，不碰；没传完的把缓存也删了，
  // 下次再传同名文件从零开始，不会撞上这次的半成品区间表。
  const targets = S.queue.filter((t) => !t.done);
  await Promise.all(targets.map((t) =>
    api('/api/fs/upload/abort?root=' + encodeURIComponent(S.root) +
      '&dir=' + encodeURIComponent(t.dir || S.path) +
      '&name=' + encodeURIComponent(t.file.name), null, 'DELETE').catch((e) => {
      clog('清分片缓存失败 ' + t.file.name + ' : ' + (e && e.message ? e.message : e));
    })
  ));
  clog('上传被用户停止，清理 ' + targets.length + ' 个任务的分片缓存');
  S.queue = [];
  lastStruct = '';
  renderUpload();
  toast(targets.length ? '已停止上传，未传完的部分已丢弃' : '已停止上传');
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

// 「停止上传」的全局开关：置 true 后在途分片被整批 abort、各层循环尽快退出。
// 只在 stopAllUploads() 里置 true、等 pump 收尾后复位，别的地方别碰。
let abortAll = false;
const liveChunks = new Set();

// 返回 'sent' | 'timeout' | 'error' | 'aborted'
function postChunk(url, blob, onSent) {
  // 全局中止：不再发新片，直接按 aborted 返回，让调用方的循环自己退出
  if (abortAll) return Promise.resolve('aborted');
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    liveChunks.add(xhr);   // 登记在途分片，「停止上传」时要整批掐掉
    xhr.open('POST', url, true);
    xhr.responseType = 'text';
    let done = false;
    let timer = null;
    let bodySent = false;

    const fin = (why) => {
      if (done) return;
      done = true;
      liveChunks.delete(xhr);
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
  t.dir = dir;   // 停止上传后清服务端分片缓存要按「任务实际用的目录」来
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
    if (abortAll) return;   // 用户点了「停止上传」，别再开新一轮
    const sentCount = pending.length;
    t.round = round;
    t.pendingN = pending.length;
    await sendRound(t, dir, pending, conc);
    if (abortAll) return;
    await sleep(260);
    if (abortAll) return;

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
    if (abortAll) return;   // 中止时不做最终核对，也别把任务标成 err
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
      if (abortAll) return;   // 中止：不再发新片
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
      if (abortAll) return;
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
    // 队列里有 30MB+ 的大文件时提示：亮屏锁已申请，但更稳的路还是系统传输
    if (S.queue.some((t) => t.size >= 30 * 1024 * 1024)) {
      html += '<div class="up-tip">大文件上传中：<b>亮屏锁已自动申请</b>（页面保持在前台就不会熄屏）。' +
        '更推荐改用系统「文件」App（iOS）/ 文件管理器（安卓）<b>连 WebDAV 或 SMB</b>：' +
        '传输由系统接管，锁屏、切后台都能传完。地址在管理台概览页（形如 <code>' +
        location.origin + '/dav/</code>）。</div>';
    }
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
      if (S.busy) {
        // 用户原话（2026-11-01）：上传中点关闭要「问他一下，确定的话就得停」。
        const n = S.queue.filter((t) => !t.done).length;
        if (!confirm('正在上传中，确定要停止吗？\n' + n + ' 个未传完的任务将丢弃（已传完的文件不受影响）。')) return;
        stopAllUploads();
        return;
      }
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


// 一个按钮循环切四态：名称↑ → 名称↓ → 时间↓ → 时间↑ → 名称↑。
// 排成这个顺序是因为「先按名字、再按时间」最常用，而时间默认要新的在前（↓），
// 切到时间那次直接给 ↓ 比先给「最老的在前」合手。
const SORT_CYCLE = [
  ['name', 'asc'], ['name', 'desc'], ['time', 'desc'], ['time', 'asc'],
];
$('btnSort').addEventListener('click', () => {
  const cur = SORT_CYCLE.findIndex((c) => c[0] === S.sort.by && c[1] === S.sort.dir[S.sort.by]);
  const next = SORT_CYCLE[(cur + 1) % SORT_CYCLE.length];
  S.sort.by = next[0];
  S.sort.dir[next[0]] = next[1];
  saveSort();
  renderSort();
  renderList();          // 只重排列表，不重新请求：排序是本地行为
  toast('排序：' + sortLabel());   // 手机上按钮只有图标，不提示一句根本不知道点了什么
});
renderSort();


// ---------- 分享海报 ----------
//
// 一张竖版推广图：项目介绍 + 功能点 + 插画 + GitHub 二维码 + 链接。
// 为什么用 canvas 手绘而不是让 AI 画整张：
//   ① **二维码必须是真码** —— 生成式模型画出来的码扫不出来，只能贴真实 PNG；
//   ② 文字要清晰锐利、排版要稳，AI 画中文字经常缺笔画；
//   ③ 插画单独用 ComfyUI 生成（无文字、纯装饰），只占中间一块，画歪了也不影响信息。
// 画完转成 <img> 展示：手机上 canvas 长按不弹菜单，img 才能“长按保存”。

const POSTER_W = 1080;
const POSTER_H = 2220;
// 海报二维码的落点：目标读者是普通用户，扫了就该去**下载页**，
// 仓库主页/源码对他们没有意义。（码本身是贴的 github-qr.png，两者要一致）
const POSTER_QR_URL = 'https://github.com/sylvesterzhang/selfnas/releases/latest';
let posterImgs = null;   // 图片只加载一次，反复打开不必重下

function loadImg(src) {
  return new Promise((resolve, reject) => {
    const im = new Image();
    im.onload = () => resolve(im);
    im.onerror = () => reject(new Error('图片加载失败: ' + src));
    im.src = src;
  });
}

function roundRectPath(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

// 按最大宽度/高度等比缩放，返回绘制用的 w/h
function fitBox(img, maxW, maxH) {
  const k = Math.min(maxW / img.width, maxH / img.height);
  return { w: Math.round(img.width * k), h: Math.round(img.height * k) };
}

// —— 平台 logo：直接喂 SVG path 给 Path2D，矢量清晰、不额外加图片资源 ——
const LOGO_WIN = 'M11.4 24H0V12.6h11.4V24zM24 24H12.6V12.6H24V24zM11.4 11.4H0V0h11.4v11.4zm12.6 0H12.6V0H24v11.4z';
const LOGO_APPLE = 'M12.152 6.896c-.948 0-2.415-1.078-3.96-1.04-2.04.027-3.91 1.183-4.961 3.014-2.117 3.675-.546 9.103 1.519 12.09 1.013 1.454 2.208 3.09 3.792 3.039 1.52-.065 2.09-.987 3.935-.987 1.831 0 2.35.987 3.96.948 1.637-.026 2.676-1.48 3.676-2.948 1.156-1.688 1.636-3.325 1.662-3.415-.039-.013-3.182-1.221-3.22-4.857-.026-3.04 2.48-4.494 2.597-4.559-1.429-2.09-3.623-2.324-4.39-2.376-2-.156-3.675 1.09-4.61 1.09zM15.53 3.83c.843-1.012 1.4-2.427 1.245-3.83-1.207.052-2.662.805-3.532 1.818-.78.896-1.454 2.338-1.273 3.714 1.338.104 2.715-.688 3.559-1.701';

// 把 24×24 的 logo 路径等比缩放后画在以 (cx,cy) 为中心的框里
function pathLogo(c, d, cx, cy, maxH, color) {
  const k = maxH / 24;
  c.save();
  c.translate(cx - 12 * k, cy - 12 * k);
  c.scale(k, k);
  c.fillStyle = color;
  c.fill(new Path2D(d));
  c.restore();
}

// Linux 的 Tux：简易企鹅（Path2D 画不了带镂空的双色，用基本图形拼，反而更像图标）
function drawTux(c, cx, cy, S) {
  const top = cy - S / 2;
  const bg = '#eef4fb';
  c.fillStyle = '#1e2a3a';
  c.beginPath(); c.ellipse(cx, top + S * 0.56, S * 0.31, S * 0.40, 0, 0, Math.PI * 2); c.fill();
  c.fillStyle = bg;
  c.beginPath(); c.ellipse(cx, top + S * 0.64, S * 0.20, S * 0.29, 0, 0, Math.PI * 2); c.fill();
  c.fillStyle = bg;
  c.beginPath(); c.arc(cx - S * 0.10, top + S * 0.30, S * 0.06, 0, Math.PI * 2); c.fill();
  c.beginPath(); c.arc(cx + S * 0.10, top + S * 0.30, S * 0.06, 0, Math.PI * 2); c.fill();
  c.fillStyle = '#f2a33c';
  c.beginPath();
  c.moveTo(cx, top + S * 0.38);
  c.lineTo(cx - S * 0.09, top + S * 0.48);
  c.lineTo(cx + S * 0.09, top + S * 0.48);
  c.closePath(); c.fill();
  c.beginPath(); c.ellipse(cx - S * 0.12, top + S * 0.95, S * 0.09, S * 0.045, 0, 0, Math.PI * 2); c.fill();
  c.beginPath(); c.ellipse(cx + S * 0.12, top + S * 0.95, S * 0.09, S * 0.045, 0, 0, Math.PI * 2); c.fill();
}

function bubble(c, x, y, w, h, r, fill) {
  roundRectPath(c, x, y, w, h, r);
  c.fillStyle = fill;
  c.fill();
}

async function renderPoster() {
  if (!posterImgs) {
    posterImgs = await Promise.all([
      loadImg('/assets/img/poster-art.png'),
      loadImg('/assets/img/github-qr.png'),
      // 品牌字标（自带上「SelfNAS」六个字母）。加载失败给 null，退回排字兜底 ——
      // 不能因为一张装饰图挂掉整张海报。
      loadImg('/assets/img/wordmark.png').catch(() => null),
    ]);
  }
  const [art, qr, wm] = posterImgs;

  const cv = document.createElement('canvas');
  cv.width = POSTER_W;
  cv.height = POSTER_H;
  const c = cv.getContext('2d');

  // —— 背景：浅色渐变 + 右上角淡色光斑
  const bg = c.createLinearGradient(0, 0, POSTER_W * 0.4, POSTER_H);
  bg.addColorStop(0, '#ffffff');
  bg.addColorStop(0.55, '#f5f9fd');
  bg.addColorStop(1, '#eaf3fb');
  c.fillStyle = bg;
  c.fillRect(0, 0, POSTER_W, POSTER_H);

  c.save();
  const glow = c.createRadialGradient(POSTER_W - 60, 120, 20, POSTER_W - 60, 120, 520);
  glow.addColorStop(0, 'rgba(24, 95, 165, 0.13)');
  glow.addColorStop(1, 'rgba(24, 95, 165, 0)');
  c.fillStyle = glow;
  c.fillRect(0, 0, POSTER_W, 700);
  c.restore();

  const FONT = '"Microsoft YaHei", "PingFang SC", "Hiragino Sans GB", -apple-system, sans-serif';
  const MARGIN = 96;
  const CONTENT_W = POSTER_W - MARGIN * 2;

  // 中文文案长度不固定，写死字号容易出框 —— 统一按最大宽度往下缩
  function fitText(text, weight, size, minSize) {
    let s = size;
    c.font = weight + ' ' + s + 'px ' + FONT;
    while (c.measureText(text).width > CONTENT_W && s > minSize) {
      s -= 1;
      c.font = weight + ' ' + s + 'px ' + FONT;
    }
    return s;
  }

  // —— 标题：直接用品牌字标（图里已经带「SelfNAS」六个字母，不再用 canvas 排字）
  // 位置对齐原来那行 104px 文字的视觉框：上沿 ≈182、基线 258、字高 ≈80。
  // 这张图已经裁到墨迹边界且是透明底，所以按目标矩形直接铺，不用再留内边距。
  if (wm) {
    const wmH = 80;
    const wmW = wmH * (wm.width / wm.height);
    c.drawImage(wm, MARGIN, 182, wmW, wmH);
  } else {
    c.fillStyle = '#0f1b2d';
    c.font = '700 104px ' + FONT;
    c.textBaseline = 'alphabetic';
    c.fillText('selfnas', MARGIN, 258);
  }

  // 标题右侧的小标签
  c.font = '400 30px ' + FONT;
  const tag = '私有文件站';
  const tagW = c.measureText(tag).width + 40;
  roundRectPath(c, POSTER_W - MARGIN - tagW, 198, tagW, 56, 28);
  c.fillStyle = 'rgba(24, 95, 165, 0.10)';
  c.fill();
  c.fillStyle = '#185fa5';
  c.fillText(tag, POSTER_W - MARGIN - tagW + 20, 236);

  // —— 一句话说完它是干什么的
  c.fillStyle = '#1e2a3a';
  c.font = '600 46px ' + FONT;
  c.fillText('把电脑变成你自己的文件中心', MARGIN, 348);

  // —— 卖点：轻、不装东西（这才是它跟网盘 / NAS 的区别）
  c.fillStyle = '#64748b';
  let subSize = fitText('不用装 App · 不用上传网盘 · 不用装 NAS 系统', '400', 32, 24);
  c.font = '400 ' + subSize + 'px ' + FONT;
  c.fillText('不用装 App · 不用上传网盘 · 不用装 NAS 系统', MARGIN, 404);

  // —— 定位：补上「电脑↔手机」这块空白，不用再找线
  c.fillStyle = '#8a97a8';
  subSize = fitText('小到几乎不占地方，让电脑和手机随时互通，再也不用找数据线', '400', 30, 22);
  c.font = '400 ' + subSize + 'px ' + FONT;
  c.fillText('小到几乎不占地方，让电脑和手机随时互通，再也不用找数据线', MARGIN, 450);

  // —— 主视觉卡：**整张卡就是一张图**
  // 之前是「插画一张卡 + 文字浮在外面」，视觉上像两块东西拼在一起。
  // 现在底图铺满整卡，平台图标和三个卖点都压在图里 —— 图片和文字是一个整体。
  // 文字必须用 canvas 画（AI 画中文会缺笔画），所以底图只留画面、不带任何字。
  const feats = [
    ['躺着看片', '手机直接播放电脑里的电影，MKV 也能放'],
    ['拍完就传', '手机照片视频一键传回电脑，接着剪辑修图'],
    ['随手就翻', '家里任意一块硬盘，都能用手机直接翻'],
  ];

  const heroX = MARGIN;
  const heroW = CONTENT_W;
  const heroTop = 496;
  const heroH = 880;

  // 卡片底（圆角 + 阴影）
  c.save();
  c.shadowColor = 'rgba(15, 27, 45, 0.10)';
  c.shadowBlur = 30;
  c.shadowOffsetY = 10;
  roundRectPath(c, heroX, heroTop, heroW, heroH, 32);
  c.fillStyle = '#ffffff';
  c.fill();
  c.restore();

  // 底图铺满整卡（cover：等比放大到盖满，多余部分裁掉）
  c.save();
  roundRectPath(c, heroX, heroTop, heroW, heroH, 32);
  c.clip();
  // 垂直方向不居中：底图上半是空天、下半是干净地面，整体往上抬 heroLift 像素，
  // 把画面里的空白从「卖点文字头顶」挪到「卡顶图标头顶」，文字区才有呼吸感。
  // ⚠️ 上抬时**必须把高度一起补足**（heroH + heroLift）：只改 y 不改尺寸，
  // 底图底边就会停在半腰，卡片最下面那条露出白卡底色 —— 用户一眼就看出来
  // 「下半段变白了、背景没铺到卡片底部」（2026-11-01 实测 y=1320~1374 全是 255,255,255）。
  // 现在按「底边贴住卡底」反推缩放，多出来的高度从**画面顶部**裁掉，底部永远压满。
  const heroLift = 70;
  const heroScale = Math.max(heroW / art.width, (heroH + heroLift + 4) / art.height);
  const heroDW = art.width * heroScale;
  const heroDH = art.height * heroScale;
  c.drawImage(art, heroX + (heroW - heroDW) / 2, heroTop - heroLift, heroDW, heroDH);

  // 上下各铺一层白色渐变，只用来「提亮 + 让文字够对比」，**不能盖死底图**。
  // 早期版本底部是 0.88~0.99 的白，下半张卡直接变纯白 —— 看上去还是「上面一张图、
  // 下面一块白」，割裂感没解决（2026-11-01 用户第二次提）。现在最重只到 0.32，
  // 底图那片浅蓝地面完整透出来，卖点文字是压在图上的，不是压在白色上。
  const topScrim = c.createLinearGradient(0, heroTop, 0, heroTop + 170);
  topScrim.addColorStop(0, 'rgba(255,255,255,0.66)');
  topScrim.addColorStop(1, 'rgba(255,255,255,0)');
  c.fillStyle = topScrim;
  c.fillRect(heroX, heroTop, heroW, 170);

  const botScrim = c.createLinearGradient(0, heroTop + heroH - 360, 0, heroTop + heroH);
  botScrim.addColorStop(0, 'rgba(255,255,255,0)');
  botScrim.addColorStop(1, 'rgba(255,255,255,0.15)');
  c.fillStyle = botScrim;
  c.fillRect(heroX, heroTop + heroH - 360, heroW, 360);
  c.restore();

  // —— 支持平台（压在图顶部，属于这张卡的一部分）
  (function drawPlatforms() {
    const items = [
      { kind: 'win', label: 'Windows' },
      { kind: 'mac', label: 'macOS' },
      { kind: 'linux', label: 'Linux' },
    ];
    const chipH = 64;
    const chipPadX = 20;
    const iconBox = 34;
    const gapIcon = 12;
    const gapChip = 16;
    const y = heroTop + 44;

    c.font = '600 28px ' + FONT;
    const widths = items.map(it => chipPadX * 2 + iconBox + gapIcon + c.measureText(it.label).width);
    const total = widths.reduce((a, b) => a + b, 0) + gapChip * (items.length - 1);
    let x = heroX + (heroW - total) / 2;

    items.forEach((it, i) => {
      const w = widths[i];
      roundRectPath(c, x, y, w, chipH, chipH / 2);
      c.fillStyle = 'rgba(255,255,255,0.92)';
      c.fill();
      c.strokeStyle = 'rgba(24,95,165,0.14)';
      c.lineWidth = 2;
      c.stroke();

      const iconCx = x + chipPadX + iconBox / 2;
      const iconCy = y + chipH / 2;
      if (it.kind === 'win') {
        pathLogo(c, LOGO_WIN, iconCx, iconCy, iconBox, '#0078d4');
      } else if (it.kind === 'mac') {
        pathLogo(c, LOGO_APPLE, iconCx, iconCy - 1, iconBox + 2, '#1d1d1f');
      } else {
        drawTux(c, iconCx, iconCy, iconBox + 4);
      }

      c.fillStyle = '#0f1b2d';
      c.font = '600 28px ' + FONT;
      c.textBaseline = 'middle';
      c.fillText(it.label, x + chipPadX + iconBox + gapIcon, iconCy + 1);
      c.textBaseline = 'alphabetic';

      x += w + gapChip;
    });
  })();

  // —— 三个卖点（同样压在图底部，跟图是一体的）
  const featTop = heroTop + 660;
  const featStep = 74;
  feats.forEach(([title, desc], i) => {
    const fy = featTop + i * featStep;
    c.beginPath();
    c.arc(heroX + 56, fy - 12, 17, 0, Math.PI * 2);
    c.fillStyle = '#185fa5';
    c.fill();
    c.strokeStyle = '#fff';
    c.lineWidth = 4;
    c.lineCap = 'round';
    c.beginPath();
    c.moveTo(heroX + 48, fy - 12);
    c.lineTo(heroX + 54, fy - 6);
    c.lineTo(heroX + 65, fy - 19);
    c.stroke();

    c.fillStyle = '#0f1b2d';
    c.font = '600 34px ' + FONT;
    c.fillText(title, heroX + 100, fy);
    const tw = c.measureText(title).width;
    c.fillStyle = '#5b6b7f';
    c.font = '400 30px ' + FONT;
    c.fillText('　' + desc, heroX + 100 + tw, fy);
  });

  // —— 「不想折腾？让 AI 帮你装」：把提示词直接演一遍（模拟聊天框）
  // 为什么要这块：很多人不会敲命令、也懒得看文档。告诉他「把地址丢给 WorkBuddy 就行」，
  // 比再写十条安装命令都有效 —— 而且这是真实可行的（AI 能下载、解压、起服务）。
  //
  // 对话分工很关键：「设成开机自启」必须由**用户**提，AI 只答应「好，我来下载安装」。
  // 否则读者根本不知道这软件还带自启能力 —— 需求得从用户嘴里说出来才像真实场景。
  const aiY = 1416;
  const aiCardH = 356;
  c.save();
  c.shadowColor = 'rgba(15, 27, 45, 0.10)';
  c.shadowBlur = 32;
  c.shadowOffsetY = 10;
  roundRectPath(c, MARGIN, aiY, CONTENT_W, aiCardH, 28);
  c.fillStyle = '#ffffff';
  c.fill();
  c.restore();

  const aiPadX = MARGIN + 36;
  const aiRight = POSTER_W - aiPadX;

  c.fillStyle = '#0f1b2d';
  c.font = '700 36px ' + FONT;
  c.fillText('不想折腾？让 AI 帮你装', aiPadX, aiY + 62);

  c.fillStyle = '#64748b';
  c.font = '400 26px ' + FONT;
  c.fillText('把 GitHub 地址发给 WorkBuddy，一句话就能装好', aiPadX, aiY + 102);

  // 用户气泡（右对齐，主色）：真实场景是「把地址粘进去 + 说一句要装」
  // ⚠️ 地址必须真的写出来。早期只写「帮我装一下 selfnas」，会让人以为 AI 天生知道
  // 装哪个仓库；而「把地址丢给 AI」正是这张海报要教的那一步（2026-11-01 用户指出）。
  // 地址用等宽字体，看起来就是粘进来的链接，不是手打的中文。
  (function userBubble() {
    const urlLine = 'github.com/sylvesterzhang/selfnas';
    const sayLine = '帮我装一下，顺便设成开机自启';
    const urlFont = '400 24px ui-monospace, Consolas, monospace';
    const sayFont = '400 26px ' + FONT;

    c.font = urlFont;
    const wUrl = c.measureText(urlLine).width;
    c.font = sayFont;
    const wSay = c.measureText(sayLine).width;

    const padX = 28, lineH = 40;
    const w = Math.max(wUrl, wSay) + padX * 2;
    const h = 2 * lineH + 26;
    const x = aiRight - w;
    const y = aiY + 126;
    bubble(c, x, y, w, h, 22, '#185fa5');

    c.fillStyle = '#ffffff';
    const baseY = y + 20 + 26;
    c.font = urlFont;
    c.fillText(urlLine, x + padX, baseY);
    c.font = sayFont;
    c.fillText(sayLine, x + padX, baseY + lineH);
  })();

  // AI 气泡（左对齐，浅底）+ 小头像：只答应「下载安装」，不重复用户的需求
  (function aiBubble() {
    const txt = '好，我来下载安装。';
    c.font = '400 26px ' + FONT;
    const padX = 26;
    const w = c.measureText(txt).width + padX * 2;
    const h = 72;
    const avR = 21;
    const y = aiY + 248;
    const bx = aiPadX + avR * 2 + 16;

    c.beginPath();
    c.arc(aiPadX + avR, y + h / 2, avR, 0, Math.PI * 2);
    c.fillStyle = '#185fa5';
    c.fill();
    c.fillStyle = '#ffffff';
    c.font = '700 24px ' + FONT;
    c.textAlign = 'center';
    c.textBaseline = 'middle';
    c.fillText('W', aiPadX + avR, y + h / 2 + 1);
    c.textAlign = 'left';
    c.textBaseline = 'alphabetic';

    bubble(c, bx, y, w, h, 22, '#eef3f9');
    c.fillStyle = '#1e2a3a';
    c.font = '400 26px ' + FONT;
    c.fillText(txt, bx + padX, y + 46);
  })();

  // —— 二维码卡片（压轴：看完卖点和装法，最后才引导扫码）
  const cardY = 1812;
  const cardH = 288;
  c.save();
  c.shadowColor = 'rgba(15, 27, 45, 0.10)';
  c.shadowBlur = 32;
  c.shadowOffsetY = 10;
  roundRectPath(c, MARGIN, cardY, CONTENT_W, cardH, 28);
  c.fillStyle = '#ffffff';
  c.fill();
  c.restore();

  // 二维码本体（真码，扫了直达 GitHub 下载页——普通用户要的是下载，不是看源码）。
  // 源图 512px，这里画 256px —— **正好 1/2 的整数倍缩放**，每个模块边缘不产生小数抗锯齿，
  // 扫描识别率最高；随手给个 0.37 倍反而会发虚。
  const qrSize = 256;
  const qrX = MARGIN + 36;
  const qrY = cardY + (cardH - qrSize) / 2;
  c.fillStyle = '#ffffff';
  c.fillRect(qrX, qrY, qrSize, qrSize);
  c.drawImage(qr, qrX, qrY, qrSize, qrSize);

  const tx = qrX + qrSize + 40;
  const textW = 96 + (POSTER_W - 192) - tx - 24;   // 卡片右侧可用宽度，超了就是出框
  c.fillStyle = '#0f1b2d';
  let fs = 38;
  c.font = '700 ' + fs + 'px ' + FONT;
  while (c.measureText('扫码下载，完全开源免费').width > textW && fs > 24) {
    fs -= 2;
    c.font = '700 ' + fs + 'px ' + FONT;
  }
  c.fillText('扫码下载，完全开源免费', tx, cardY + 90);

  c.fillStyle = '#185fa5';
  fs = 24;
  c.font = '400 ' + fs + 'px ui-monospace, Consolas, monospace';
  while (c.measureText('github.com/sylvesterzhang/selfnas').width > textW && fs > 16) {
    fs -= 1;
    c.font = '400 ' + fs + 'px ui-monospace, Consolas, monospace';
  }
  c.fillText('github.com/sylvesterzhang/selfnas', tx, cardY + 140);

  c.fillStyle = '#64748b';
  fs = 26;
  c.font = '400 ' + fs + 'px ' + FONT;
  while (c.measureText('数据都在自己电脑上，不经过任何第三方').width > textW && fs > 18) {
    fs -= 1;
    c.font = '400 ' + fs + 'px ' + FONT;
  }
  c.fillText('数据都在自己电脑上，不经过任何第三方', tx, cardY + 192);

  // —— 页脚
  c.fillStyle = '#94a3b8';
  c.font = '400 28px ' + FONT;
  c.textAlign = 'center';
  c.fillText('selfnas · 让手机和电脑之间没有那根线', POSTER_W / 2, 2154);
  c.textAlign = 'left';

  return cv.toDataURL('image/png');
}

let posterDataURL = '';

async function openPoster() {
  const overlay = $('poster');
  const body = $('posterBody');
  overlay.classList.add('on');
  body.innerHTML = '<div class="poster-loading">正在生成海报…</div>';
  try {
    posterDataURL = await renderPoster();
    body.innerHTML = '';
    const im = document.createElement('img');
    im.className = 'poster-img';
    im.alt = 'selfnas 宣传海报';
    im.src = posterDataURL;
    body.appendChild(im);
  } catch (e) {
    body.innerHTML = '<div class="poster-loading">海报生成失败：' + esc(e.message) + '</div>';
  }
}

$('btnPoster').addEventListener('click', openPoster);
$('posterClose').addEventListener('click', () => {
  $('poster').classList.remove('on');
  $('posterBody').innerHTML = '';   // 4MB 的 dataURL 留着占内存，关掉就放
});
$('poster').addEventListener('click', (ev) => {
  if (ev.target.id === 'poster') {
    $('poster').classList.remove('on');
    $('posterBody').innerHTML = '';
  }
});
// dataURL → File，给 Web Share 用（分享面板只认 File/Blob，不认 dataURL）
function dataURLtoFile(dataURL, name) {
  try {
    const bin = atob(dataURL.split(',')[1]);
    const arr = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
    return new File([arr], name, { type: 'image/png' });
  } catch (e) { return null; }
}

$('posterSave').addEventListener('click', async () => {
  if (!posterDataURL) return;
  // iOS Safari 的 a[download] 会把图塞进「文件」App 深处，用户根本找不到。
  // iPhone 上优先走系统分享面板，能直接「存储到照片」；桌面/安卓保持原来的直接下载
  // （局域网 http 不是 secure context，iOS 上拿不到 Web Share，靠下面的长按提示兜底）。
  const f = isIOS ? dataURLtoFile(posterDataURL, 'selfnas-poster.png') : null;
  if (f && navigator.canShare && navigator.canShare({ files: [f] })) {
    try {
      await navigator.share({ files: [f], title: 'selfnas' });
      return; // 存了还是取消了都算走完，不再触发下载
    } catch (e) {
      if (e && e.name === 'AbortError') return; // 用户主动关掉分享面板
      // 分享真失败了，掉到下面的下载兜底
    }
  }
  const a = document.createElement('a');
  a.href = posterDataURL;
  a.download = 'selfnas-poster.png';
  document.body.appendChild(a);
  a.click();
  a.remove();
  toast(isIOS ? '没弹出分享的话：长按上面的图片，选「存储到照片」' : '已开始下载');
});


async function boot() {
  clog('文件页加载 standalone=' + (window.navigator.standalone ? 1 : 0) +
    ' dpr=' + (window.devicePixelRatio || 1) + ' w=' + window.innerWidth);
  try {
    // 四个小请求一次并发：串行的话（roots→fav→lastopen→lastdir）要排四个来回，
    // 列表迟迟不出来，页面上先空着一块，视觉上也是「闪」。谁失败都各自兜底，不互相拖死。
    const [ , , lo, ld] = await Promise.all([
      loadRoots(),
      loadFavs(),   // 先拿一次收藏，列表里的 ★ 才不会漏画
      // 「刚点开」标记：开机先取一次（刷新页面/换设备都能把标记带回来）
      api('/api/fs/lastopen').catch(() => null),
      // 上次浏览的目录：直接回到原地，不用一层层重新点进去。
      api('/api/fs/lastdir').catch(() => null),
    ]);
    if (lo && lo.data && lo.data.path) S.lastOpen = lo.data;
    // 权限是按当前登录态算的 —— 后台把盘/白名单目录收走后这里恢复不了，
    // 就回默认根，并把那条已失效的记录清掉（不然下次还是先撞一次墙）。
    let lastDir = (ld && ld.data && ld.data.root) ? ld.data : null;
    if (lastDir && !S.roots.some((x) => x.path === lastDir.root)) {
      lastDir = null;   // 整个盘 / 白名单目录都不在列表里了，记录已无意义
      api('/api/fs/lastdir', { root: '', path: '' }).catch(() => {});
    }
    if (lastDir) {
      const rt = S.roots.find((x) => x.path === lastDir.root);
      S.root = rt.path;
      S.rootName = rt.name;
      S.rootDrive = rt.drive || '';
      S.path = lastDir.path || '';
    }
    // 到这里「选哪个根」已经定了，根栏只画这一次 —— 这是消掉刷新闪烁的关键
    renderRoots();
    if (S.root) {
      const code = await loadList();
      if (code !== 0 && lastDir) {
        // 只在「确实没权限 / 目录没了」时清记录；服务端抽风（500 等）别把人家的位置弄丢
        if (code === 403 || code === 404) api('/api/fs/lastdir', { root: '', path: '' }).catch(() => {});
        S.root = S.roots[0].path;
        S.rootName = S.roots[0].name;
        S.rootDrive = S.roots[0].drive || '';
        S.path = '';
        renderRoots();
        await loadList();
        toast('上次的位置已失效，回到默认目录');
      }
    }
  } catch (e) {
    if (e.message !== '未登录') toast(e.message);
  }
}

boot();
