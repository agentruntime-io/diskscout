'use strict';
// DiskScout window. The app (Go) pushes changes over /events; this page only asks for what changed and redraws.
// Nothing here uses innerHTML with data: every path, name and chat message goes through textContent.

const $ = id => document.getElementById(id);
const fmt = n => { const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return (i ? n.toFixed(1) : n) + ' ' + u[i]; };
const num = n => Number(n).toLocaleString();
const secs = s => s < 90 ? Math.round(s) + ' s' : s < 5400 ? Math.round(s / 60) + ' min' : (s / 3600).toFixed(1) + ' h';
function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  if (props) for (const [k, v] of Object.entries(props)) { if (k === 'class') e.className = v; else if (k === 'text') e.textContent = v; else if (k.startsWith('on')) e[k] = v; else if (v !== false && v != null) e.setAttribute(k, v === true ? '' : v); }
  for (const k of kids) if (k != null) e.append(k);
  return e;
}

const SVG_NS = 'http://www.w3.org/2000/svg';
function ic(name, cls) {
  const svg = document.createElementNS(SVG_NS, 'svg'); svg.setAttribute('class', 'ic' + (cls ? ' ' + cls : ''));
  const use = document.createElementNS(SVG_NS, 'use'); use.setAttribute('href', '#i-' + name); svg.append(use); return svg;
}

class ApiError extends Error { constructor(msg, status) { super(msg); this.status = status; } }
async function api(path, body) {
  const r = await fetch(path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  let j = null; try { j = await r.json(); } catch (e) { /* not JSON */ }
  if (!r.ok) throw new ApiError((j && j.error) || ('the app answered ' + r.status), r.status);
  return j;
}
function toast(text, kind, ms = 4500) {
  const t = h('div', { class: 'toast' + (kind === 'err' ? ' err' : ''), text });
  $('toasts').append(t); setTimeout(() => t.remove(), ms);
}
async function copyText(t) {
  try { await navigator.clipboard.writeText(t); return true; } catch (e) { /* fall through */ }
  const ta = h('textarea', { style: 'position:fixed;opacity:0' }); ta.value = t; document.body.append(ta); ta.select();
  let ok = false; try { ok = document.execCommand('copy'); } catch (e) { /* ignore */ } ta.remove(); return ok;
}
const debounce = (f, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => f(...a), ms); }; };
const parentOf = p => { const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/')); return i <= 2 && /^[A-Za-z]:/.test(p) ? p.slice(0, 3) : p.slice(0, i); };

// ------------------------------------------------------------------ state
const S = {
  tab: 'space', status: { phase: 'idle' }, disk: [], cats: [], catRev: -1, settings: null, drives: [], user: '',
  path: '', node: null, crumbs: [], sel: null, selEl: null, treeRev: -1, haveTree: false, complete: true,
  hist: [''], hpos: 0, filter: '', opts: { sys: true, free: true, unacc: true }, sidemode: 'tree',
  kids: new Map(), expanded: new Set(['']), pendingSelect: '', strips: {}, renderedPath: null,
  clean: new Map(),
  groups: new Map(), epoch: -1, rev: 0, gstat: { count: 0, reclaimable: 0 }, dupsDirty: true,
  typeFilter: '', lastVerify: 0, live: true,
  approvals: [], trusted: false, chatOpen: false, chatAvail: false, chatBusy: false, chatLoaded: false,
};

// ------------------------------------------------------------------ live connection
function connect() {
  const es = new EventSource('/events');
  let lostTimer = 0;
  const on = (name, f) => es.addEventListener(name, e => f(JSON.parse(e.data)));
  on('hello', st => { applyState(st); });
  on('status', onStatus);
  on('disk', d => { S.disk = d; renderDrives(); });
  on('tree', () => { S.haveTree = true; S.kidsStale = true; scheduleTree(); });
  on('archive', d => { const p = Z.progress.get(d.zip); if (p) { p.max = d.total || 1; p.value = d.done; } });
  on('live', d => onLiveEvent(d));
  on('archive-result', d => onZipResult(d));
  on('archive-batch', d => onZipBatch(d));
  on('groups', g => { S.gstat = { count: g.count, reclaimable: g.reclaimable }; renderBadge(); if (g.epoch !== S.epoch || g.rev !== S.rev) scheduleGroups(); });
  on('categories', () => { loadCats(); });
  on('deleted', d => toast(d.mode === 'empty-recycle-bin' ? `Recycle Bin emptied (${fmt(d.bytes)} gone for good).` : `${num(d.count)} file${d.count === 1 ? '' : 's'} ${d.mode === 'trash' ? 'moved to the Recycle Bin' : 'deleted'} (${fmt(d.bytes)})`));
  on('toast', d => toast(d.text));
  on('approval', ap => { if (!S.approvals.some(a => a.id === ap.id)) S.approvals.push(ap); showApproval(); });
  on('approval-done', d => { S.approvals = S.approvals.filter(a => a.id !== d.id); showApproval(); });
  on('suggest', items => addSuggestions(items));
  on('ui', m => onUiCommand(m));
  on('chat', m => addChatMsg(m));
  on('chatstate', st => setChatState(st));
  on('chatreset', () => { $('chatlog').replaceChildren(); updateChips(); });
  es.onopen = () => { clearTimeout(lostTimer); $('conn').hidden = true; };
  es.onerror = () => { clearTimeout(lostTimer); lostTimer = setTimeout(() => { $('conn').hidden = false; }, 4000); };
}

function applyState(st) {
  S.settings = st.settings; S.drives = st.drives; S.user = st.user; S.disk = st.disk; S.trusted = st.trusted; S.chatAvail = st.chatAvailable;
  liveFor = '';   // a (re)connected window does not know what the app is watching: say it again
  if (st.catRev !== S.catRev) { S.cats = st.cats; S.catRev = st.catRev; S.dupsDirty = true; }
  S.haveTree = st.haveTree;
  applyPrefs(st.settings && st.settings.prefs);
  S.approvals = st.pending || [];
  if (st.suggestions && st.suggestions.length) addSuggestions(st.suggestions, true);
  onStatus(st.status, true);
  renderDrives(); showApproval(); setChatState({ available: st.chatAvailable, busy: S.chatBusy });
  if (st.haveTree) scheduleTree(0); else renderSpace();
  refreshStrips();
  scheduleGroups(0);
  if (S.chatOpen) loadChat();
}

function applyPrefs(p) {
  p = p || {};
  if ('sys' in p) S.opts.sys = !!p.sys;
  if ('free' in p) S.opts.free = !!p.free;
  if ('unacc' in p) S.opts.unacc = !!p.unacc;
  if ('live' in p) S.live = !!p.live;
  if (p.sidemode === 'tree' || p.sidemode === 'big') S.sidemode = p.sidemode;
  $('tSys').checked = S.opts.sys; $('tFree').checked = S.opts.free; $('tUnacc').checked = S.opts.unacc;
  if ('hiDup' in p) { $('hiDup').checked = !!p.hiDup; $('map').classList.toggle('hl', !!p.hiDup); }
  setSideMode(S.sidemode, true);
}
const savePrefs = () => api('/api/prefs', { sys: S.opts.sys, free: S.opts.free, unacc: S.opts.unacc, hiDup: $('hiDup').checked, sidemode: S.sidemode, live: S.live }).catch(() => {});

// what the map is filtered by: the text in the box, and the kind picked in the legend
const effFilter = () => [S.filter, S.typeFilter && 'type:' + S.typeFilter].filter(Boolean).join(' ');
const reportView = debounce(() => api('/api/view', { tab: S.tab, path: S.path, selected: S.sel && S.sel.path || '', filter: effFilter() }).catch(() => {}), 600);

// ------------------------------------------------------------------ drives and scan status
const typeOfItem = it => it.type || typeOf(it.ext);
function stripEl(items) {
  const strip = h('div', { class: 'dstrip' });
  const add = (size, cls, title, o = {}) => {
    const b = h('button', { class: 'seg ' + cls, type: 'button', title, tabindex: o.path ? '0' : '-1', style: `flex:${size} 1 0` });
    if (o.path) b.onclick = () => { showTab('space'); navigate(o.path); };
    strip.append(b);
  };
  const real = items.filter(i => i.kind === 'dir' || i.kind === 'file'), top = real.slice(0, 5), topSet = new Set(top);
  let rest = 0; for (const i of items) if (!topSet.has(i) && i.kind !== 'free' && i.kind !== 'unacc') rest += i.size;
  for (const it of top) add(it.size, 't-' + (it.kind === 'file' ? typeOfItem(it) : it.dom || 'other'), `${it.name} · ${fmt(it.size)}`, { path: it.path });
  if (rest > 0) add(rest, 'rest', `Everything else · ${fmt(rest)}`);
  const un = items.find(i => i.kind === 'unacc'); if (un) add(un.size, 'unacc', `Unaccounted · ${fmt(un.size)}`);
  const fr = items.find(i => i.kind === 'free'); if (fr) add(fr.size, 'free', `Free space · ${fmt(fr.size)}`);
  return strip;
}
function renderDrives() {
  const box = $('drives'); box.replaceChildren();
  for (const d of S.disk) {
    const pct = d.total ? d.free / d.total : 1, low = pct < 0.05 || d.free < 5 * 2 ** 30, warn = !low && pct < 0.12;
    const top = h('div', { class: 'dtop' }, h('span', { class: 'dl', text: d.drive }), h('span', { class: 'dv', text: fmt(d.free) }), h('span', { class: 'du', text: 'free' }));
    if (low || warn) top.append(h('span', { class: 'dbadge' }, ic('alert'), low ? 'Low space' : 'Getting full'));
    top.append(h('span', { class: 'dof', text: 'of ' + fmt(d.total) }));
    const items = S.strips[d.drive];
    box.append(h('div', { class: 'drive ' + (low ? 'low' : warn ? 'warn' : 'ok'), title: `${d.drive} ${fmt(d.free)} free of ${fmt(d.total)}` }, top,
      items && items.length ? stripEl(items) : h('div', { class: 'dmeter' }, h('i', { style: `width:${Math.round((1 - pct) * 100)}%` }))));
  }
}
let stripBusy = false, stripLast = 0;
async function refreshStrips() {   // once a drive is fully scanned, its tile shows where the space went
  if (!S.haveTree || !S.complete || stripBusy || !S.disk.length || Date.now() - stripLast < 3000) return;
  stripBusy = true; stripLast = Date.now();
  try {
    for (const d of S.disk) {
      try { const r = await api(`/api/tree?path=${encodeURIComponent(d.drive + '\\')}&depth=1&limit=40&hideSys=0&free=1&unacc=1`); S.strips[d.drive] = r.node.children || []; }
      catch (e) { delete S.strips[d.drive]; }
    }
    renderDrives();
  } finally { stripBusy = false; }
}

let prevPhase = 'idle';
function setBar(frac) {
  const bar = $('scanbar'), fill = $('scanfill');
  if (frac == null) { bar.classList.add('indet'); fill.style.width = ''; }
  else { bar.classList.remove('indet'); fill.style.width = (100 * Math.max(0, Math.min(1, frac))).toFixed(1) + '%'; }
}
function onStatus(st, quiet) {
  S.status = st;
  const busy = st.phase === 'scanning' || st.phase === 'hashing';
  $('btnScan').disabled = busy; $('btnCancel').hidden = !busy;
  const box = $('scanbox'), line = $('scanline');
  box.hidden = st.phase === 'idle';
  box.className = 'scanchip ' + (busy ? 'busy' : st.phase === 'done' ? 'done' : '');
  $('scanbar').hidden = !busy;
  if (busy && prevPhase !== 'scanning' && prevPhase !== 'hashing') { S.strips = {}; renderDrives(); }
  if (st.phase === 'scanning') {
    let pct = '', eta = '';
    if (st.expected > 0) {
      const p = Math.min(0.99, st.bytes / st.expected);
      pct = Math.floor(p * 100) + '% · ';
      if (p > 0.03 && st.elapsed > 5) eta = ' · about ' + secs(st.elapsed * (1 - p) / p) + ' left';
      setBar(p);
    } else setBar(null);
    line.textContent = `Reading folders… ${pct}${num(st.files)} files · ${fmt(st.bytes)}${eta}` + (st.current ? ' · ' + st.current : '');
  } else if (st.phase === 'hashing') {
    line.textContent = `Comparing contents… ${num(st.sizeGroupsDone)} of ${num(st.sizeGroups)} · ${fmt(st.bytesHashed)} read · ${num(st.groups)} duplicate groups so far`;
    setBar(st.sizeGroups ? st.sizeGroupsDone / st.sizeGroups : 0);
  } else if (st.source === 'import') {
    line.textContent = st.message || 'Opened a saved report';
  } else if (st.phase !== 'idle') {
    line.textContent = `${st.phase === 'cancelled' ? 'Cancelled (partial map)' : 'Scanned'} ${num(st.files)} files · ${fmt(st.bytes)} in ${secs(st.elapsed)}` + (st.errors ? ` · ${num(st.errors)} unreadable` : '');
  }
  box.title = st.current || '';
  if (!quiet && (prevPhase === 'scanning' || prevPhase === 'hashing') && !busy) {
    toast(st.phase === 'cancelled' ? 'Scan cancelled. What was read so far is shown.' : `Scan finished: ${num(st.files)} files, ${num(st.groups)} duplicate groups (${fmt(st.reclaimable)} reclaimable).`);
    if (st.phase === 'done' && st.groups) $('tabbtn-dups').animate([{ background: 'var(--raised)' }, { background: 'transparent' }], 1800);
  }
  prevPhase = st.phase;
  if (st.groups !== undefined && busy) { S.gstat = { count: st.groups, reclaimable: st.reclaimable }; }
  renderBadge(); if (S.sel) renderDetail();
}
function renderBadge() { $('dupbadge').textContent = S.gstat.count ? `${num(S.gstat.count)} · ${fmt(S.gstat.reclaimable)}` : ''; }

// ------------------------------------------------------------------ tabs and menus
function showTab(name) {
  S.tab = name;
  for (const t of ['space', 'dups']) { $('tab-' + t).hidden = t !== name; $('tabbtn-' + t).setAttribute('aria-selected', String(t === name)); }
  if (name === 'space') scheduleTree(0);
  if (name === 'dups') { renderDups(); if (Date.now() - S.lastVerify > 15000) verify(true); }
  reportView(); syncLive();
}
document.querySelectorAll('#tabs button').forEach(b => { b.onclick = () => showTab(b.dataset.tab); });

function showMenu(anchor, items) {
  const m = $('ctx'); m.replaceChildren();
  for (const it of items) {
    if (it === '-') { m.append(h('hr')); continue; }
    const b = h('button', { text: it.label, class: it.danger ? 'danger' : '' }); b.disabled = !!it.disabled;
    b.onclick = () => { hideMenu(); it.run(); }; m.append(b);
  }
  m.hidden = false;
  const el = anchor.getBoundingClientRect;
  const r = el ? anchor.getBoundingClientRect() : { left: anchor.x, bottom: anchor.y, right: anchor.x };
  const w = m.offsetWidth, hh = m.offsetHeight;
  m.style.left = Math.max(4, Math.min(innerWidth - w - 4, el ? r.right - w : r.left)) + 'px';
  m.style.top = Math.max(4, Math.min(innerHeight - hh - 4, r.bottom + 2)) + 'px';
}
function hideMenu() { $('ctx').hidden = true; }
document.addEventListener('mousedown', e => { if (!$('ctx').contains(e.target)) hideMenu(); });
window.addEventListener('blur', hideMenu);

$('btnMenu').onclick = e => { e.stopPropagation(); showMenu($('btnMenu'), [
  { label: 'Connect Claude (Desktop or terminal)…', run: openConnect },
  { label: 'Open a saved report…', run: openImport },
  { label: 'Category definitions…', run: openCats },
  { label: 'Scan errors…', run: openErrors },
  { label: 'Check the files still exist', run: () => verify(false) },
  { label: 'Check for updates…', run: openUpdate },
  '-',
  { label: 'Quit DiskScout', run: () => { api('/api/quit', {}).catch(() => {}); document.body.replaceChildren(h('p', { class: 'empty', text: 'DiskScout has quit. You can close this window.' })); } },
]); };
$('btnCancel').onclick = () => api('/api/cancel', {}).catch(e => toast(e.message, 'err'));

async function verify(silent) {
  S.lastVerify = Date.now();
  try { const r = await api('/api/verify', {}); if (!silent) toast(r.gone ? `${num(r.gone)} file${r.gone === 1 ? ' was' : 's were'} already gone and ${r.gone === 1 ? 'was' : 'were'} removed from the list.` : 'Every listed file is still there.'); }
  catch (e) { if (!silent) toast(e.message, 'err'); }
}
window.addEventListener('focus', () => { if (S.gstat.count && Date.now() - S.lastVerify > 20000) verify(true); });

// ------------------------------------------------------------------ space view
const TYPES = {
  video: ['mp4', 'mov', 'mkv', 'avi', 'webm', 'wmv', 'm4v', 'flv', 'mpg', 'mpeg'],
  audio: ['mp3', 'wav', 'flac', 'm4a', 'aac', 'ogg', 'wma', 'aiff'],
  image: ['jpg', 'jpeg', 'png', 'gif', 'webp', 'heic', 'bmp', 'tif', 'tiff', 'psd', 'svg', 'raw', 'cr2', 'nef', 'ico'],
  archive: ['zip', 'rar', '7z', 'tar', 'gz', 'tgz', 'bz2', 'xz', 'iso', 'img', 'vhd', 'vhdx', 'vmdk', 'wim', 'cab', 'dmg', 'nupkg', 'asar', 'pack'],
  doc: ['pdf', 'doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx', 'txt', 'md', 'csv', 'rtf', 'odt', 'epub'],
  code: ['js', 'ts', 'tsx', 'jsx', 'py', 'go', 'rs', 'java', 'c', 'cpp', 'h', 'cs', 'json', 'html', 'css', 'map', 'log', 'sql', 'yml', 'yaml', 'toml', 'lock', 'ipynb'],
  exe: ['exe', 'dll', 'sys', 'msi', 'node', 'so', 'dylib', 'bin', 'lib', 'pdb', 'obj', 'o', 'a'],
};
const EXT = {}; for (const [k, v] of Object.entries(TYPES)) for (const e of v) EXT[e] = k;
const typeOf = ext => EXT[ext] || 'other';
const TYPE_LABEL = { video: 'Video', archive: 'Archives & disk images', image: 'Images', doc: 'Documents', audio: 'Audio', code: 'Code & data', exe: 'Programs & libraries', other: 'Other' };

// The legend is a real filter: pick a kind and the map shows only those files, with every folder sized by the files of that
// kind inside it (the server's `type:` filter), so it works at any level, not just where single files are drawn. Each kind
// says how much of it is in the folder you are looking at.
(function legend() {
  const box = $('legend');
  for (const t of Object.keys(TYPE_LABEL)) {
    const b = h('button', { type: 'button', 'aria-pressed': 'false', 'data-kind': t }, h('i', { class: 't-' + t }), h('span', { text: TYPE_LABEL[t].split(' ')[0] }), h('small'));
    b.onclick = () => { S.typeFilter = S.typeFilter === t ? '' : t; renderLegend(); S.kids.clear(); scheduleTree(0); reportView(); };
    box.append(b);
  }
})();
function renderLegend() {
  const kinds = (S.node && S.node.kinds) || {};
  for (const b of $('legend').children) {
    const t = b.dataset.kind, v = kinds[t] || 0;
    b.setAttribute('aria-pressed', String(S.typeFilter === t));
    b.querySelector('small').textContent = v ? fmt(v) : '';
    b.classList.toggle('none', !v && S.typeFilter !== t);
    b.title = `${TYPE_LABEL[t]}${v ? ': ' + fmt(v) + ' in this folder (small files included)' : ': none in this folder'}.\nClick to show only these files; click again to show everything.`;
  }
}

// Live: the folder that is open follows the disk (the app watches it and reads it again when files in it change).
const isDrive = p => /^[A-Za-z]:\\?$/.test(p);
let liveFor = '';
function syncLive() {
  const btn = $('liveBtn'), can = !!S.path && !isDrive(S.path) && S.haveTree, active = S.live && can && S.tab === 'space';
  btn.setAttribute('aria-pressed', String(S.live)); btn.classList.toggle('on', active); btn.classList.toggle('idle', S.live && !can);
  btn.title = !S.live ? 'Live updating is off. Use Rescan to refresh a folder. Click to turn it on.'
    : can ? 'Live: this folder refreshes itself a moment after files in it change. Only the folder you have open is followed. Click to turn it off.'
      : 'Live is on, but a whole drive changes too often to follow. Open a folder to follow it.';
  const want = active ? S.path : '';
  if (want === liveFor) return;
  liveFor = want;
  api('/api/live', { on: !!want, path: want }).catch(e => { liveFor = ''; btn.classList.remove('on'); toast(e.message, 'err'); });
}
$('liveBtn').onclick = () => {
  S.live = !S.live; savePrefs(); syncLive();
  // turning it on catches up on whatever changed while it was off
  if (S.live && S.path && !isDrive(S.path) && S.tab === 'space') api('/api/rescan', { path: S.path }).catch(() => {});
};
function onLiveEvent(d) {   // the app turned it off by itself, and says why
  liveFor = ''; S.live = false; savePrefs(); syncLive(); toast(d.reason || 'Live updating was turned off.', 'err', 10000);
}
document.addEventListener('click', e => { const v = $('viewMenu'); if (v.open && !v.contains(e.target)) v.open = false; });
document.addEventListener('keydown', e => { const v = $('viewMenu'); if (e.key === 'Escape' && v.open) v.open = false; });
$('hiDup').onchange = e => { $('map').classList.toggle('hl', e.target.checked); savePrefs(); };
for (const [id, key] of [['tSys', 'sys'], ['tFree', 'free'], ['tUnacc', 'unacc']]) {
  $(id).onchange = e => { S.opts[key] = e.target.checked; S.kids.clear(); savePrefs(); scheduleTree(0); };
}
$('filter').oninput = debounce(e => { S.filter = e.target.value.trim(); S.kids.clear(); scheduleTree(0); reportView(); }, 300);

const treeQS = (path, depth, limit) => `path=${encodeURIComponent(path)}&depth=${depth}&limit=${limit}&hideSys=${S.opts.sys ? 0 : 1}&free=${S.opts.free ? 1 : 0}&unacc=${S.opts.unacc ? 1 : 0}&filter=${encodeURIComponent(effFilter())}`;

let treeBusy = false, treeAgain = false, treeTimer = 0, treeLast = 0, kidsLast = 0;
function scheduleTree(delay = 350) {
  if (S.tab !== 'space') return;
  clearTimeout(treeTimer);
  treeTimer = setTimeout(loadTree, Math.max(delay, delay === 0 ? 0 : 900 - (Date.now() - treeLast)));
}
async function loadTree() {
  if (treeBusy) { treeAgain = true; return; }
  treeBusy = true;
  try {
    const r = await api('/api/tree?' + treeQS(S.path, 4, 60));
    S.node = r.node; S.crumbs = r.crumbs; S.treeRev = r.rev; S.haveTree = true; S.complete = r.complete;
    renderFilterNote(r.filterBad || []);
    renderSpace(); refreshStrips();
  } catch (e) {
    if (e.status === 404 && S.path) { navigate('', false); }   // the folder is gone from the map: go to the top
    else if (e.status === 404) { S.node = null; S.haveTree = false; renderSpace(); }
    else toast(e.message, 'err');
  } finally { treeBusy = false; treeLast = Date.now(); if (treeAgain) { treeAgain = false; scheduleTree(100); } }
}

function renderFilterNote(bad) {
  const n = $('filterNote');
  if (!effFilter()) { n.hidden = true; return; }
  n.hidden = false;
  n.replaceChildren(S.typeFilter && !S.filter ? `Showing only ${TYPE_LABEL[S.typeFilter]} (files of 1 MB and up); folders are sized by what is inside them. Click it again to show everything.`
    : 'Showing only files of 1 MB and up that match; folders are sized by what matches inside them.');
  if (bad.length) n.append(' ', h('span', { class: 'bad', text: 'Not understood, ignored: ' + bad.join(' ') }));
}

// ---- navigation history
function navigate(path, push = true) {
  if (push && S.hist[S.hpos] !== path) { S.hist = S.hist.slice(0, S.hpos + 1); S.hist.push(path); S.hpos++; }
  S.path = path; S.sel = null; S.selEl = null; S.node = null; renderDetail(); updateNav(); loadTree(); reportView();
}
function navGo(delta) { const p = S.hpos + delta; if (p < 0 || p >= S.hist.length) return; S.hpos = p; navigate(S.hist[p], false); }
const upOne = () => { const c = S.crumbs; if (c.length > 1) navigate(c[c.length - 2].path); };
function updateNav() { $('navBack').disabled = S.hpos <= 0; $('navFwd').disabled = S.hpos >= S.hist.length - 1; $('navUp').disabled = !S.path; $('navHome').disabled = !S.path; }
$('navBack').onclick = () => navGo(-1); $('navFwd').onclick = () => navGo(1); $('navUp').onclick = upOne; $('navHome').onclick = () => navigate('');

function renderSpace() {
  const crumbs = $('crumbs'); crumbs.replaceChildren();
  S.crumbs.forEach((c, i) => {
    if (i) crumbs.append(h('span', { class: 'sep', text: '›' }));
    if (i === S.crumbs.length - 1) crumbs.append(h('span', { class: 'cur', text: c.name }));
    else crumbs.append(h('button', { text: c.name, onclick: () => navigate(c.path) }));
  });
  if (S.node) crumbs.append(h('span', { class: 'size', text: `${fmt(S.node.size)} · ${num(S.node.files)} files` + (S.node.done ? '' : ' (still reading…)') }));
  updateNav(); renderMap(); renderBig(); renderDetail(); treeSync(); renderLegend(); syncLive();
}

function showMsg(text, ...extra) { const m = $('mapmsg'); m.replaceChildren(h('div', null, h('p', { text }), ...extra)); m.hidden = false; }

function squarify(items, x, y, w, h0) {
  const out = []; if (w <= 0 || h0 <= 0 || !items.length) return out;
  const total = items.reduce((s, i) => s + i.size, 0); const scale = (w * h0) / total;
  let rest = items.slice();
  const worst = (row, area, side) => { let mx = 0, mn = Infinity; for (const i of row) { const a = i.size * scale; if (a > mx) mx = a; if (a < mn) mn = a; } const s2 = side * side; return Math.max((s2 * mx) / (area * area), (area * area) / (s2 * mn)); };
  while (rest.length) {
    const side = Math.min(w, h0); let row = [rest[0]], area = rest[0].size * scale, k = 1;
    while (k < rest.length) { const a2 = area + rest[k].size * scale; if (worst(row.concat(rest[k]), a2, side) <= worst(row, area, side)) { row.push(rest[k]); area = a2; k++; } else break; }
    const thick = area / side;
    if (w >= h0) { let cy = y; for (const i of row) { const ih = (i.size * scale) / thick; out.push({ item: i, x, y: cy, w: thick, h: ih }); cy += ih; } x += thick; w -= thick; }
    else { let cx = x; for (const i of row) { const iw = (i.size * scale) / thick; out.push({ item: i, x: cx, y, w: iw, h: thick }); cx += iw; } y += thick; h0 -= thick; }
    rest = rest.slice(row.length);
    if (w <= 0 || h0 <= 0) break;
  }
  return out;
}

const els = new Map();   // path -> block element, for the selection outline
function block(item, x, y, w, h0, idx) {
  const gap = w > 8 && h0 > 8 ? 1 : 0;   // the 2px of surface between neighbours: 1px taken from each block
  const x0 = Math.round(x), y0 = Math.round(y);
  const left = x0 + gap, top = y0 + gap, wd = Math.max(1, Math.round(x + w) - x0 - 2 * gap), ht = Math.max(1, Math.round(y + h0) - y0 - 2 * gap);
  const d = h('div', { class: `blk ${item.kind}` + (item.kind === 'file' ? ' t-' + typeOfItem(item) : item.kind === 'dir' ? ' t-' + (item.dom || 'other') : '') + (item.dup ? ' dup' : '') + (item.sys ? ' sys' : '') + (item.kind === 'dir' && item.dom && item.dom !== 'other' && item.pure >= 0.6 ? ' pure' : '') + (item.kind === 'dir' && !item.done ? ' busy' : '') + (item.path && S.clean.has(item.path) ? ' tag' : '') });
  d.style.cssText = `left:${left}px;top:${top}px;width:${wd}px;height:${ht}px`;
  if (idx !== undefined) d.style.setProperty('--i', Math.min(idx, 45));
  d._it = item; if (item.path) els.set(item.path, d);
  // a label is only worth drawing if it can show enough letters to mean something
  const room = Math.floor((wd - (item.kind === 'dir' ? 16 : 16)) / 6.9);
  if (wd >= 44 && ht >= 17 && room >= Math.min(item.name.length, 5)) {
    const l = h('div', { class: 'lbl', text: item.name });
    if ((ht >= 32 && wd >= 70 && !(item.kind === 'dir' && item.children)) || (item.kind === 'dir' && wd >= 110)) l.append(h('small', { text: fmt(item.size) }));
    d.append(l);
  }
  const kids = item.kind === 'dir' && item.children ? item.children.filter(c => c.size > 0) : [];
  if (kids.length && wd >= 84 && ht >= 60) {   // a folder opens only when its header is a small part of it
    for (const r of squarify(kids, 1, 16, wd - 2, ht - 17)) if (r.w >= 3 && r.h >= 3) d.append(block(r.item, r.x, r.y, r.w, r.h));
  } else if (item.kind === 'dir') d.classList.add('flat');
  return d;
}

function showEmpty() {
  const m = $('mapmsg');
  m.replaceChildren(h('div', null, $('tplEmpty').content.cloneNode(true), h('h3', { text: 'Where did your space go?' }),
    h('p', { text: 'Scan a drive and the map fills in while it reads. Bigger blocks are bigger folders and files, so the heavy things stand out.' }),
    h('button', { class: 'btn primary', text: 'Scan…', onclick: openScan }), document.createTextNode(' '),
    h('button', { class: 'btn', text: 'Open a saved report…', onclick: openImport })));
  m.hidden = false;
}

function renderMap() {
  const map = $('map'); els.clear(); map.replaceChildren(); $('mapmsg').hidden = true; hideTip();
  const enter = S.renderedPath !== S.path; S.renderedPath = S.path;
  map.classList.toggle('enter', enter);
  if (!S.node) { if (!S.haveTree) showEmpty(); return; }
  const items = (S.node.children || []).filter(i => i.size > 0);
  if (!items.length) { showMsg(effFilter() ? 'Nothing matches the filter here.' : S.node.done ? 'This folder is empty.' : 'Reading this folder…'); return; }
  const W = map.clientWidth, H = map.clientHeight, frag = document.createDocumentFragment(); let n = 0;
  for (const r of squarify(items, 4, 4, W - 8, H - 8)) if (r.w >= 2 && r.h >= 2) frag.append(block(r.item, r.x, r.y, r.w, r.h, n++));
  map.append(frag);
  if (S.pendingSelect && els.has(S.pendingSelect)) { select(els.get(S.pendingSelect)._it, els.get(S.pendingSelect)); S.pendingSelect = ''; }
  else if (S.sel && S.sel.path && els.has(S.sel.path)) { S.selEl = els.get(S.sel.path); S.selEl.classList.add('sel'); }
}
new ResizeObserver(() => { if (S.tab === 'space' && S.node) { clearTimeout(window._rz); window._rz = setTimeout(renderMap, 120); } }).observe($('mapwrap'));

// ---- the side panel: a folder tree you can walk, or the largest items here
function setSideMode(mode, quiet) {
  S.sidemode = mode;
  $('sideTree').setAttribute('aria-pressed', String(mode === 'tree')); $('sideBig').setAttribute('aria-pressed', String(mode === 'big'));
  $('treepane').hidden = mode !== 'tree'; $('biglist').hidden = mode !== 'big';
  if (!quiet) savePrefs();
}
$('sideTree').onclick = () => setSideMode('tree'); $('sideBig').onclick = () => setSideMode('big');

async function fetchKids(path) {
  try {
    const r = await api('/api/tree?' + treeQS(path, 1, 200));
    S.kids.set(path, (r.node.children || []).filter(c => c.kind === 'dir')); return true;
  } catch (e) { S.kids.set(path, []); return false; }
}
let treeSyncToken = 0;
async function treeSync() {
  const token = ++treeSyncToken;
  for (const c of S.crumbs) { S.expanded.add(c.path); if (!S.kids.has(c.path)) { await fetchKids(c.path); if (token !== treeSyncToken) return; } }
  if (Date.now() - kidsLast > 2500 && (S.status.phase === 'scanning' || S.status.phase === 'hashing' || S.kidsStale)) {   // keep open folders current while a scan fills them in
    kidsLast = Date.now(); S.kidsStale = false;
    await Promise.all([...S.expanded].filter(p => !S.crumbs.some(c => c.path === p)).slice(0, 30).map(fetchKids));
    if (token !== treeSyncToken) return;
  }
  renderTreePane();
}
function renderTreePane() {
  const box = $('treepane'); box.replaceChildren();
  const addRows = (path, depth, parentSize) => {
    const kids = S.kids.get(path); if (!kids) return;
    for (const k of kids) {
      const open = S.expanded.has(k.path), known = S.kids.get(k.path), leaf = !!known && known.length === 0;
      const dot = h('i', { class: 'tdot t-' + (k.dom || 'other') });
      box.append(h('div', { class: 'trow' + (k.path === S.path ? ' cur' : '') + (k.sys ? ' sysrow' : ''), role: 'treeitem', 'aria-expanded': String(open), 'data-path': k.path, style: `padding-left:${4 + depth * 14}px`, title: `${k.path}\n${fmt(k.size)}` },
        h('button', { class: 'chev' + (leaf ? ' leaf' : ''), tabindex: '-1', 'data-chev': '1' }, ic('chev-r')), dot,
        h('span', { class: 'nm', text: k.name }), h('span', { class: 'sz', text: fmt(k.size) }),
        h('span', { class: 'pct' }, h('i', { style: `width:${Math.max(2, Math.round(100 * k.size / (parentSize || k.size)))}%` }))));
      if (open) addRows(k.path, depth + 1, k.size);
    }
  };
  if (!S.kids.has('')) { box.append(h('p', { class: 'empty', text: S.haveTree ? 'Loading…' : 'Scan to see your folders.' })); return; }
  addRows('', 0, (S.kids.get('') || []).reduce((t, k) => t + k.size, 0));
  const cur = box.querySelector('.cur'); if (cur) cur.scrollIntoView({ block: 'nearest' });
}
async function toggleExpand(path) {
  if (S.expanded.has(path)) S.expanded.delete(path); else { S.expanded.add(path); if (!S.kids.has(path)) { renderTreePane(); await fetchKids(path); } }
  renderTreePane();
}
$('treepane').addEventListener('click', e => {
  const row = e.target.closest('.trow'); if (!row) return;
  if (e.target.dataset.chev) toggleExpand(row.dataset.path); else navigate(row.dataset.path);
});
$('treepane').addEventListener('keydown', e => {
  const rows = [...$('treepane').querySelectorAll('.trow')]; if (!rows.length) return;
  let i = rows.findIndex(r => r.classList.contains('cur')); const cur = rows[i];
  const go = j => { if (rows[j]) navigate(rows[j].dataset.path); };
  if (e.key === 'ArrowDown') { e.preventDefault(); go(i < 0 ? 0 : i + 1); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); go(i < 0 ? 0 : i - 1); }
  else if (e.key === 'ArrowRight' && cur) { e.preventDefault(); if (!S.expanded.has(cur.dataset.path)) toggleExpand(cur.dataset.path); else go(i + 1); }
  else if (e.key === 'ArrowLeft' && cur) { e.preventDefault(); if (S.expanded.has(cur.dataset.path) && cur.dataset.path !== S.path) toggleExpand(cur.dataset.path); else if (S.expanded.has(cur.dataset.path)) toggleExpand(cur.dataset.path); else { const p = parentOf(cur.dataset.path); navigate(/^[A-Za-z]:\\?$/.test(cur.dataset.path) ? '' : p); } }
  else if (e.key === 'Enter' && cur) { e.preventDefault(); navigate(cur.dataset.path); }
});

function renderBig() {
  const box = $('biglist'); box.replaceChildren();
  const items = S.node ? S.node.children.filter(i => i.size > 0) : [];
  const top = items[0] ? items[0].size : 1;
  for (const it of items.slice(0, 60)) {
    const b = h('button', { class: 'big' + (S.sel && S.sel.path === it.path && it.path ? ' sel' : ''), type: 'button' },
      h('div', { class: 'n' }, h('span', { text: (it.kind === 'dir' ? '▸ ' : '') + it.name }), h('span', { text: fmt(it.size) })),
      h('div', { class: 'b' }, h('i', { class: 't-' + (it.kind === 'file' ? typeOfItem(it) : it.dom || 'other'), style: `width:${Math.max(1, Math.round(100 * it.size / top))}%` })));
    b.onclick = () => select(it, it.path ? els.get(it.path) : null); b.ondblclick = () => activate(it);
    box.append(b);
  }
}

function select(it, el) {
  if (S.selEl) S.selEl.classList.remove('sel');
  S.sel = it || null; S.selEl = el || null;
  if (S.selEl) S.selEl.classList.add('sel');
  renderDetail(); renderBigSel(); reportView();
}
function renderBigSel() { document.querySelectorAll('#biglist .big').forEach((b, i) => { const it = S.node && S.node.children.filter(x => x.size > 0)[i]; b.classList.toggle('sel', !!(S.sel && it && it.path && it.path === S.sel.path)); }); }
function activate(it) { if (it.kind === 'dir') navigate(it.path); else if (it.kind === 'file') api('/api/reveal', { path: it.path }).catch(e => toast(e.message, 'err')); }

const isBin = it => it && it.kind === 'dir' && /^[A-Za-z]:\\\$Recycle\.Bin$/i.test(it.path || '');
const REAL = it => it && (it.kind === 'dir' || it.kind === 'file');
const EXPLAIN = {
  free: 'Free space on this drive: what is still available to use.',
  unacc: 'Unaccounted: Windows counts this as used, but the scan could not see it. Usually shadow copies and other protected system areas (System Volume Information), plus anything you told the scan to skip. It needs administrator rights to look closer.',
  small: 'Lumps of small things that are too small to draw one by one.',
};
function renderDetail() {
  const it = S.sel, busy = S.status.phase === 'scanning' || S.status.phase === 'hashing', real = REAL(it);
  $('dname').textContent = it ? it.name : 'Nothing selected';
  const icon = $('dicon'); icon.replaceChildren(ic(!it ? 'folder' : it.kind === 'file' ? 'file' : it.kind === 'free' ? 'check' : it.kind === 'unacc' ? 'info' : 'folder'));
  icon.className = 'dicon' + (!it ? ' none' : it.kind === 'file' ? ' t-' + typeOfItem(it) : it.kind === 'dir' ? ' t-' + (it.dom || 'other') : it.kind === 'free' ? '' : ' none');
  icon.style.setProperty('--c', it && it.kind === 'free' ? 'var(--good)' : '');
  $('dpath').textContent = !it ? 'Click a block to select it. Double-click a folder to open it, a file to show it in Explorer. Right-click for more.' : real ? it.path : (EXPLAIN[it.kind] || '');
  const meta = $('dmeta'); meta.replaceChildren();
  if (it) {
    const m = (val, text, cls) => h('span', { class: 'm' + (cls ? ' ' + cls : '') }, val != null ? h('b', { text: val }) : null, text);
    meta.append(m(fmt(it.size), ' total'));
    if (S.node && S.node.size) meta.append(m((100 * it.size / S.node.size).toFixed(1) + '%', ' of this folder'));
    if (it.kind === 'dir') meta.append(m(num(it.files), ' files'));
    if (it.kind === 'dir' && it.dom && it.pure >= 0.4 && it.dom !== 'other') meta.append(m(Math.round(it.pure * 100) + '%', ' ' + (TYPE_LABEL[it.dom] || it.dom).toLowerCase()));
    if (it.kind === 'file' && it.mtime) meta.append(m(null, 'modified ' + new Date(it.mtime / 1e6).toLocaleDateString()));
    if (it.sys) meta.append(m(null, 'managed by Windows', 'warn'));
    if (it.dup) meta.append(m(null, 'has an identical copy', 'warn'));
    if (isBin(it)) meta.append(m(null, 'takes space until emptied', 'warn'));
    if (real && S.clean.has(it.path)) meta.append(m(null, 'on your clean list'));
    if (!real && EXPLAIN[it.kind]) $('dpath').textContent = EXPLAIN[it.kind];
  }
  $('dReveal').disabled = $('dCopy').disabled = $('dTrash').disabled = $('dTag').disabled = !real;
  $('dTrash').hidden = isBin(it); $('dEmpty').hidden = !isBin(it);
  $('dRescan').disabled = !it || it.kind !== 'dir' || busy;
  $('dTag').querySelector('.bl').textContent = real && S.clean.has(it.path) ? 'Remove from clean list' : 'Add to clean list';
}
$('dReveal').onclick = () => S.sel && api('/api/reveal', { path: S.sel.path }).catch(e => toast(e.message, 'err'));
$('dCopy').onclick = async () => { if (S.sel) toast(await copyText(S.sel.path) ? 'Path copied' : 'Could not copy', 'ok', 1500); };
$('dRescan').onclick = () => rescan(S.sel);
$('dTrash').onclick = () => S.sel && confirmTrash(S.sel);
$('dTag').onclick = () => S.sel && toggleClean(S.sel);
$('dEmpty').onclick = () => S.sel && confirmEmptyBin(S.sel);

async function rescan(it) {
  try { toast('Reading ' + it.name + ' again…', 'ok', 1500); const r = await api('/api/rescan', { path: it.path }); toast(r.delta ? `${it.name} changed by ${r.delta > 0 ? '+' : '−'}${fmt(Math.abs(r.delta))}.` : `${it.name} is unchanged.`); }
  catch (e) { toast(e.message, 'err'); }
}

// hover, click and context menu on the map
const tip = $('tip');
function hideTip() { tip.hidden = true; }
$('map').addEventListener('mousemove', e => {
  const b = e.target.closest('.blk'); if (!b || !b._it) { hideTip(); return; }
  const it = b._it, pct = S.node && S.node.size ? ` (${(100 * it.size / S.node.size).toFixed(1)}%)` : '';
  tip.textContent = REAL(it) ? `${it.path}\n${fmt(it.size)}${pct}` + (it.kind !== 'file' ? ` · ${num(it.files)} files` : it.mtime ? ' · ' + new Date(it.mtime / 1e6).toLocaleDateString() : '') + (it.dup ? '\nIdentical copy exists elsewhere' : '') + (it.sys ? '\nManaged by Windows' : '') + (S.clean.has(it.path) ? '\n★ On your clean list' : '')
    : `${it.name}\n${fmt(it.size)}${pct}\n${EXPLAIN[it.kind] || ''}`;
  tip.style.whiteSpace = 'pre-wrap'; tip.style.maxWidth = '420px'; tip.hidden = false;
  tip.style.left = Math.min(innerWidth - tip.offsetWidth - 8, e.clientX + 14) + 'px';
  tip.style.top = Math.min(innerHeight - tip.offsetHeight - 8, e.clientY + 16) + 'px';
});
$('map').addEventListener('mouseleave', hideTip);
$('map').addEventListener('click', e => { const b = e.target.closest('.blk'); select(b && b._it ? b._it : null, b); });
$('map').addEventListener('dblclick', e => { const b = e.target.closest('.blk'); if (b && b._it) activate(b._it); });
$('map').addEventListener('contextmenu', e => {
  e.preventDefault(); const b = e.target.closest('.blk'); if (!b || !b._it || !REAL(b._it)) return;
  const it = b._it; select(it, b); const busy = S.status.phase === 'scanning' || S.status.phase === 'hashing';
  showMenu({ x: e.clientX, y: e.clientY }, [
    it.kind === 'dir' ? { label: 'Open this folder', run: () => navigate(it.path) } : null,
    { label: 'Show in Explorer', run: () => api('/api/reveal', { path: it.path }).catch(x => toast(x.message, 'err')) },
    { label: 'Copy path', run: async () => toast(await copyText(it.path) ? 'Path copied' : 'Could not copy', 'ok', 1500) },
    { label: S.clean.has(it.path) ? 'Remove from clean list' : 'Add to clean list', run: () => toggleClean(it) },
    it.kind === 'dir' ? { label: 'Rescan this folder', disabled: busy, run: () => rescan(it) } : null,
    '-', isBin(it) ? { label: 'Empty Recycle Bin…', danger: true, run: () => confirmEmptyBin(it) } : { label: 'Move to Recycle Bin…', danger: true, run: () => confirmTrash(it) },
  ].filter(Boolean));
});
document.addEventListener('keydown', e => {
  if (e.altKey && e.key === 'ArrowLeft') { e.preventDefault(); navGo(-1); return; }
  if (e.altKey && e.key === 'ArrowRight') { e.preventDefault(); navGo(1); return; }
  if (document.querySelector('dialog[open]') || S.tab !== 'space' || /INPUT|TEXTAREA|SELECT/.test(e.target.tagName) || e.target.id === 'treepane') return;
  if (e.key === 'Backspace') { e.preventDefault(); upOne(); }
  else if (e.key === 'Enter' && S.sel) activate(S.sel);
  else if (e.key === 'Delete' && REAL(S.sel)) isBin(S.sel) ? confirmEmptyBin(S.sel) : confirmTrash(S.sel);
  else if ((e.key === 't' || e.key === 'T') && REAL(S.sel)) toggleClean(S.sel);
  else if (e.key === 'Escape') select(null, null);
});

// a generic "are you sure"
let confirmAction = null;
function confirmDialog(title, body, okLabel, action) {
  $('kTitle').textContent = title; $('kBody').textContent = body; $('kBody').style.whiteSpace = 'pre-wrap'; $('kGo').textContent = okLabel; $('kGo').disabled = false; $('kErr').hidden = true;
  confirmAction = action; $('dlgConfirm').showModal();
}
$('kCancel').onclick = () => $('dlgConfirm').close();
$('kGo').onclick = async () => {
  $('kGo').disabled = true;
  try { await confirmAction(); $('dlgConfirm').close(); } catch (e) { $('kErr').textContent = e.message; $('kErr').hidden = false; $('kGo').disabled = false; }
};
function confirmEmptyBin(it) {
  const drive = it.path.slice(0, 2);
  confirmDialog(`Empty the Recycle Bin on ${drive}?`, `About ${fmt(it.size)} (${num(it.files)} items) will be deleted permanently. Windows cannot undo this.\n\nThis is what finally gives the space back after you moved things to the Recycle Bin.`, 'Empty Recycle Bin',
    async () => { await api('/api/empty-bin', { drive }); S.sel = null; scheduleTree(0); });
}

// moving a folder or file from the map to the Recycle Bin
let trashing = null;
function confirmTrash(it) {
  trashing = it; $('tErr').hidden = true; $('tGo').disabled = false;
  $('tTitle').textContent = it.kind === 'dir' ? 'Move this folder to the Recycle Bin?' : 'Move this file to the Recycle Bin?';
  $('tBody').textContent = `${it.path}\n${fmt(it.size)}` + (it.kind === 'dir' ? ` in ${num(it.files)} files, and everything inside it.` : '.');
  $('tBody').style.whiteSpace = 'pre-wrap'; $('dlgTrash').showModal();
}
$('tCancel').onclick = () => $('dlgTrash').close();
$('tGo').onclick = async () => {
  $('tGo').disabled = true;
  try {
    const r = await api('/api/trash', { path: trashing.path });
    if (!r.ok && !r.gone) { $('tErr').textContent = r.error; $('tErr').hidden = false; $('tGo').disabled = false; return; }
    S.clean.delete(trashing.path); renderClean();
    $('dlgTrash').close(); S.sel = null; scheduleTree(0);
  } catch (e) { $('tErr').textContent = e.message; $('tErr').hidden = false; $('tGo').disabled = false; }
};

// ------------------------------------------------------------------ the clean list
function toggleClean(it) {
  if (S.clean.has(it.path)) S.clean.delete(it.path); else S.clean.set(it.path, { path: it.path, name: it.name, size: it.size, kind: it.kind, reason: '', by: '' });
  if (els.has(it.path)) els.get(it.path).classList.toggle('tag', S.clean.has(it.path));
  renderClean(); renderDetail();
}
function addSuggestions(items, quiet) {
  let n = 0;
  for (const s of items) { if (!S.clean.has(s.path)) n++; S.clean.set(s.path, { path: s.path, name: s.name, size: s.size, kind: s.kind, reason: s.reason, by: s.by }); if (els.has(s.path)) els.get(s.path).classList.add('tag'); }
  api('/api/suggestions/ack', {}).catch(() => {});
  renderClean();
  if (!quiet && n) { toast(`${items[0].by || 'Claude'} added ${num(n)} item${n === 1 ? '' : 's'} to your clean list.`); $('btnClean').animate([{ background: 'var(--sel)' }, { background: 'transparent' }], 1800); }
}
function renderClean() {
  let total = 0; for (const c of S.clean.values()) total += c.size;
  $('cleanCount').textContent = S.clean.size ? `${num(S.clean.size)} · ${fmt(total)}` : '';
  if ($('dlgClean').open) fillClean();
}
function fillClean() {
  const box = $('cleanList'); box.replaceChildren();
  const items = [...S.clean.values()].sort((a, b) => b.size - a.size); let total = 0;
  for (const c of items) {
    total += c.size;
    box.append(h('div', { class: 'crow' },
      h('span', { class: 'p' }, h('b', { text: (c.kind === 'dir' ? '▸ ' : '') + c.name }), h('small', { text: c.path }),
        c.reason ? h('span', { class: 'why' }, h('span', { class: 'by', text: (c.by || 'Note') + ': ' }), c.reason) : null),
      h('span', { class: 's', text: fmt(c.size) }),
      h('button', { class: 'linkbtn', type: 'button', text: 'remove', onclick: () => { S.clean.delete(c.path); if (els.has(c.path)) els.get(c.path).classList.remove('tag'); renderClean(); renderDetail(); } })));
  }
  if (!items.length) box.append(h('p', { class: 'empty', text: 'Nothing on the list yet. Select things on the map and press T (or "Add to clean list"), or ask Claude.' }));
  $('cleanSum').textContent = items.length ? `${num(items.length)} item${items.length === 1 ? '' : 's'} · ${fmt(total)}. The Recycle Bin frees no space until it is emptied.` : '';
  $('cleanGo').disabled = !items.length; $('cleanClear').disabled = !items.length;
}
$('btnClean').onclick = () => { $('cleanErr').hidden = true; $('cleanProg').hidden = true; fillClean(); $('dlgClean').showModal(); };
$('cleanClose').onclick = () => $('dlgClean').close();
$('cleanClear').onclick = () => { for (const p of S.clean.keys()) if (els.has(p)) els.get(p).classList.remove('tag'); S.clean.clear(); renderClean(); renderDetail(); };
$('cleanGo').onclick = async () => {
  let total = 0; for (const c of S.clean.values()) total += c.size;
  if (!confirm(`Move ${S.clean.size} item${S.clean.size === 1 ? '' : 's'} (${fmt(total)}) to the Recycle Bin?\n\nYou can restore them from the bin until you empty it.`)) return;
  const items = [...S.clean.values()].sort((a, b) => a.path.length - b.path.length);   // folders before what is inside them
  $('cleanGo').disabled = true; $('cleanClose').disabled = true; $('cleanProg').hidden = false; $('cleanProg').max = items.length; $('cleanProg').value = 0;
  let moved = 0, freed = 0; const failed = [];
  for (let i = 0; i < items.length; i++) {
    try {
      const r = await api('/api/trash', { path: items[i].path });
      if (r.ok || r.gone) { S.clean.delete(items[i].path); if (r.ok) { moved++; freed += r.size; } } else failed.push(items[i].path + '\n    ' + r.error);
    } catch (e) { failed.push(items[i].path + '\n    ' + e.message); }
    $('cleanProg').value = i + 1;
  }
  $('cleanClose').disabled = false; renderClean(); scheduleTree(0);
  toast(`${num(moved)} item${moved === 1 ? '' : 's'} moved to the Recycle Bin (${fmt(freed)}). Empty the bin to get the space back.`);
  if (failed.length) { $('cleanErr').hidden = false; $('cleanErr').textContent = `${failed.length} not moved:\n` + failed.join('\n'); }
  else $('dlgClean').close();
};

// Claude (or anything else) moving the window
function onUiCommand(m) {
  if (m.view === 'zips') { D.view = 'zips'; Z.stale = true; }   // Claude pointing at the list of zips
  if (m.tab) showTab(m.tab);
  if (m.select) { S.pendingSelect = m.select; if (!m.path) navigate(parentOf(m.select)); }
  if (m.path) { showTab('space'); navigate(m.path); }
}

// ------------------------------------------------------------------ duplicates: data
let gBusy = false, gAgain = false, gTimer = 0, landed = false;
function scheduleGroups(delay = 500) { clearTimeout(gTimer); gTimer = setTimeout(syncGroups, delay); }
async function syncGroups() {
  if (gBusy) { gAgain = true; return; }
  gBusy = true;
  try {
    const r = await api(`/api/groups?since=${S.rev}&epoch=${S.epoch}`);
    if (r.epoch !== S.epoch) { S.groups.clear(); selected.clear(); S.epoch = r.epoch; }
    for (const g of r.groups) S.groups.set(g.id, g);
    if (r.groups.length) dirTotals.clear();
    S.rev = r.rev;
    for (const p of [...selected.keys()]) if (!pathLive(p)) selected.delete(p);
    S.dupsDirty = true;
    const st = stats(); S.gstat = { count: st.groups, reclaimable: st.reclaim }; renderBadge();
    if (!landed && !S.haveTree && st.groups) { landed = true; showTab('dups'); }   // only a saved report is loaded: that is the useful page
    else if (S.tab === 'dups') renderDups();
  } catch (e) { /* the next event retries */ } finally { gBusy = false; if (gAgain) { gAgain = false; scheduleGroups(300); } }
}
function pathLive(p) { for (const g of S.groups.values()) for (const c of g.copies) if (c.path === p && !c.gone) return true; return false; }
const live = g => g.copies.filter(c => !c.gone);
const catIdx = c => c.cat >= 0 && c.cat < S.cats.length ? c.cat : S.cats.length;
const catName = i => i < S.cats.length ? S.cats[i].name : 'Uncategorized';
const D = { cat: -1, loc: '', q: '', sort: 'wasted', limit: 150, view: 'files', dlimit: 40 };
const selected = new Map();   // path -> size
const dirTotals = new Map();  // folder -> what the duplicate search saw in it (see "by folder" below)
let shown = [];

function stats() {
  const cat = S.cats.map(() => ({ n: 0, b: 0 })).concat([{ n: 0, b: 0 }]), loc = new Map(); let reclaim = 0, files = 0, groups = 0;
  for (const g of S.groups.values()) {
    const lv = live(g); if (lv.length < 2) continue;
    groups++; reclaim += g.size * (lv.length - 1); files += lv.length - 1;
    for (const c of lv) if (!c.keep) { const s = cat[catIdx(c)]; s.n++; s.b += g.size; const l = loc.get(c.loc) || { n: 0, b: 0 }; l.n++; l.b += g.size; loc.set(c.loc, l); }
  }
  return { cat, loc, reclaim, files, groups };
}
const matches = c => !c.keep && (D.cat < 0 || catIdx(c) === D.cat) && (!D.loc || c.loc === D.loc);
function view() {
  const terms = D.q.toLowerCase().split(/\s+/).filter(Boolean), out = [];
  for (const g of S.groups.values()) {
    const lv = live(g); if (lv.length < 2) continue;
    if (terms.length) { const hay = lv.map(c => c.path.toLowerCase()).join('\n'); if (!terms.every(t => hay.includes(t))) continue; }
    const hit = lv.reduce((n, c) => n + (matches(c) ? 1 : 0), 0);
    if (hit) out.push({ g, lv, hit });
  }
  const key = { wasted: x => -x.g.size * (x.lv.length - 1), size: x => -x.g.size, copies: x => -x.lv.length, path: x => x.lv[0].path.toLowerCase() }[D.sort];
  return out.sort((a, b) => { const ka = key(a), kb = key(b); return ka < kb ? -1 : ka > kb ? 1 : 0; });
}

// ------------------------------------------------------------------ duplicates: drawing
function chips(el, label, entries, key) {
  el.replaceChildren(h('span', { class: 'label', text: label }));
  const add = (text, id, sub, risk, tipText) => {
    const b = h('button', { type: 'button', class: 'chip', 'aria-pressed': String(D[key] === id), title: tipText || null });
    if (risk) b.append(h('span', { class: 'risk ' + risk }));
    b.append(text + ' '); if (sub) b.append(h('small', { text: sub }));
    b.onclick = () => { D[key] = (id !== (key === 'cat' ? -1 : '') && D[key] === id) ? (key === 'cat' ? -1 : '') : id; D.limit = 150; renderDups(); };
    el.append(b);
  };
  add('All', key === 'cat' ? -1 : '', '');
  for (const e of entries) add(e.text, e.id, e.sub, e.risk, e.tip);
}
const RISK_TEXT = { low: 'usually safe to clean', medium: 'check first', high: 'leave alone unless you are sure', review: 'your own files: your call' };
function renderDups() {
  const st = stats();
  $('dsub').textContent = st.groups ? `${fmt(st.reclaim)} reclaimable in ${num(st.groups)} groups (${num(st.files)} redundant files). KEEP is only a suggestion; deleting only touches files listed here.`
    : (S.status.phase === 'scanning' || S.status.phase === 'hashing' ? 'Duplicates appear here as they are confirmed, biggest first.' : 'No duplicates yet. Run a scan with “Also look for duplicate files” ticked, or open a saved report from the ⋯ menu.');
  chips($('cats'), 'Category', st.cat.map((s, i) => ({ s, i })).filter(x => x.s.n).sort((a, b) => b.s.b - a.s.b).map(({ s, i }) => ({
    id: i, text: catName(i), sub: fmt(s.b), risk: i < S.cats.length ? S.cats[i].risk : '',
    tip: i < S.cats.length ? `${S.cats[i].desc}\n\nRisk: ${RISK_TEXT[S.cats[i].risk] || ''}\nRules: ${(S.cats[i].rules || []).length ? S.cats[i].rules.join('   ') : '(everything else)'}` : 'Matched no category' })), 'cat');
  chips($('locs'), 'Location', [...st.loc.entries()].sort((a, b) => b[1].b - a[1].b).slice(0, 14).map(([l, s]) => ({ id: l, text: l, sub: fmt(s.b) })), 'loc');
  shown = view();
  const byDir = D.view === 'dirs', byZip = D.view === 'zips', other = byDir || byZip;
  for (const [id, on] of [['vFiles', !other], ['vDirs', byDir], ['vZips', byZip]]) $(id).setAttribute('aria-pressed', String(on));
  $('list').hidden = other; $('dirlist').hidden = !byDir; $('ziplist').hidden = !byZip; $('sort').hidden = $('selview').hidden = $('selall').hidden = other;
  if (byDir) { $('more').hidden = true; renderDirs(shown); updateSel(); S.dupsDirty = false; return; }
  if (byZip) { $('more').hidden = true; renderZips(); updateSel(); S.dupsDirty = false; return; }
  let files = 0, bytes = 0; for (const x of shown) { files += x.hit; bytes += x.hit * x.g.size; }
  $('stats').textContent = shown.length ? `${num(shown.length)} groups · ${num(files)} duplicate files · ${fmt(bytes)} in this view` : '';
  const list = $('list'); list.replaceChildren();
  if (!shown.length) list.append(h('p', { class: 'empty', text: S.groups.size ? 'No groups match.' : '' }));
  for (const x of shown.slice(0, D.limit)) list.append(groupEl(x));
  const rest = shown.length - D.limit; $('more').hidden = rest <= 0; $('more').textContent = `Show more (${num(rest)} groups left)`;
  updateSel(); S.dupsDirty = false;
}
function rowEl(c, size) {
  const row = h('div', { class: 'row ' + (c.keep ? 'keep' : 'dup') + (selected.has(c.path) ? ' sel' : '') });
  const cb = h('input', { type: 'checkbox' }); cb.checked = selected.has(c.path); cb.dataset.path = c.path; cb.dataset.size = size; row.append(cb);
  row.append(h('span', { class: 'tag', text: c.keep ? 'KEEP' : 'dup' }));
  const i = Math.max(c.path.lastIndexOf('\\'), c.path.lastIndexOf('/'));
  row.append(h('span', { class: 'path', title: c.path }, c.path.slice(0, i + 1), h('b', { text: c.path.slice(i + 1) })));
  row.append(h('span', { class: 'meta', text: c.mtime + (c.links ? ` · +${c.links} hardlink${c.links > 1 ? 's' : ''}` : '') }));
  row.append(h('span', { class: 'act' }, h('button', { class: 'linkbtn', type: 'button', text: 'copy path', 'data-copy': c.path }), h('button', { class: 'linkbtn', type: 'button', text: 'show in Explorer', 'data-reveal': c.path })));
  return row;
}
function groupEl({ g, lv }) {
  const el = h('div', { class: 'group', 'data-id': g.id });
  el.append(h('div', { class: 'gh' }, `${fmt(g.size)} × ${lv.length} `, h('span', { text: `· reclaim ${fmt(g.size * (lv.length - 1))} · group #${g.id}` }), h('span', { class: 'badge', text: 'EVERY COPY SELECTED', hidden: true })));
  for (const c of lv) el.append(rowEl(c, g.size));
  flagGroup(el, lv); return el;
}
function flagGroup(el, lv) { const all = lv.every(c => selected.has(c.path)); el.classList.toggle('all', all); el.querySelector('.badge').hidden = !all; }
// ------------------------------------------------------------------ duplicates: by folder
// Two folders belong together when several files in one have a byte-identical twin in the other. Every pair is a group the
// scan confirmed by hashing both files in full, so what is counted here is proven matches, not a guess about similar names.
let statsBase = '';
const sepAt = p => Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/'));
const dirOf = p => p.slice(0, sepAt(p));
const baseOf = p => p.slice(sepAt(p) + 1);
function clusters(rows) {
  const m = new Map();
  for (const { g, lv } of rows) {
    const byDir = new Map();
    for (const c of lv) { const d = dirOf(c.path); if (!byDir.has(d)) byDir.set(d, c); }
    if (byDir.size < 2) continue;
    const dirs = [...byDir.keys()].sort((a, b) => a.toLowerCase() < b.toLowerCase() ? -1 : 1).slice(0, 6);   // a file with dozens of copies would drown the list
    for (let i = 0; i < dirs.length; i++) for (let j = i + 1; j < dirs.length; j++) {
      const key = dirs[i].toLowerCase() + '\n' + dirs[j].toLowerCase();
      let k = m.get(key); if (!k) m.set(key, k = { a: dirs[i], b: dirs[j], pairs: [], bytes: 0 });
      k.pairs.push({ id: g.id, size: g.size, ca: byDir.get(dirs[i]), cb: byDir.get(dirs[j]) }); k.bytes += g.size;
    }
  }
  return [...m.values()].filter(k => k.pairs.length >= 3).sort((x, y) => y.bytes - x.bytes);
}
function renderDirs(rows) {
  const box = $('dirlist'); box.replaceChildren();
  const all = clusters(rows);
  statsBase = all.length ? `${num(all.length)} pair${all.length === 1 ? ' of folders shares' : 's of folders share'} at least 3 identical files.` : '';
  $('stats').textContent = statsBase;
  if (!all.length) { box.append(h('p', { class: 'empty', text: S.groups.size ? 'No two folders share several identical files (with these filters).' : '' })); return; }
  const list = all.slice(0, D.dlimit);
  for (const k of list) box.append(clusterEl(k));
  if (all.length > list.length) box.append(h('button', { class: 'btn more', type: 'button', text: `Show more (${num(all.length - list.length)} pairs left)`, onclick: () => { D.dlimit += 40; renderDups(); } }));
  fillTotals(list);
}
function pathEl(d) { return h('span', { class: 'path', title: d }, d.slice(0, sepAt(d) + 1), h('b', { text: d.slice(sepAt(d) + 1) })); }
function clusterEl(k) {
  const el = h('div', { class: 'group cl' }); el._k = k;
  el.append(h('div', { class: 'gh' }, `${num(k.pairs.length)} identical files `, h('span', { text: `· ${fmt(k.bytes)}` }), h('span', { class: 'cov', 'data-cov': '' })));
  const btns = [];
  const refresh = () => btns.forEach(b => b.paint());
  for (const side of ['a', 'b']) {
    const L = side.toUpperCase(), pick = p => p['c' + side];
    const btn = h('button', { class: 'btn small', type: 'button' });
    btn.paint = () => { btn.textContent = k.pairs.every(p => selected.has(pick(p).path)) ? `Unselect ${L}'s ${num(k.pairs.length)}` : `Select ${L}'s ${num(k.pairs.length)} copies`; };
    btn.onclick = () => {
      const on = !k.pairs.every(p => selected.has(pick(p).path));
      for (const p of k.pairs) { if (on) selected.set(pick(p).path, p.size); else selected.delete(pick(p).path); }
      refresh(); updateSel();
    };
    btns.push(btn);
    el.append(h('div', { class: 'side' }, h('span', { class: 'tag', text: L }),
      h('div', { class: 'what' }, pathEl(k[side]), h('span', { class: 'tot', 'data-dir': k[side], 'data-n': k.pairs.length })), btn));
  }
  refresh();
  const body = h('div', { class: 'pbody' }), det = h('details', { class: 'proof' }, h('summary', { text: 'Show the proof' }), body);
  let loaded = false;
  det.addEventListener('toggle', async () => {
    if (!det.open || loaded) return;
    loaded = true; body.textContent = 'Reading the two folders…';
    try { const r = await api('/api/dirfiles', { dirs: [k.a, k.b], list: true }); fillProof(body, k, r.dirs); }
    catch (e) { loaded = false; body.textContent = e.message; }
  });
  el.append(det);
  return el;
}
async function fillTotals(list) {
  const ask = [...new Set(list.flatMap(k => [k.a, k.b]))].filter(d => !dirTotals.has(d));
  try {
    for (let i = 0; i < ask.length; i += 100) {
      const r = await api('/api/dirfiles', { dirs: ask.slice(i, i + 100), list: false });
      for (const [d, v] of Object.entries(r.dirs)) dirTotals.set(d, v);
    }
  } catch (e) { return; }
  const pct = (n, t) => Math.round(100 * Math.min(1, n / t)) + '%';
  for (const card of $('dirlist').querySelectorAll('.cl')) {
    const k = card._k, ta = dirTotals.get(k.a), tb = dirTotals.get(k.b), n = k.pairs.length;
    for (const el of card.querySelectorAll('[data-dir]')) {
      const t = el.dataset.dir === k.a ? ta : tb;
      el.textContent = !t || !t.known ? 'Totals are only kept for a scan run in this window.'
        : t.files ? `${num(t.files)} files here (${fmt(t.bytes)}). ${num(n)} of them ${n === 1 ? 'has' : 'have'} an identical twin in the other folder: ${pct(n, t.files)}.` : '';
    }
    const cov = card.querySelector('[data-cov]');
    cov.textContent = ta && tb && ta.known && ta.files && tb.files ? ` · ${pct(n, ta.files)} of A · ${pct(n, tb.files)} of B` : '';
  }
  if (statsBase && [...dirTotals.values()].some(t => t.known)) {
    $('stats').textContent = `${statsBase} Counts cover files of ${fmt((S.settings && S.settings.minDup) || 1048576)} or more, the ones the scan compared by their full contents.`;
  }
}
function fillProof(body, k, info) {
  body.replaceChildren(h('p', { class: 'pnote', text: 'Each pair holds exactly the same bytes: the scan compared the full contents of both files, and the delete step compares them again just before anything is removed. The names can differ.' }));
  const grid = h('div', { class: 'pf' }), pairs = k.pairs.slice().sort((x, y) => y.size - x.size);
  for (const p of pairs.slice(0, 300)) grid.append(h('span', { text: baseOf(p.ca.path) }), h('span', { class: 'sz', text: fmt(p.size) }), h('span', { text: baseOf(p.cb.path) }));
  body.append(h('h4', { text: `Identical in both (${num(pairs.length)})` }), grid);
  if (pairs.length > 300) body.append(h('p', { class: 'pnote', text: `… and ${num(pairs.length - 300)} more pairs.` }));
  for (const side of ['a', 'b']) {
    const t = info[k[side]]; if (!t || !t.known) { body.append(h('p', { class: 'pnote', text: 'The list of files in each folder is only kept for a scan run in this window, so what is left over cannot be shown.' })); break; }
    const mine = new Set(pairs.map(p => baseOf(p['c' + side].path).toLowerCase()));
    const only = (t.list || []).filter(f => !mine.has(f.name.toLowerCase())), L = side.toUpperCase();
    body.append(h('h4', { text: `Only in ${L} (${num(only.length)}): no identical twin in the other folder` }));
    if (!only.length) body.append(h('p', { class: 'pnote', text: `Nothing. Every file the scan checked in ${L} is also in the other folder.` }));
    else {
      const g2 = h('div', { class: 'pf one' });
      for (const f of only.slice(0, 200)) g2.append(h('span', { text: f.name }), h('span', { class: 'sz', text: fmt(f.size) }));
      body.append(g2);
      if (only.length > 200) body.append(h('p', { class: 'pnote', text: `… and ${num(only.length - 200)} more.` }));
    }
  }
}
// ------------------------------------------------------------------ duplicates: zips and the folders they were unpacked into
// A zip lists every file inside it, so "is it all in that folder?" has an exact answer: each file in the zip has a file at the
// matching path with the same bytes, and the folder holds nothing the zip does not. The server answers by reading both, and
// remembers the answer, so the window, "Compare all" and Claude all see the same results.
const Z = { list: null, known: true, stale: true, loading: false, ver: 0, drawn: '', limit: 40, filter: 'all', batch: null, progress: new Map(), cards: new Map() };
const s1 = n => n === 1 ? '' : 's';
// where a zip stands: safe (everything in it is in its folder), different, unchecked, or cannot (a RAR, or a zip that will not open)
const zipState = a => !a.readable || !a.base || a.cmpErr ? 'cannot' : a.check ? (a.check.zipInFolder ? 'safe' : 'different') : 'unchecked';
const ZORDER = { safe: 0, unchecked: 1, different: 2, cannot: 3 };
const zipAgo = t => { const m = Math.round((Date.now() - t) / 60000); return m < 1 ? 'just now' : m < 60 ? `${m} min ago` : m < 1440 ? `${Math.round(m / 60)} h ago` : `${Math.round(m / 1440)} d ago`; };
async function loadZips() {
  if (Z.loading) return; Z.loading = true;
  try { const r = await api('/api/archives', {}); Z.list = r.archives || []; Z.known = r.known; Z.stale = false; Z.ver++; }
  catch (e) { Z.list = []; Z.known = true; Z.stale = false; Z.ver++; toast(e.message, 'error'); }
  finally { Z.loading = false; }
  if (S.tab === 'dups' && D.view === 'zips') renderDups();
}
function renderZips() {
  const box = $('ziplist'), sum = $('zipsum');
  $('stats').textContent = '';
  if (Z.stale) { if (!Z.loading) loadZips(); if (!Z.list) { sum.hidden = true; box.replaceChildren(h('p', { class: 'empty', text: 'Looking inside the zip files…' })); Z.drawn = ''; return; } }
  if (!Z.known) { sum.hidden = true; Z.drawn = ''; box.replaceChildren(h('p', { class: 'empty', text: 'This needs a scan run in this window with “Also look for duplicate files” ticked: it looks at the zips that scan saw. A saved report has no such list.' })); return; }
  renderZipSummary();
  const terms = D.q.toLowerCase().split(/\s+/).filter(Boolean);
  const key = `${D.q}|${Z.limit}|${Z.filter}|${Z.ver}`;
  if (key === Z.drawn && box.childElementCount) return;   // do not wipe a comparison that is running or its answer
  Z.drawn = key; box.replaceChildren(); Z.cards.clear();
  const all = Z.list.filter(a => (!terms.length || terms.every(t => (a.zip + '\n' + (a.base || '')).toLowerCase().includes(t))) && (Z.filter === 'all' || zipState(a) === Z.filter))
    .sort((x, y) => ZORDER[zipState(x)] - ZORDER[zipState(y)] || y.size - x.size);
  if (!all.length) { box.append(h('p', { class: 'empty', text: Z.list.length ? 'No zips match.' : 'No zip was found next to, or unpacked into, a folder (in the files the last scan compared).' })); return; }
  for (const a of all.slice(0, Z.limit)) { const el = zipEl(a); Z.cards.set(a.zip, el); box.append(el); }
  if (all.length > Z.limit) box.append(h('button', { class: 'btn more', type: 'button', text: `Show more (${num(all.length - Z.limit)} zips left)`, onclick: () => { Z.limit += 40; renderDups(); } }));
}
// the top of the list: how many zips are in which state, a way to compare them all, and to move the safe ones in one go
function renderZipSummary() {
  const box = $('zipsum'), c = { safe: 0, different: 0, unchecked: 0, cannot: 0 }; let safeBytes = 0;
  for (const a of Z.list) { const s = zipState(a); c[s]++; if (s === 'safe') safeBytes += a.size; }
  box.hidden = !Z.list.length; box.replaceChildren(); if (!Z.list.length) return;
  const chips = h('div', { class: 'chips' });
  for (const [id, label, n, extra, tip] of [['all', 'All', Z.list.length, '', ''],
    ['safe', 'Safe to remove', c.safe, c.safe ? fmt(safeBytes) : '', 'Everything in the zip is in its folder, byte for byte'],
    ['different', 'Not the same', c.different, '', 'The zip holds something the folder lacks or has changed'],
    ['unchecked', 'Not compared yet', c.unchecked, '', 'Only names and sizes have been matched so far'],
    ['cannot', 'Cannot compare', c.cannot, '', 'A RAR, 7z or tar (DiskScout cannot open them), or a zip that will not open']]) {
    const b = h('button', { type: 'button', class: 'chip', 'aria-pressed': String(Z.filter === id), title: tip || null }, label + ' ', h('small', { text: String(n) + (extra ? ' · ' + extra : '') }));
    b.onclick = () => { Z.filter = id; Z.limit = 40; Z.drawn = ''; renderDups(); };
    chips.append(b);
  }
  box.append(chips);
  const act = h('div', { class: 'zsumact' }), b = Z.batch;
  if (b) {
    act.append(h('progress', { class: 'zprog', max: b.total || 1, value: b.done || 0 }), h('span', { class: 'pnote', text: `Comparing ${num(b.done)} of ${num(b.total)}…` }),
      h('button', { class: 'btn small', type: 'button', text: 'Stop', onclick: () => api('/api/archive/cancel', {}).catch(() => {}) }));
  } else {
    if (c.unchecked) act.append(h('button', { class: 'btn primary small', type: 'button', text: `Compare all ${num(c.unchecked)} not yet compared`, onclick: startCompareAll,
      title: 'Reads each zip and its folder in the background, biggest first. Safe to leave running; you can keep using the window.' }));
    if (c.safe) act.append(h('button', { class: 'btn danger small', type: 'button', text: `Move the ${num(c.safe)} safe zip${s1(c.safe)} to the Recycle Bin (${fmt(safeBytes)})…`, onclick: trashSafe }));
  }
  if (act.childElementCount) box.append(act);
  box.append(h('p', { class: 'pnote', text: 'Safe to remove means everything in the zip is also in its folder, byte for byte. The folders are never touched here.' }));
}
async function startCompareAll() {
  const items = Z.list.filter(a => zipState(a) === 'unchecked').map(a => ({ zip: a.zip, base: a.base, strip: !!a.strip }));
  if (!items.length) return;
  try { await api('/api/archive/checkall', { items }); Z.batch = { done: 0, total: items.length, running: true }; renderZipSummary(); }
  catch (e) { toast(e.message, 'err'); }
}
let zipPaint = 0;
function onZipResult(d) {   // one zip finished while "Compare all" runs
  const a = Z.list && Z.list.find(x => x.zip === d.zip); if (!a) return;
  if (d.check) { a.check = d.check; a.cmpErr = ''; } else a.cmpErr = d.error || 'it could not be compared';
  const old = Z.cards.get(d.zip);
  if (old && old.isConnected) { const n = zipEl(a); old.replaceWith(n); Z.cards.set(d.zip, n); }
  clearTimeout(zipPaint); zipPaint = setTimeout(() => { if (S.tab === 'dups' && D.view === 'zips' && Z.list) renderZipSummary(); }, 250);
}
function onZipBatch(d) {
  Z.batch = d.running ? d : null;
  if (!d.running) { Z.ver++; toast(d.cancelled ? 'Stopped. What was compared so far is kept.' : 'Every zip has been compared.'); if (S.tab === 'dups' && D.view === 'zips') { Z.drawn = ''; renderDups(); } }
  else if (S.tab === 'dups' && D.view === 'zips' && Z.list) renderZipSummary();
}
function zipEl(a) {
  const el = h('div', { class: 'group cl zc' });
  el.append(h('div', { class: 'gh' }, `${baseOf(a.zip)} `, h('span', { text: `· ${fmt(a.size)}` + (a.readable ? ` · ${num(a.entries)} file${s1(a.entries)}, ${fmt(a.bytes)} unpacked` : ` · ${a.kind}`) })));
  el.append(h('div', { class: 'side' }, h('span', { class: 'tag', text: 'ZIP' }),
    h('div', { class: 'what' }, pathEl(a.zip), a.err ? h('span', { class: 'tot', text: 'It cannot be opened here: ' + a.err }) : null)));
  if (a.base) {
    let t;
    if (!a.readable) t = `${num(a.folderFiles)} file${s1(a.folderFiles)} in it. ${a.note}`;
    else {
      t = a.check ? `${num(a.entries)} files in the zip.` : `${num(a.same)} of the zip's ${num(a.entries)} files are here with the same name and size (contents not compared yet).`;
      if (a.extras > 0) t += ` The folder also holds ${num(a.extras)} file${s1(a.extras)} that ${a.extras === 1 ? 'is' : 'are'} not in the zip.`;
      else if (a.extras === 0) t += ' Nothing else is in the folder.';
      else t += ' It was unpacked in among other files, so what else belongs there cannot be told.';
    }
    el.append(h('div', { class: 'side' }, h('span', { class: 'tag', text: 'DIR' }), h('div', { class: 'what' }, pathEl(a.root || a.base), h('span', { class: 'tot', text: t }))));
  }
  if (!a.readable || !a.base) return el;
  const out = h('div', { class: 'zres' }), prog = h('progress', { class: 'zprog', hidden: true, max: 1, value: 0 });
  const btn = h('button', { class: 'btn small', type: 'button', text: a.check ? 'Compare again' : 'Compare contents' });
  btn.onclick = async () => {
    btn.disabled = true; prog.hidden = false; prog.value = 0; Z.progress.set(a.zip, prog);
    out.textContent = `Reading ${fmt(a.bytes)}: each file in the zip is unpacked in memory and compared, byte for byte, with the file in the folder.`;
    try { const c = await api('/api/archive/check', { zip: a.zip, base: a.base, strip: !!a.strip }); a.cmpErr = ''; a.check = c; showResult(out, a, c, true); btn.textContent = 'Compare again'; renderZipSummary(); }
    catch (e) { out.textContent = e.message; }
    finally { prog.hidden = true; btn.disabled = false; Z.progress.delete(a.zip); }
  };
  el.append(h('div', { class: 'zact' }, btn, prog), out);
  if (a.cmpErr) out.textContent = 'It could not be compared: ' + a.cmpErr;
  else if (a.check) showResult(out, a, a.check, false);
  return el;
}
function nameBlock(title, nl) {
  if (!nl || !nl.n) return null;
  const kids = [h('summary', { text: `${title} (${num(nl.n)})` }), h('div', { class: 'pf names' }, ...(nl.names || []).map(x => h('span', { text: x })))];
  if (nl.n > (nl.names || []).length) kids.push(h('p', { class: 'pnote', text: `… and ${num(nl.n - nl.names.length)} more.` }));
  return h('details', { class: 'zl' }, ...kids);
}
// v is a full comparison (with the names, full=true) or the remembered short form (counts only)
function showResult(out, a, v, full) {
  const cnt = x => typeof x === 'number' ? x : (x && x.n) || 0;
  const miss = cnt(v.missing), diff = cnt(v.different), unr = cnt(v.unreadable), ext = cnt(v.extras);
  out.replaceChildren(h('p', { class: 'verdict ' + (v.zipInFolder ? 'ok' : 'warn'), text: v.verdict }),
    h('p', { class: 'pnote', text: `${num(v.identical)} of ${num(v.entries)} files in the zip are in the folder, byte for byte.` + (v.at ? ` Compared ${zipAgo(v.at)}.` : '') }));
  if (full) { for (const b of [nameBlock('In the zip, but not in the folder', v.missing), nameBlock('In both, but the contents differ', v.different),
    nameBlock('Could not be read, so not compared', v.unreadable), nameBlock('In the folder, but not in the zip', v.extras)]) if (b) out.append(b); }
  else if (miss || diff || unr || ext) {
    out.append(h('p', { class: 'pnote', text: [miss && `${num(miss)} in the zip but not in the folder`, diff && `${num(diff)} differ`, unr && `${num(unr)} could not be read`, ext && `${num(ext)} in the folder but not in the zip`].filter(Boolean).join(' · ') + '. Press Compare again to see which files.' }));
  }
  const zbtn = h('button', { class: 'btn small danger', type: 'button', text: 'Move the zip to the Recycle Bin…', disabled: !v.zipInFolder,
    title: v.zipInFolder ? 'Everything in it is in the folder' : 'Not everything in it is in the folder' });
  const fbtn = h('button', { class: 'btn small danger', type: 'button', text: 'Move the folder to the Recycle Bin…', disabled: !v.folderInZip,
    title: v.folderInZip ? 'Everything in it is in the zip' : v.extrasKnown ? 'It holds files that are not in the zip, or that differ' : 'What else is in that folder cannot be told' });
  zbtn.onclick = () => trashPart(a, 'zip', v); fbtn.onclick = () => trashPart(a, 'folder', v);
  out.append(h('div', { class: 'zact' }, zbtn, fbtn));
}
function trashPart(a, which, c) {
  const zip = which === 'zip';
  confirmDialog(zip ? 'Move the zip to the Recycle Bin?' : 'Move the folder to the Recycle Bin?',
    `${zip ? a.zip : (c.root || a.root || a.base)}\n\n${zip ? `Every file in it was compared with the folder, byte for byte, and each one is there.` : `Every file in it was compared with the zip, byte for byte, and each one is in the zip. Nothing else is in the folder.`}\n\n` +
    `The comparison is run again just before anything moves. It goes to the Recycle Bin, so you can restore it until you empty the bin. Afterwards ${zip ? 'the folder' : 'the zip'} is the only copy.`,
    'Move to the Recycle Bin',
    async () => {
      $('kBody').textContent += '\n\nChecking both again before anything moves…';
      const r = await api('/api/archive/trash', { zip: a.zip, base: a.base, strip: !!a.strip, which });
      if (!r.result.ok && !r.result.gone) throw new Error(r.result.error || 'it was not moved');
      toast(`${zip ? 'The zip' : 'The folder'} was moved to the Recycle Bin.`);
      Z.stale = true; scheduleTree(0); renderDups();
    });
}
// every zip that was found safe, in one go: each is compared again just before it moves, and any that no longer match stay
function trashSafe() {
  const safe = Z.list.filter(a => zipState(a) === 'safe'), bytes = safe.reduce((s, a) => s + a.size, 0);
  if (!safe.length) return;
  const sample = safe.slice(0, 10).map(a => `  ${baseOf(a.zip)} (${fmt(a.size)})`).join('\n') + (safe.length > 10 ? `\n  … and ${num(safe.length - 10)} more` : '');
  confirmDialog(`Move ${num(safe.length)} zip${s1(safe.length)} to the Recycle Bin?`,
    `Each of these zips was compared with the folder it was unpacked into, byte for byte, and everything in it is there. ${fmt(bytes)} in all.\n\n${sample}\n\n` +
    `Before each one moves it is compared again, and any that no longer match are left alone. They go to the Recycle Bin, so you can restore them until you empty the bin. The folders are not touched, and afterwards they are the only copy.`,
    'Move to the Recycle Bin',
    async () => {
      let moved = 0, freed = 0; const left = [];
      for (let i = 0; i < safe.length; i++) {
        const a = safe[i];
        $('kBody').textContent = `Checking and moving ${num(i + 1)} of ${num(safe.length)}…\n\n${baseOf(a.zip)}`;
        try {
          const r = await api('/api/archive/trash', { zip: a.zip, base: a.base, strip: !!a.strip, which: 'zip' });
          if (r.result.ok || r.result.gone) { moved++; freed += a.size; } else left.push(`${baseOf(a.zip)}: ${r.result.error}`);
        } catch (e) { left.push(`${baseOf(a.zip)}: ${e.message}`); }
      }
      Z.stale = true; scheduleTree(0); renderDups();
      toast(`${num(moved)} zip${s1(moved)} moved to the Recycle Bin (${fmt(freed)})` + (left.length ? `; ${num(left.length)} left alone because they no longer match.` : '.'), left.length ? 'err' : undefined, 9000);
    });
}
$('vFiles').onclick = () => { D.view = 'files'; renderDups(); };
$('vDirs').onclick = () => { D.view = 'dirs'; D.dlimit = 40; renderDups(); };
$('vZips').onclick = () => { D.view = 'zips'; Z.stale = true; Z.limit = 40; renderDups(); };

function fullySelected() { let n = 0; for (const g of S.groups.values()) { const lv = live(g); if (lv.length && lv.every(c => selected.has(c.path))) n++; } return n; }
function updateSel() {
  let bytes = 0; for (const s of selected.values()) bytes += s;
  const all = selected.size ? fullySelected() : 0, info = $('selinfo'); info.replaceChildren();
  info.append(selected.size ? `${num(selected.size)} selected · ${fmt(bytes)}` : 'Nothing selected. Tick files, or use the buttons on the right.');
  if (all) info.append(h('span', { class: 'warn', text: ` · ⚠ ${num(all)} group${all > 1 ? 's' : ''} with every copy selected` }));
  $('clear').disabled = $('del').disabled = !selected.size;
}
$('list').addEventListener('change', e => {
  const t = e.target; if (t.dataset.path === undefined) return;
  if (t.checked) selected.set(t.dataset.path, Number(t.dataset.size)); else selected.delete(t.dataset.path);
  t.closest('.row').classList.toggle('sel', t.checked);
  const el = t.closest('.group'), g = S.groups.get(Number(el.dataset.id)); flagGroup(el, live(g)); updateSel();
});
$('list').addEventListener('click', async e => {
  const t = e.target;
  if (t.dataset.copy !== undefined) { const ok = await copyText(t.dataset.copy), old = t.textContent; t.textContent = ok ? 'copied' : 'failed'; setTimeout(() => { t.textContent = old; }, 900); }
  else if (t.dataset.reveal !== undefined) api('/api/reveal', { path: t.dataset.reveal }).catch(() => { t.textContent = 'failed'; });
});
$('more').onclick = () => { D.limit += 150; renderDups(); };
$('sort').onchange = e => { D.sort = e.target.value; D.limit = 150; renderDups(); };
let qTimer; $('q').oninput = e => { clearTimeout(qTimer); qTimer = setTimeout(() => { D.q = e.target.value; D.limit = 150; renderDups(); }, 120); };
$('selview').onclick = () => { for (const { g, lv } of shown) for (const c of lv) if (matches(c)) selected.set(c.path, g.size); renderDups(); };
$('selall').onclick = () => { for (const { g, lv } of shown) for (const c of lv) selected.set(c.path, g.size); renderDups(); };
$('clear').onclick = () => { selected.clear(); renderDups(); };

// ------------------------------------------------------------------ duplicates: deleting
const dlg = $('dlgDel');
const mode = () => document.querySelector('input[name=mode]:checked').value;
function syncDlg() {
  const permanent = mode() === 'permanent'; $('typed').hidden = !permanent;
  $('dgo').disabled = (permanent && $('typedv').value !== 'DELETE') || (!$('allok').hidden && !$('allokv').checked);
  $('dgo').textContent = permanent ? 'Delete permanently' : 'Move to Recycle Bin';
}
async function runDelete() {
  const paths = [...selected.keys()], m = mode(), allowAll = !$('allok').hidden && $('allokv').checked, skipVerify = $('skipv').checked;
  dlg.querySelectorAll('input').forEach(i => { i.disabled = true; }); $('dgo').disabled = true; $('dcancel').disabled = true;
  $('dprog').hidden = false; $('dprog').max = paths.length; $('dprog').value = 0;
  $('dsum').hidden = false; $('dsum').textContent = skipVerify ? 'Working…' : 'Checking each file against the copy you keep, then removing it. Large files take a while to read.';
  let removed = 0, freed = 0, checked = 0; const failed = [];
  for (let i = 0; i < paths.length; i += 50) {
    try {
      const res = await api('/api/delete', { paths: paths.slice(i, i + 50), mode: m, allowAll, skipVerify });
      for (const r of res.results) { if (r.ok || r.gone) selected.delete(r.path); if (r.ok) { removed++; freed += r.size; if (r.verified) checked++; } else if (!r.gone) failed.push(r.path + '\n    ' + r.error); }
    } catch (err) { failed.push('(stopped) ' + err.message); break; }
    $('dprog').value = Math.min(paths.length, i + 50);
  }
  dirTotals.clear();
  $('dsum').textContent = `${num(removed)} file${removed === 1 ? '' : 's'} ${m === 'trash' ? 'moved to the Recycle Bin' : 'deleted permanently'} (${fmt(freed)})` + (failed.length ? `, ${num(failed.length)} left alone:` : '.')
    + (checked ? (checked === removed ? ' Each one was compared byte for byte with a copy that stays.' : ` ${num(checked)} of them were compared byte for byte with a copy that stays.`) : '');
  if (failed.length) { $('derr').hidden = false; $('derr').textContent = failed.slice(0, 60).join('\n') + (failed.length > 60 ? `\n… and ${failed.length - 60} more` : ''); }
  $('dgo').hidden = true; $('dcancel').disabled = false; $('dcancel').textContent = 'Close';
  await syncGroups();
}
$('del').onclick = () => {
  let bytes = 0; for (const s of selected.values()) bytes += s;
  const all = fullySelected(), n = selected.size;
  $('dtitle').textContent = `Delete ${num(n)} file${n === 1 ? '' : 's'}?`; $('dsize').textContent = `${fmt(bytes)} in total.`;
  const names = [...selected.keys()]; $('dlist').textContent = names.slice(0, 300).join('\n') + (n > 300 ? `\n… and ${n - 300} more` : '');
  $('alltxt').textContent = `${num(all)} group${all === 1 ? '' : 's'} will lose every copy of a file. I understand.`;
  $('allok').hidden = !all; $('allokv').checked = false; $('skipv').checked = false; $('typedv').value = ''; document.querySelector('input[name=mode][value=trash]').checked = true;
  dlg.querySelectorAll('input').forEach(i => { i.disabled = false; });
  for (const id of ['dprog', 'dsum', 'derr']) $(id).hidden = true;
  $('dgo').hidden = false; $('dcancel').disabled = false; $('dcancel').textContent = 'Cancel'; syncDlg(); dlg.showModal();
};
dlg.addEventListener('input', syncDlg); dlg.addEventListener('change', syncDlg);
$('dgo').onclick = runDelete; $('dcancel').onclick = () => dlg.close();

// ------------------------------------------------------------------ scan dialog
function openScan() {
  const s = S.settings || {};
  $('sRoots').value = (s.roots || []).join('\n'); $('sDupes').checked = s.findDupes !== false;
  $('sMin').value = String(s.minDup || 1048576); if (!$('sMin').value) $('sMin').value = '1048576';
  $('sWorkers').value = s.workers || 4; $('sDupEx').value = (s.dupExcludes || []).join('\n'); $('sEx').value = (s.excludes || []).join('\n');
  $('sErr').hidden = true; $('sDupOpts').hidden = !$('sDupes').checked; $('dlgScan').showModal();
}
const lines = id => $(id).value.split('\n').map(x => x.trim()).filter(Boolean);
$('btnScan').onclick = openScan;
$('sDupes').onchange = () => { $('sDupOpts').hidden = !$('sDupes').checked; };
$('dlgScan').querySelectorAll('[data-preset]').forEach(b => { b.onclick = () => {
  const p = b.dataset.preset;
  $('sRoots').value = p === 'all' ? S.drives.join('\n') : p === 'home' ? `C:\\Users\\${S.user}` : `C:\\Users\\${S.user}\\Downloads`;
}; });
$('sCancel').onclick = () => $('dlgScan').close();
$('sGo').onclick = async () => {
  const cfg = { roots: lines('sRoots'), findDupes: $('sDupes').checked, minDup: Number($('sMin').value), workers: Number($('sWorkers').value) || 4, dupExcludes: lines('sDupEx'), excludes: lines('sEx') };
  if (S.gstat.count && !confirm(`A new scan replaces the ${num(S.gstat.count)} duplicate groups shown now (${fmt(S.gstat.reclaimable)}). Nothing on disk changes. If they came from a saved report you can open it again from the ⋯ menu.\n\nStart the scan?`)) return;
  try { await api('/api/scan', cfg); $('dlgScan').close(); showTab('space'); S.kids.clear(); navigate('', false); S.hist = ['']; S.hpos = 0; renderSpace(); }
  catch (e) { $('sErr').textContent = e.message; $('sErr').hidden = false; }
};

// ------------------------------------------------------------------ category definitions
async function loadCats() { try { const r = await api('/api/categories'); S.cats = r.cats; S.catRev = r.rev; S.dupsDirty = true; if (S.tab === 'dups') renderDups(); scheduleGroups(0); } catch (e) { /* keep what we have */ } }
function catCard(c, stat) {
  const card = h('div', { class: 'cat' });
  const risk = h('select'); for (const [v, t] of Object.entries({ low: 'Usually safe', medium: 'Check first', high: 'Leave alone unless sure', review: 'Your files: your call' })) risk.append(h('option', { value: v, text: t }));
  risk.value = c.risk || 'review';
  const name = h('input', { type: 'text', value: c.name, 'aria-label': 'Category name' });
  const rules = h('textarea', { rows: 4, spellcheck: 'false', placeholder: 'No rules: matches everything that is left' }); rules.value = (c.rules || []).join('\n');
  const desc = h('textarea', { class: 'desc', rows: 4 }); desc.value = c.desc || '';
  const up = h('button', { class: 'btn small', type: 'button', text: '↑', title: 'Move up (checked earlier)', onclick: () => card.previousElementSibling && card.parentNode.insertBefore(card, card.previousElementSibling) });
  const dn = h('button', { class: 'btn small', type: 'button', text: '↓', title: 'Move down', onclick: () => card.nextElementSibling && card.parentNode.insertBefore(card.nextElementSibling, card) });
  const rm = h('button', { class: 'btn small', type: 'button', text: 'Delete', onclick: () => card.remove() });
  card.append(h('div', { class: 'top' }, h('span', { class: 'chip risk ' + risk.value, style: 'width:10px;height:10px;padding:0' }), name, risk, up, dn, rm),
    h('div', { class: 'stat', text: stat }),
    h('div', { class: 'cols' }, h('div', null, h('b', { text: 'Definition (what this means)' }), desc), h('div', null, h('b', { text: 'Rules (one per line)' }), rules)));
  card._id = c.id; card._get = () => ({ id: card._id, name: name.value, risk: risk.value, desc: desc.value, rules: rules.value.split('\n').map(x => x.trim()).filter(Boolean) });
  return card;
}
function fillCats(cats) {
  const st = stats(), box = $('catlist'); box.replaceChildren();
  cats.forEach((c, i) => { const s = st.cat[i]; box.append(catCard(c, s && s.n ? `${num(s.n)} duplicate files · ${fmt(s.b)} reclaimable right now` : 'No duplicates in this category right now')); });
}
async function openCats() { $('cErr').hidden = true; await loadCats(); fillCats(S.cats); $('dlgCats').showModal(); }
$('btnDefs').onclick = openCats;
$('cAdd').onclick = () => { $('catlist').append(catCard({ id: '', name: 'New category', risk: 'review', desc: '', rules: [] }, 'New: add rules, then Save')); $('catlist').lastChild.scrollIntoView({ block: 'center' }); };
$('cReset').onclick = async () => { if (!confirm('Put the original category definitions back? Your edits are lost.')) return; try { const r = await api('/api/categories/reset', {}); S.cats = r.cats; S.catRev = r.rev; fillCats(S.cats); toast('Categories reset'); scheduleGroups(0); } catch (e) { $('cErr').textContent = e.message; $('cErr').hidden = false; } };
$('cSave').onclick = async () => {
  const cats = [...$('catlist').children].map(c => c._get());
  try { const r = await api('/api/categories', cats); S.cats = r.cats; S.catRev = r.rev; fillCats(S.cats); $('cErr').hidden = true; toast('Categories saved: the groups were re-sorted'); scheduleGroups(0); }
  catch (e) { $('cErr').textContent = e.message; $('cErr').hidden = false; }
};
$('cClose').onclick = () => $('dlgCats').close();

// ------------------------------------------------------------------ the smaller dialogs
async function openImport() {
  $('iErr').hidden = true; $('dlgImport').showModal();
  try {
    const found = await api('/api/reports'), box = $('iFoundList'); box.replaceChildren();
    $('iFound').hidden = !found.length;
    for (const r of found.slice(0, 6)) box.append(h('button', { class: 'big', type: 'button', onclick: () => { $('iPath').value = r.path; $('iGo').focus(); } },
      h('div', { class: 'n' }, h('span', { text: r.path }), h('span', { text: r.modified + ' · ' + fmt(r.size) }))));
  } catch (e) { $('iFound').hidden = true; }
}
$('iCancel').onclick = () => $('dlgImport').close();
$('iGo').onclick = async () => {
  try { const r = await api('/api/import', { path: $('iPath').value }); $('dlgImport').close(); toast(`Opened ${num(r.groups)} duplicate groups.`); showTab('dups'); }
  catch (e) { $('iErr').textContent = e.message; $('iErr').hidden = false; }
};
$('btnPrefer').onclick = () => { $('pText').value = ((S.settings && S.settings.prefer) || []).join('\n'); $('dlgPrefer').showModal(); };
$('pCancel').onclick = () => $('dlgPrefer').close();
$('pGo').onclick = async () => { const prefer = lines('pText'); try { await api('/api/prefer', { prefer }); S.settings.prefer = prefer; $('dlgPrefer').close(); toast('Saved. It applies to the next scan.'); } catch (e) { toast(e.message, 'err'); } };
async function openErrors() { try { const errs = await api('/api/errors'); $('eList').textContent = errs.length ? errs.map(e => e.path + '\n    ' + e.error).join('\n') : 'Nothing: every folder and file could be read.'; $('dlgErrors').showModal(); } catch (e) { toast(e.message, 'err'); } }
$('eClose').onclick = () => $('dlgErrors').close();

// ------------------------------------------------------------------ Claude: approving what it asks to remove
function showApproval() {
  const ap = S.approvals[0], d = $('dlgApprove');
  document.title = ap ? 'DiskScout · Claude is asking…' : 'DiskScout';
  if (!ap) { if (d.open) d.close(); return; }
  if (d.dataset.id === ap.id && d.open) return;
  d.dataset.id = ap.id;
  const cmd = ap.kind === 'command', list = $('aList'); list.replaceChildren();
  $('aReason').textContent = ap.reason ? `Why: ${ap.reason}` : 'No reason was given.';
  $('aTrustRow').hidden = cmd; $('aTrust').checked = false;
  $('aHint').textContent = cmd ? 'It runs once, with no shell. Commands are never trusted automatically: you will be asked each time.' : 'They go to the Recycle Bin, so you can restore them until you empty it.';
  $('aOk').textContent = cmd ? 'Run it' : 'Approve';
  if (cmd) {
    $('aTitle').textContent = `${ap.actor} wants to run a command`;
    const kind = { cleanup: 'Cleanup: this deletes things', other: 'Unlisted command: it could do anything' }[ap.class] || ap.class;
    list.append(h('div', { class: 'cmdbox' }, h('span', { class: 'cmdclass ' + ap.class, text: kind }), h('pre', { class: 'cmdline', text: ap.command }), ap.note ? h('p', { class: 'cmdnote', text: ap.note }) : null));
    $('aTotal').textContent = 'Nothing runs until you press Run it.';
  } else {
    $('aTitle').textContent = `${ap.actor} wants to move ${num(ap.items.length)} item${ap.items.length === 1 ? '' : 's'} to the Recycle Bin`;
    for (const it of ap.items.slice(0, 200)) list.append(h('div', { class: 'arow' }, h('span', { class: 'p' }, h('b', { text: (it.kind === 'dir' ? '▸ ' : '') + it.name }), h('small', { text: it.path })), h('span', { class: 's', text: fmt(it.size) })));
    if (ap.items.length > 200) list.append(h('div', { class: 'arow', text: `… and ${ap.items.length - 200} more` }));
    $('aTotal').textContent = `${fmt(ap.total)} in total. Nothing happens until you press Approve.`;
  }
  if (!d.open) d.showModal();
  window.focus();
}
$('dlgApprove').addEventListener('cancel', e => e.preventDefault());   // Esc must not silently leave Claude waiting: use the buttons
async function answerApproval(approve) {
  const ap = S.approvals[0]; if (!ap) return;
  try { await api('/api/approval', { id: ap.id, approve, trust: approve && $('aTrust').checked }); }
  catch (e) { toast(e.message, 'err'); }
  S.approvals = S.approvals.filter(a => a.id !== ap.id); showApproval();
}
$('aOk').onclick = () => answerApproval(true); $('aDeny').onclick = () => answerApproval(false);

// ------------------------------------------------------------------ Claude: the chat panel
const CHIPS = ['What is using the most space?', 'Which duplicates are safe to delete?', 'Find big old files I can remove', 'What is unaccounted space on my drive?'];
function openChat() { S.chatOpen = true; document.body.classList.add('chat-open'); $('chat').hidden = false; loadChat(); setTimeout(() => $('chatin').focus(), 50); setTimeout(renderMap, 120); }
function closeChat() { S.chatOpen = false; document.body.classList.remove('chat-open'); $('chat').hidden = true; setTimeout(renderMap, 120); }
$('btnChat').onclick = () => S.chatOpen ? closeChat() : openChat();
$('chatClose').onclick = closeChat;
$('chatWide').onclick = () => { document.body.classList.toggle('chat-wide'); setTimeout(renderMap, 150); };
$('chatConnect').onclick = openConnect;
$('chatNew').onclick = () => api('/api/chat/reset', {}).catch(e => toast(e.message, 'err'));

async function loadChat() {
  try {
    const r = await api('/api/chat/history'); $('chatlog').replaceChildren(); r.messages.forEach(m => appendChat(m)); S.chatLoaded = true; setChatState(r.state);
  } catch (e) { /* keep what is shown */ }
  updateChips(); scrollChat(true);
}
function updateChips() {
  const box = $('chatchips'); box.replaceChildren();
  if (!S.chatAvail || $('chatlog').querySelector('.msg')) return;
  for (const c of CHIPS) box.append(h('button', { type: 'button', text: c, onclick: () => sendChat(c) }));
}
function setChatState(st) {
  S.chatAvail = st.available; S.chatBusy = !!st.busy;
  $('chatsend').hidden = S.chatBusy; $('chatstop').hidden = !S.chatBusy;
  $('chatState').textContent = S.chatBusy ? 'Working…' : 'Sees your map · asks before deleting';
  const note = $('chatnote'), dead = !st.available;
  note.hidden = !dead; $('chatin').disabled = dead; $('chatsend').disabled = dead;
  if (dead) { note.replaceChildren('Claude Code was not found on this computer, so the chat panel cannot start. You can still use Claude Desktop (or a terminal session) with DiskScout: ', h('button', { class: 'linkbtn', type: 'button', text: 'Connect…', onclick: openConnect })); }
  const t = $('chatlog').querySelector('.typing'); if (t) t.remove();
  if (S.chatBusy) $('chatlog').append(h('div', { class: 'typing', text: 'Claude is working' }));
  updateChips();
}
function scrollChat(force) { const l = $('chatlog'); if (force || l.scrollHeight - l.scrollTop - l.clientHeight < 140) l.scrollTop = l.scrollHeight; }

// Chat text: bold, `code`, lists, headings and tables. Every absolute path becomes a link that shows it in Explorer
// (shift+click: on the map) and every size is coloured by how big it is.
const PATH_START = /^[A-Za-z]:\\/;
const SIZE_ONLY = /^(\d[\d,]*(?:\.\d+)?)\s?(TB|GB|MB|KB)$/;
const TOKEN = String.raw`(\*\*[^*\n]+\*\*)|` + '`([^`\\n]+)`' + String.raw`|([A-Za-z]:\\[^\s|` + '`' + String.raw`*<>"?]+)|(\d[\d,]*(?:\.\d+)?)\s?(TB|GB|MB|KB)\b`;
function baseName(p) { const t = p.replace(/[\\/]+$/, ''); return /^[A-Za-z]:$/.test(t) ? t + '\\' : (t.split(/[\\/]/).pop() || p); }
function pathLink(path, label) {
  const a = h('a', { class: 'plink', href: '#', title: path + '\nClick: show in Explorer · Shift+click: show on the map', text: label || baseName(path) });
  a.dataset.path = path; return a;
}
function sizeSpan(text, n, unit) {
  const gb = parseFloat(n.replace(/,/g, '')) * { TB: 1024, GB: 1, MB: 1 / 1024, KB: 1 / 1048576 }[unit];
  return h('span', { class: 'sz sz-' + (gb >= 100 ? 'xl' : gb >= 10 ? 'l' : gb >= 1 ? 'm' : gb >= 0.1 ? 's' : 'xs'), text });
}
function inline(parent, text) {
  const rx = new RegExp(TOKEN, 'g');            // its own instance: bold text is parsed recursively
  let last = 0, m;
  while ((m = rx.exec(text))) {
    if (m.index > last) parent.append(text.slice(last, m.index));
    if (m[1]) { const b = h('b'); inline(b, m[1].slice(2, -2)); parent.append(b); }
    else if (m[2] !== undefined) {
      const sz = SIZE_ONLY.exec(m[2]);
      parent.append(PATH_START.test(m[2]) ? pathLink(m[2]) : sz ? sizeSpan(m[2], sz[1], sz[2]) : h('code', { text: m[2] }));
    } else if (m[3]) {
      let p = m[3], tail = ''; const t = /[.,;:!)\]']+$/.exec(p);
      if (t) { tail = t[0]; p = p.slice(0, -tail.length); }
      parent.append(pathLink(p)); if (tail) parent.append(tail);
    } else parent.append(sizeSpan(m[0], m[4], m[5]));
    last = m.index + m[0].length;
  }
  if (last < text.length) parent.append(text.slice(last));
}
const SEP_ROW = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
const splitRow = line => { let s = line.trim(); if (s.startsWith('|')) s = s.slice(1); if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1); return s.split(/(?<!\\)\|/).map(c => c.trim().replace(/\\\|/g, '|')); };
const NUMERIC = /^\**[~≈]?\s?[\d.,]+\s?(TB|GB|MB|KB|B|%|files?)?\**$/i;
function renderTable(head, aligns, rows) {
  const cell = (tag, text, i) => {
    const c = h(tag), plain = text.replace(/`/g, '');
    if (aligns[i] === 'right' || (aligns[i] !== 'left' && NUMERIC.test(plain))) c.className = 'num'; else if (aligns[i] === 'center') c.className = 'ctr';
    inline(c, text); if (tag === 'td') c.setAttribute('data-label', (head[i] || '').replace(/`/g, '')); return c;
  };
  const table = h('table', null,
    h('thead', null, h('tr', null, ...head.map((t, i) => cell('th', t, i)))),
    h('tbody', null, ...rows.map(r => h('tr', null, ...head.map((_, i) => cell('td', r[i] || '', i))))));
  return h('div', { class: 'mdtable' }, table);
}
function renderMD(text) {
  const frag = document.createDocumentFragment(), lines = text.split('\n'); let list = null, kind = '';
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trimEnd(); let m;
    if (line.includes('|') && i + 1 < lines.length && SEP_ROW.test(lines[i + 1]) && lines[i + 1].includes('-')) {
      const head = splitRow(line), aligns = splitRow(lines[i + 1]).map(c => c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : c.startsWith(':') ? 'left' : '');
      const rows = []; let j = i + 2;
      while (j < lines.length && lines[j].includes('|') && lines[j].trim()) { rows.push(splitRow(lines[j])); j++; }
      frag.append(renderTable(head, aligns, rows)); list = null; i = j - 1; continue;
    }
    if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) { list = null; frag.append(h('hr')); }
    else if ((m = /^\s*[-*•]\s+(.*)$/.exec(line))) { if (!list || kind !== 'ul') { list = h('ul'); kind = 'ul'; frag.append(list); } const li = h('li'); inline(li, m[1]); list.append(li); }
    else if ((m = /^\s*\d+[.)]\s+(.*)$/.exec(line))) { if (!list || kind !== 'ol') { list = h('ol'); kind = 'ol'; frag.append(list); } const li = h('li'); inline(li, m[1]); list.append(li); }
    else if (!line.trim()) list = null;
    else { list = null; const p = h('p'); const hm = /^#{1,4}\s+(.*)$/.exec(line); if (hm) { p.className = 'mdh'; inline(p, hm[1]); } else inline(p, line); frag.append(p); }
  }
  return frag;
}
function appendChat(m) {
  const typing = $('chatlog').querySelector('.typing'); if (typing) typing.remove();
  let el;
  if (m.role === 'assistant') { el = h('div', { class: 'msg assistant' }); el.append(renderMD(m.text)); if (el.querySelector('table')) el.classList.add('wide'); }
  else if (m.role === 'tool') el = h('div', { class: 'msg tool', text: '⚙ ' + m.tool + (m.text && m.text !== '{}' ? ' ' + m.text : '') });
  else el = h('div', { class: 'msg ' + m.role, text: m.text });
  $('chatlog').append(el);
  if (S.chatBusy) $('chatlog').append(h('div', { class: 'typing', text: 'Claude is working' }));
}
function addChatMsg(m) { appendChat(m); updateChips(); scrollChat(m.role === 'user'); }
async function sendChat(text) {
  text = (text || '').trim(); if (!text) return;
  try { await api('/api/chat/send', { text }); $('chatin').value = ''; $('chatin').style.height = 'auto'; } catch (e) { addChatMsg({ role: 'error', text: e.message }); }
}
$('chatform').onsubmit = e => { e.preventDefault(); sendChat($('chatin').value); };
$('chatin').addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendChat($('chatin').value); } });
$('chatlog').addEventListener('click', e => {
  const a = e.target.closest('a.plink'); if (!a) return;
  e.preventDefault(); const p = a.dataset.path;
  if (e.shiftKey) { showTab('space'); S.pendingSelect = p; navigate(parentOf(p)); }
  else api('/api/reveal', { path: p }).catch(err => toast(err.message, 'err'));
});
$('chatin').addEventListener('input', e => { const t = e.target; t.style.height = 'auto'; t.style.height = Math.min(t.scrollHeight, 140) + 'px'; });
$('chatstop').onclick = () => api('/api/chat/stop', {}).catch(e => toast(e.message, 'err'));

// ------------------------------------------------------------------ Claude: connecting Desktop and the terminal
async function openConnect() { $('connectCards').replaceChildren(h('p', { class: 'mut', text: 'Looking for Claude…' })); $('dlgConnect').showModal(); await fillConnect(); }
async function fillConnect(info) {
  try { info = info || await api('/api/claude'); } catch (e) { $('connectCards').replaceChildren(h('p', { class: 'err', text: e.message })); return; }
  const box = $('connectCards'); box.replaceChildren();
  const status = t => h('span', { class: 'st ' + (t.stale ? 'stale' : t.connected ? 'ok' : 'no'), text: t.stale ? 'points at another copy' : t.connected ? 'connected' : t.found ? 'not connected' : 'not found' });
  const act = async (label, target, fn) => { try { const r = await fn(); toast(label); await fillConnect(r.info); } catch (e) { toast(e.message, 'err', 9000); } };

  const dsk = h('div', { class: 'ccard' }, h('h3', null, 'Claude Desktop', status(info.desktop)),
    h('p', { text: info.desktop.found ? 'Adds DiskScout to Desktop\'s tools. Desktop then starts DiskScout itself when you ask about your disk. Quit Claude Desktop fully (tray icon) and reopen it afterwards.' : 'Claude Desktop does not seem to be installed. If it is, paste the snippet below into its config file under Settings > Developer > Edit Config.' }));
  const drow = h('div', { class: 'row' });
  if (info.desktop.found) drow.append(h('button', { class: 'btn primary', type: 'button', text: info.desktop.connected && !info.desktop.stale ? 'Reconnect' : 'Connect to Claude Desktop', onclick: () => act('Connected. Restart Claude Desktop to see it.', 'desktop', () => api('/api/claude/connect', { target: 'desktop', path: info.desktop.where })) }));
  if (info.desktop.connected) drow.append(h('button', { class: 'btn', type: 'button', text: 'Remove', onclick: () => act('Removed from Claude Desktop.', 'desktop', () => api('/api/claude/disconnect', { target: 'desktop', path: info.desktop.where })) }));
  dsk.append(drow, h('details', null, h('summary', { text: 'Do it by hand' }), h('pre', { text: info.snippet }), h('button', { class: 'btn small', type: 'button', text: 'Copy snippet', onclick: async () => toast(await copyText(info.snippet) ? 'Copied' : 'Could not copy', 'ok', 1500) })));
  if (info.desktop.where) dsk.append(h('p', { class: 'mut', text: info.desktop.where }));

  const cli = h('div', { class: 'ccard' }, h('h3', null, 'Claude Code (terminal)', status(info.cli)),
    h('p', { text: info.cli.found ? 'Adds DiskScout for every Claude Code session you start. Then just ask it about your disk.' : 'Claude Code was not found. Install it from claude.com/claude-code, then come back.' }));
  const crow = h('div', { class: 'row' });
  if (info.cli.found) crow.append(h('button', { class: 'btn primary', type: 'button', text: info.cli.connected && !info.cli.stale ? 'Reconnect' : 'Connect to Claude Code', onclick: () => act('Connected. New Claude Code sessions will have DiskScout.', 'cli', () => api('/api/claude/connect', { target: 'cli' })) }));
  if (info.cli.connected) crow.append(h('button', { class: 'btn', type: 'button', text: 'Remove', onclick: () => act('Removed from Claude Code.', 'cli', () => api('/api/claude/disconnect', { target: 'cli' })) }));
  cli.append(crow, h('details', null, h('summary', { text: 'Do it by hand' }), h('pre', { text: info.command }), h('button', { class: 'btn small', type: 'button', text: 'Copy command', onclick: async () => toast(await copyText(info.command) ? 'Copied' : 'Could not copy', 'ok', 1500) })));

  const chat = h('div', { class: 'ccard' }, h('h3', null, 'Chat panel in this window', h('span', { class: 'st ' + (info.cli.found ? 'ok' : 'no'), text: info.cli.found ? 'ready' : 'needs Claude Code' })),
    h('p', { text: info.cli.found ? 'The "Ask Claude" button uses your own Claude Code, so nothing needs setting up. It can only use DiskScout\'s tools: no files and no shell. Anything that deletes waits for your Approve.' : 'The chat panel runs your installed Claude Code. Without it, use Claude Desktop above.' }));
  box.append(chat, dsk, cli, h('p', { class: 'hint', text: 'Whichever you use, Claude can look at your map and duplicates, point at things in this window and add suggestions to your clean list. Anything it asks to remove waits for your Approve here, and goes to the Recycle Bin.' }));
}
$('connClose').onclick = () => $('dlgConnect').close();

// ------------------------------------------------------------------ check for updates
// The one thing here that leaves the computer, and only because the user pressed the menu item: it asks GitHub for the newest
// release's number. Nothing is downloaded or installed, and nothing checks by itself.
async function openUpdate() {
  $('uBody').textContent = 'Asking GitHub for the newest version…'; $('uHow').hidden = true; $('uErr').hidden = true; $('uOpen').hidden = true;
  $('dlgUpdate').showModal();
  try {
    const r = await api('/api/update/check', {});
    $('uBody').textContent = r.newer ? `Version ${r.latest} is available. You have ${r.current}.`
      : r.latest === r.current ? `You are up to date: ${r.current} is the newest version.`
      : `You have ${r.current}, which is newer than the latest release (${r.latest}).`;
    $('uHow').hidden = $('uOpen').hidden = !r.newer;
  } catch (e) { $('uBody').textContent = 'Could not check for updates.'; $('uErr').textContent = e.message; $('uErr').hidden = false; }
}
$('uClose').onclick = () => $('dlgUpdate').close();
$('uOpen').onclick = () => api('/api/update/open', {}).catch(e => toast(e.message, 'err'));

// ------------------------------------------------------------------ go
connect();
api('/api/state').then(applyState).catch(e => toast(e.message, 'err'));
renderSpace();
