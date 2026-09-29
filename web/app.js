const ADHOC = '_adhoc';
const DIMS = ['machine', 'run', 'project'];
const POLL_MS = 2000;
const AGE_MS = 30000;

let state = readState();
let board = null;
let boardTag = '';
let detail = null; // SessionDetail, or {missing: true}
let detailTag = '';
let detailFor = '';
let offset = 0; // server clock minus browser clock
let live = false;
let updatedAt = null;
let flash = '';

// ---------- DOM helpers (text only ever lands via text nodes) ----------

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false || kid === '') continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function dotJoin(parts) {
  const out = [];
  for (const p of parts) {
    if (!p) continue;
    if (out.length) out.push(' · ');
    out.push(p);
  }
  return out;
}

const $ = (sel) => document.querySelector(sel);

// ---------- time ----------

const nowMs = () => Date.now() + offset;
const pad = (n) => String(n).padStart(2, '0');

function hhmm(ms) {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function hhmmss(d) {
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function age(ms) {
  const m = Math.floor(Math.max(0, ms) / 60000);
  if (m < 1) return '<1 min';
  if (m < 60) return `${m} min`;
  const hrs = Math.floor(m / 60);
  if (hrs < 24) return `${hrs} h`;
  return `${Math.floor(hrs / 24)} d`;
}

function duration(ms) {
  const m = Math.floor(Math.max(0, ms) / 60000);
  const hrs = Math.floor(m / 60);
  return hrs ? `${hrs} h ${m % 60} min` : `${m} min`;
}

// ---------- URL state ----------

function readState() {
  const p = new URLSearchParams(location.search);
  const s = {};
  for (const k of [...DIMS, 'session']) s[k] = p.get(k) || '';
  return s;
}

function urlFor(s) {
  const p = new URLSearchParams();
  for (const k of [...DIMS, 'session']) if (s[k]) p.set(k, s[k]);
  const q = p.toString();
  return q ? `?${q}` : location.pathname;
}

function go(next) {
  const sessionChanged = next.session !== state.session;
  state = next;
  history.pushState(null, '', urlFor(state));
  if (sessionChanged) resetDetail();
  render();
  if (sessionChanged && state.session) refresh();
}

function resetDetail() {
  detail = null;
  detailTag = '';
  detailFor = '';
}

window.addEventListener('popstate', () => {
  const next = readState();
  if (next.session !== state.session) resetDetail();
  state = next;
  render();
  refresh();
});

function navLink(next, attrs, ...kids) {
  return h('a', {
    ...attrs,
    href: urlFor(next),
    onclick: (e) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      go(next);
    },
  }, ...kids);
}

// ---------- filtering ----------

const repoName = (repo) => (repo || '').split('/').pop();
const hasOwner = (repo) => (repo || '').includes('/');

function matches(machine, run, project) {
  if (state.machine && machine !== state.machine) return false;
  if (state.run === ADHOC ? run !== '' : state.run && run !== state.run) return false;
  if (state.project && project !== state.project) return false;
  return true;
}

const ctxMatches = (c) => matches(c.machine, c.run, c.project);
const itemMatches = (i) => matches(i.ctx.machine, i.run, repoName(i.repo));

function chipValues(b) {
  const vals = { machine: new Set(), run: new Set(), project: new Set() };
  const addCtx = (c) => {
    vals.machine.add(c.machine);
    vals.run.add(c.run);
    vals.project.add(c.project);
  };
  for (const s of b.sessions) addCtx(s.ctx);
  for (const d of b.decisions) addCtx(d.ctx);
  for (const l of b.log) addCtx(l.ctx);
  for (const n of b.notes) addCtx(n.ctx);
  for (const i of b.items) {
    vals.machine.add(i.ctx.machine);
    vals.run.add(i.run);
    vals.project.add(repoName(i.repo));
  }
  for (const r of b.runs) vals.run.add(r.name);
  for (const k of DIMS) {
    if (state[k] && state[k] !== ADHOC) vals[k].add(state[k]);
    vals[k].delete('');
    vals[k].delete(ADHOC);
  }
  const out = {};
  for (const k of DIMS) out[k] = [...vals[k]].sort();
  return out;
}

// ---------- classification ----------

const RANK = { waiting: 0, quiet: 1, active: 2, idle: 3, ended: 4 };

function tone(display) {
  switch (display) {
    case 'waiting': return 'wait';
    case 'quiet': return 'warn';
    case 'ended': return 'done';
    case 'idle': return 'idle';
    default: return 'run';
  }
}

function ledgerTone(stateText) {
  const s = (stateText || '').toLowerCase();
  if (/^(merged|closed|done)/.test(s)) return 'done';
  if (/decision|blocked|waiting/.test(s)) return 'wait';
  return 'run';
}

function nowText(s, now) {
  if (s.display === 'ended') return 'ended';
  if (s.display === 'waiting') return 'waiting on you';
  if (s.display === 'quiet') return `quiet ${age(now - s.last_seen_at)}`;
  const parts = [];
  if (s.note) parts.push(s.note);
  if (s.tool) parts.push(`running ${s.tool} since ${hhmm(s.tool_since)}`);
  return parts.length ? parts.join(' · ') : s.state === 'active' ? 'active' : 'idle';
}

const sessionName = (s) => s.ctx.name || s.id.slice(0, 8);

// ---------- links ----------

function issueLink(repo, issue) {
  if (!issue) return null;
  if (!hasOwner(repo)) return repo ? `${repo}#${issue}` : `#${issue}`;
  return h('a', { href: `https://github.com/${repo}/issues/${issue}`, target: '_blank', rel: 'noopener' },
    `${repoName(repo)}#${issue}`);
}

function prLink(repo, pr, label) {
  if (!pr) return null;
  if (!hasOwner(repo)) return label;
  return h('a', { href: `https://github.com/${repo}/pull/${pr}`, target: '_blank', rel: 'noopener' }, label);
}

const sessionLink = (id, text) =>
  navLink({ ...state, session: id }, { 'data-testid': 'session-link' }, text);

// ---------- network ----------

async function getJSON(url, tag) {
  const r = await fetch(url, { headers: tag ? { 'If-None-Match': tag } : {}, cache: 'no-cache' });
  if (r.status === 304) return { status: 304 };
  const data = await r.json().catch(() => ({}));
  return { status: r.status, tag: r.headers.get('ETag') || '', data };
}

async function post(url, body) {
  const headers = { 'X-Agentboard': '1' };
  if (body) headers['Content-Type'] = 'application/json';
  const r = await fetch(url, { method: 'POST', headers, body: body ? JSON.stringify(body) : undefined });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `request failed (${r.status})`);
  return data;
}

let busy = false;
let again = false;

async function refresh() {
  if (busy) {
    again = true;
    return;
  }
  busy = true;
  try {
    do {
      again = false;
      if (await load()) render();
    } while (again);
  } catch (err) {
    console.warn('agentboard: refresh failed', err);
  } finally {
    busy = false;
  }
}

async function load() {
  let changed = false;
  const b = await getJSON('api/board', boardTag);
  if (b.status === 200) {
    board = b.data;
    boardTag = b.tag;
    offset = board.now - Date.now();
    changed = true;
  } else if (b.status !== 304) {
    throw new Error(`board: HTTP ${b.status}`);
  }
  const id = state.session;
  if (id) {
    if (detailFor !== id) {
      detail = null;
      detailTag = '';
    }
    const d = await getJSON(`api/sessions/${encodeURIComponent(id)}`, detailTag);
    if (state.session === id) {
      if (d.status === 200) {
        detail = d.data;
        detailTag = d.tag;
        detailFor = id;
        changed = true;
      } else if (d.status === 404) {
        detail = { missing: true };
        detailTag = '';
        detailFor = id;
        changed = true;
      }
    }
  }
  updatedAt = new Date();
  return changed;
}

function connect() {
  const es = new EventSource('events');
  es.addEventListener('open', () => setLive(true));
  es.addEventListener('error', () => setLive(es.readyState === EventSource.OPEN));
  es.addEventListener('board', (e) => {
    setLive(true);
    let v = '';
    try {
      v = JSON.parse(e.data).version;
    } catch {
      return;
    }
    if (!board || v !== board.version || (state.session && detail && !detail.missing && detail.version !== v)) refresh();
  });
  setInterval(() => {
    if (es.readyState !== EventSource.OPEN) refresh();
  }, POLL_MS);
}

function setLive(on) {
  if (live === on) return;
  live = on;
  const el = $('[data-testid=live]');
  if (el) {
    el.dataset.state = on ? 'live' : 'reconnecting';
    el.textContent = on ? '● live' : '● reconnecting';
  }
}

// ---------- actions ----------

async function dismiss(id) {
  try {
    await post(`ui/decisions/${id}/dismiss`);
    flash = '';
  } catch (err) {
    flash = `Dismiss failed: ${err.message}`;
    render();
  }
  refresh();
}

const clearDialog = $('[data-testid=clear-dialog]');
const COUNT_FIELD = { ended_older: 'ended_older_24h', ended_all: 'ended_all', quiet: 'quiet_2h' };
let clearCounts = null;

function clearMode() {
  const r = clearDialog.querySelector('input[name=clear-mode]:checked');
  return r ? r.value : 'ended_older';
}

function updateClearConfirm() {
  const btn = clearDialog.querySelector('[data-testid=clear-confirm]');
  if (!clearCounts) {
    btn.textContent = 'Clear sessions';
    return;
  }
  const n = clearCounts[COUNT_FIELD[clearMode()]] || 0;
  btn.textContent = `Clear ${n} session${n === 1 ? '' : 's'}`;
}

async function openClear() {
  clearCounts = null;
  clearDialog.querySelector('[data-role=error]').textContent = '';
  for (const el of clearDialog.querySelectorAll('[data-testid=clear-count]')) el.textContent = '(…)';
  updateClearConfirm();
  clearDialog.showModal();
  try {
    const r = await getJSON('api/clear-counts');
    if (r.status !== 200) throw new Error(r.data.error || `HTTP ${r.status}`);
    clearCounts = r.data;
    for (const el of clearDialog.querySelectorAll('[data-testid=clear-count]')) {
      el.textContent = `(${clearCounts[COUNT_FIELD[el.dataset.mode]] || 0})`;
    }
    updateClearConfirm();
  } catch (err) {
    clearDialog.querySelector('[data-role=error]').textContent = `Could not load counts: ${err.message}`;
  }
}

clearDialog.addEventListener('change', updateClearConfirm);
clearDialog.querySelector('[data-testid=clear-cancel]').addEventListener('click', () => clearDialog.close());
clearDialog.querySelector('[data-testid=clear-confirm]').addEventListener('click', async () => {
  try {
    await post('ui/clear', { mode: clearMode() });
    clearDialog.close();
    refresh();
  } catch (err) {
    clearDialog.querySelector('[data-role=error]').textContent = err.message;
  }
});

const deleteDialog = $('[data-testid=delete-dialog]');
let deleteId = '';

function openDelete(id) {
  deleteId = id;
  deleteDialog.querySelector('[data-role=error]').textContent = '';
  deleteDialog.showModal();
}

deleteDialog.querySelector('[data-testid=delete-cancel]').addEventListener('click', () => deleteDialog.close());
deleteDialog.querySelector('[data-testid=delete-confirm]').addEventListener('click', async () => {
  try {
    await post(`ui/sessions/${encodeURIComponent(deleteId)}/delete`);
    deleteDialog.close();
    if (state.session === deleteId) go({ ...state, session: '' });
    refresh();
  } catch (err) {
    deleteDialog.querySelector('[data-role=error]').textContent = err.message;
  }
});

// ---------- theme ----------

const THEME_KEY = 'agentboard-theme';
let memoryTheme = ''; // keeps the choice for the session when storage throws

function currentTheme() {
  let t = memoryTheme;
  try { t = localStorage.getItem(THEME_KEY) || t; } catch { /* private mode */ }
  return t === 'light' || t === 'dark' ? t : 'auto';
}

function setTheme(t) {
  memoryTheme = t === 'auto' ? '' : t;
  try {
    if (t === 'auto') localStorage.removeItem(THEME_KEY);
    else localStorage.setItem(THEME_KEY, t);
  } catch { /* private mode */ }
  const root = document.documentElement;
  if (t === 'auto') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', t);
  const meta = document.querySelector('meta[name="color-scheme"]');
  if (meta) meta.setAttribute('content', t === 'auto' ? 'light dark' : t);
  render();
}

function themeSwitch() {
  const cur = currentTheme();
  const btn = (t, label) => h('button', {
    type: 'button', 'data-testid': `theme-${t}`, 'aria-pressed': String(cur === t), onclick: () => setTheme(t),
  }, label);
  return h('div', { class: 'theme-switch', role: 'group', 'aria-label': 'Theme', 'data-testid': 'theme-switch' },
    btn('light', 'Light'), btn('dark', 'Dark'), btn('auto', 'Auto'));
}

// ---------- rendering ----------

function render() {
  const app = $('#app');
  if (!board) return;
  if (state.session) {
    document.title = 'agentboard · session';
    app.replaceChildren(...renderSession());
  } else {
    document.title = 'agentboard';
    app.replaceChildren(...renderMain());
  }
}

function section(title, small, ...body) {
  return [h('h2', null, title, small == null ? null : h('small', null, small)), ...body.flat(Infinity)];
}

const empty = (text) => h('p', { class: 'empty' }, text);

function renderMain() {
  const now = nowMs();
  const sessions = board.sessions.filter((s) => ctxMatches(s.ctx));
  const machines = new Set(sessions.map((s) => s.ctx.machine).filter(Boolean));
  const header = h('header', null,
    h('h1', null, 'agentboard'),
    h('div', { class: 'head-side' }, h('div', { class: 'meta' }, dotJoin([
      liveBadge(),
      updatedAt && `updated ${hhmmss(updatedAt)}`,
      `${sessions.length} session${sessions.length === 1 ? '' : 's'} on ${machines.size} machine${machines.size === 1 ? '' : 's'}`,
    ]), flash && h('span', { class: 'err' }, ' ', flash)), themeSwitch()));
  return [
    header,
    renderChips(),
    ...renderNeeds(now),
    ...renderMeter(),
    ...renderSessions(sessions, now),
    ...renderLedger(),
    renderNotes(),
    ...section('Log', 'newest first · decisions, merges, blockers', renderLog(board.log.filter((l) => ctxMatches(l.ctx)), true)),
  ];
}

function liveBadge() {
  return h('span', { class: 'live', 'data-testid': 'live', 'data-state': live ? 'live' : 'reconnecting' },
    live ? '● live' : '● reconnecting');
}

function renderChips() {
  const vals = chipValues(board);
  const labels = { machine: 'all machines', run: 'all runs', project: 'all projects' };
  const chip = (dim, value, label, cls) => h('button', {
    type: 'button', class: cls ? `chip ${cls}` : 'chip', 'data-testid': 'chip', 'data-dim': dim, 'data-value': value,
    'aria-pressed': String(state[dim] === value), onclick: () => go({ ...state, [dim]: value }),
  }, label);
  const out = [];
  for (const dim of DIMS) {
    if (out.length) out.push(h('span', { class: 'chip-sep', 'aria-hidden': 'true' }, '·'));
    out.push(chip(dim, '', labels[dim]));
    for (const v of vals[dim]) out.push(chip(dim, v, v, dim === 'run' ? 'run-name' : ''));
    if (dim === 'run') out.push(chip(dim, ADHOC, 'ad hoc'));
  }
  return h('div', { class: 'chips' }, out);
}

function whoLine(c, sessionId, at, now, extra) {
  const s = board.sessions.find((x) => x.id === sessionId);
  const name = c.name || (sessionId ? sessionId.slice(0, 8) : '');
  return h('div', { class: 'who' }, h('span', { class: 'dot wait' }), dotJoin([
    extra,
    c.machine,
    c.project,
    c.branch && h('span', { class: 'branch' }, c.branch),
    issueLink(c.repo, c.issue),
    s && prLink(c.repo, s.pr, `PR #${s.pr}`),
    s ? sessionLink(s.id, name) : name,
    `${age(now - at)} ago`,
  ]));
}

function decisionCard(d, now, withWho) {
  return h('div', { class: 'you', 'data-testid': 'decision', 'data-id': d.id },
    h('button', { type: 'button', 'data-testid': 'decision-dismiss', onclick: () => dismiss(d.id) }, 'Dismiss'),
    withWho
      ? whoLine(d.ctx, d.ctx.session, d.created_at, now, d.kind !== 'question' && d.kind)
      : h('div', { class: 'who' }, dotJoin([d.kind !== 'question' && d.kind, `${age(now - d.created_at)} ago`])),
    h('div', null, d.question),
    d.rec && h('div', { class: 'rec', 'data-testid': 'decision-rec' }, h('b', null, 'Recommend:'), ' ', d.rec));
}

function byNewest(key) {
  return (a, b) => b[key] - a[key];
}

function renderNeeds(now) {
  const decisions = board.decisions.filter((d) => d.status === 'open' && ctxMatches(d.ctx));
  const waiting = board.sessions.filter((s) => s.state === 'waiting' && ctxMatches(s.ctx)).sort(byNewest('last_seen_at'));
  const groups = [];
  let total = 0;
  for (const kind of ['question', 'review', 'waiting', 'notify']) {
    let cards;
    if (kind === 'waiting') {
      cards = waiting.map((s) => h('div', { class: 'you', 'data-testid': 'needs-session', 'data-session': s.id },
        whoLine(s.ctx, s.id, s.last_seen_at, now),
        h('div', null, s.waiting_on ? `Waiting on you: ${s.waiting_on.replaceAll('_', ' ')}` : 'Waiting on you')));
    } else {
      cards = decisions.filter((d) => d.kind === kind).sort(byNewest('created_at')).map((d) => decisionCard(d, now, true));
    }
    total += cards.length;
    if (cards.length) groups.push(h('div', { 'data-testid': 'needs-group', 'data-kind': kind }, cards));
  }
  return section('What needs you', `${total} open`, groups.length ? groups : empty('Nothing needs you.'));
}

function selectedMeter() {
  if (state.run === ADHOC) return null;
  if (state.run) return board.meters.find((m) => m.run === state.run) || null;
  const metered = new Set(board.meters.map((m) => m.run));
  const run = board.runs.filter((r) => !r.ended_at && metered.has(r.name)).sort(byNewest('started_at'))[0];
  return run ? board.meters.find((m) => m.run === run.name) : null;
}

function renderMeter() {
  const m = selectedMeter();
  if (!m) return [];
  const items = board.items.filter((i) => i.run === m.run);
  const st = (i) => (i.state || '').toLowerCase();
  const prsWaiting = items.filter((i) => i.pr > 0 && !/^(merged|closed)/.test(st(i))).length;
  const mergedReview = items.filter((i) => st(i).startsWith('merged') && i.tier === 'high-confidence').length;
  const box = (value, label) => h('div', null, h('b', null, value), h('span', null, label));
  return section('Queue', m.run, h('div', { class: 'meter', 'data-testid': 'meter' },
    box(`${m.open_start} → ${m.open_now}`, 'open bugs, start → now'),
    box(`${m.filed} / ${m.closed}`, 'filed / closed this pass'),
    box(String(prsWaiting), 'PRs waiting on you'),
    box(String(mergedReview), 'merged, for your review')));
}

function sessionGroups(sessions) {
  const runs = new Map(board.runs.map((r) => [r.name, r]));
  const byRun = new Map();
  for (const s of sessions) {
    const key = s.ctx.run || '';
    if (!byRun.has(key)) byRun.set(key, []);
    byRun.get(key).push(s);
  }
  const named = [...byRun.keys()].filter((k) => k !== '')
    .map((k) => runs.get(k) || { name: k, started_at: 0, ended_at: 0, coordinator_name: '' });
  const order = [
    ...named.filter((r) => !r.ended_at).sort(byNewest('started_at')),
    ...named.filter((r) => r.ended_at).sort(byNewest('started_at')),
  ];
  const groups = order.map((r) => ({ key: r.name, run: r, rows: byRun.get(r.name) }));
  if (byRun.has('')) groups.push({ key: ADHOC, run: null, rows: byRun.get('') });
  for (const g of groups) {
    g.rows.sort((a, b) => (RANK[a.display] ?? 5) - (RANK[b.display] ?? 5) || b.last_seen_at - a.last_seen_at);
  }
  return groups;
}

function groupLabel(r) {
  if (!r) return 'ad hoc';
  return dotJoin([
    r.name,
    r.started_at && `started ${hhmm(r.started_at)}`,
    r.ended_at && `ended ${hhmm(r.ended_at)}`,
    r.coordinator_name && `coordinator ${r.coordinator_name}`,
  ]).join('');
}

function whereCell(c, pr) {
  const parts = dotJoin([
    c.project,
    c.branch && h('span', { class: 'branch' }, c.branch),
    issueLink(c.repo, c.issue),
    prLink(c.repo, pr, `PR #${pr}`),
  ]);
  return parts.length ? parts : '—';
}

function renderSessions(sessions, now) {
  const clear = h('button', { type: 'button', 'data-testid': 'clear-open', onclick: openClear }, 'Clear finished…');
  const heading = h('h2', null, 'Sessions', h('small', null, clear));
  if (!sessions.length) return [heading, empty('No sessions.')];
  const rows = [];
  for (const g of sessionGroups(sessions)) {
    rows.push(h('tr', { class: 'group', 'data-testid': 'run-group', 'data-run': g.key }, h('td', { colspan: 5 }, groupLabel(g.run))));
    for (const s of g.rows) {
      const t = tone(s.display);
      rows.push(h('tr', { 'data-testid': 'session-row', 'data-session': s.id, 'data-display': s.display },
        h('td', null, h('span', { class: `dot ${t}` }), sessionLink(s.id, sessionName(s))),
        h('td', null, s.ctx.machine || '—'),
        h('td', null, whereCell(s.ctx, s.pr)),
        h('td', { class: t }, nowText(s, now)),
        h('td', null, age(now - s.last_seen_at))));
    }
  }
  const head = h('thead', null, h('tr', null,
    ['Session', 'Where', 'Project · branch · issue', 'Now', 'Last post'].map((t) => h('th', null, t))));
  const cols = h('colgroup', null, ['session', 'where', 'project', 'now', 'last'].map((c) => h('col', { class: `c-${c}` })));
  return [heading, h('table', { class: 'cards sessions' }, cols, head, h('tbody', null, rows))];
}

function runLabel() {
  if (state.run === ADHOC) return 'ad hoc';
  return state.run || 'all runs';
}

function renderLedger() {
  const items = board.items.filter(itemMatches);
  if (!items.length) return section('Ledger', runLabel(), empty('No items.'));
  const head = h('thead', null, h('tr', null, ['Issue', 'What', 'PR', 'State'].map((t) => h('th', null, t))));
  const rows = items.map((i) => h('tr', { 'data-testid': 'item-row', 'data-key': `${i.repo}#${i.number}` },
    h('td', null, issueLink(i.repo, i.number)),
    h('td', null, i.title || '—'),
    h('td', null, prLink(i.repo, i.pr, `#${i.pr}`) || '—'),
    h('td', { class: ledgerTone(i.state) }, dotJoin([i.state, i.tier]).join('') || '—')));
  return section('Ledger', runLabel(), h('table', { class: 'cards ledger' }, h('colgroup', null, ['issue', 'what', 'pr', 'state'].map((c) => h('col', { class: `c-${c}` }))), head, h('tbody', null, rows)));
}

function renderNotes() {
  const notes = board.notes.filter((n) => ctxMatches(n.ctx));
  const col = (kind, title) => {
    const list = notes.filter((n) => n.kind === kind);
    return h('div', null, ...section(title, null, list.length
      ? h('ul', { class: 'log' }, list.map((n) => h('li', { 'data-testid': 'note', 'data-kind': kind }, n.text)))
      : empty('Nothing yet.')));
  };
  return h('div', { class: 'two' }, col('well', 'Went well'), col('less', 'Went less well'));
}

function logPrefix(l) {
  const verb = l.kind === 'ask' ? 'asks: ' : l.kind === 'answer' ? 'answers: ' : '';
  if (l.ctx.name) return `${l.ctx.name}${verb ? ` ${verb}` : ': '}`;
  return verb;
}

function renderLog(lines, withSource) {
  if (!lines.length) return empty('Nothing logged.');
  return h('ul', { class: 'log' }, lines.map((l) => h('li', { 'data-testid': 'log-line' },
    h('span', { class: 't' }, hhmm(l.at)),
    withSource ? logPrefix(l) : (l.kind === 'ask' ? 'asks: ' : l.kind === 'answer' ? 'answers: ' : ''),
    l.text,
    withSource && (l.ctx.machine || l.ctx.project)
      ? [' ', h('span', { class: 'src' }, dotJoin([l.ctx.machine, l.ctx.project]).join(''))]
      : null)));
}

function renderSession() {
  const back = navLink({ ...state, session: '' }, null, '← all sessions');
  if (!detail) return [h('header', null, h('h1', null, 'Loading…'), h('div', { class: 'head-side' }, h('div', { class: 'meta' }, back), themeSwitch()))];
  if (detail.missing) {
    return [h('header', null, h('h1', null, 'Session not found'), h('div', { class: 'head-side' }, h('div', { class: 'meta' }, back), themeSwitch())),
      empty('It may have been cleared or deleted.')];
  }
  const now = nowMs();
  const s = detail.session;
  const c = s.ctx;
  const item = detail.item;
  const t = tone(s.display);
  const until = s.ended_at || now;
  const kv = (label, value) => [h('dt', null, label), h('dd', null, value || '—')];
  const open = detail.decisions.filter((d) => d.status === 'open');
  const events = h('table', { class: 'history' },
    h('colgroup', null, ['time', 'state', 'note'].map((c) => h('col', { class: `c-${c}` }))),
    h('thead', null, h('tr', null, ['Time', 'State', 'Note'].map((x) => h('th', null, x)))),
    h('tbody', null, detail.events.map((e) => h('tr', null,
      h('td', null, hhmm(e.at)),
      h('td', { class: tone(e.state) }, e.state === 'waiting' ? 'waiting on you' : e.state),
      h('td', null, e.note)))));
  return [
    h('section', { 'data-testid': 'session-view', 'data-session': s.id },
      h('header', null,
        h('h1', null, h('span', { class: `dot ${t}` }), sessionName(s)),
        h('div', { class: 'head-side' }, h('div', { class: 'meta' }, dotJoin([liveBadge(), back])), themeSwitch())),
      h('dl', { class: 'kv' },
        kv('Machine', c.machine), kv('Run', c.run || 'ad hoc'),
        kv('Project', c.project), kv('Branch', c.branch && h('span', { class: 'branch' }, c.branch)),
        kv('Issue', c.issue ? [issueLink(c.repo, c.issue), item && item.title ? ` ${item.title}` : ''] : ''),
        kv('PR', s.pr ? dotJoin([prLink(c.repo, s.pr, `#${s.pr}`), item && item.state]) : ''),
        kv('Session id', h('code', null, s.id)),
        kv('Started', `${hhmm(s.started_at)} · ${duration(until - s.started_at)}`),
        kv('Now', h('span', { class: t }, nowText(s, now)))),
      open.length ? section(open.length === 1 ? 'Open decision' : 'Open decisions', null,
        open.map((d) => decisionCard(d, now, false))) : null,
      section('State history', null, detail.events.length ? events : empty('No history.')),
      section('Log', null, renderLog(detail.log, false)),
      h('p', null, h('button', { type: 'button', class: 'danger', 'data-testid': 'session-delete', onclick: () => openDelete(s.id) },
        'Delete this session…'))),
  ];
}

// ---------- start ----------

refresh();
connect();
setInterval(() => {
  if (board) render();
}, AGE_MS);
