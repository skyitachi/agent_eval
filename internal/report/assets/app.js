/* Evaluation report client.
 *
 * Plain ES5-compatible-ish JS, no build step and no dependencies, for the same
 * reason the CSS has none: the artifact must keep working when opened from a
 * file:// URL on a machine that has never seen this project.
 *
 * SECURITY: almost everything rendered here is model- or agent-generated text
 * (prompts, trajectories, tool output, diffs, stderr). A report is opened in a
 * browser, which makes innerHTML a stored-XSS sink. Every interpolation goes
 * through esc() by default; only fragments explicitly wrapped in raw() -- and
 * therefore constructed only by this file -- bypass it.
 */
(function () {
  'use strict';

  /* ------------------------------------------------------------ escaping */

  var RAW = 'aeval:raw';

  function raw(s) {
    var o = {};
    o[RAW] = String(s);
    return o;
  }

  function isRaw(v) {
    return v && typeof v === 'object' && Object.prototype.hasOwnProperty.call(v, RAW);
  }

  function esc(v) {
    return String(v == null ? '' : v).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function tpl(strings) {
    var vals = Array.prototype.slice.call(arguments, 1);
    var out = '';
    for (var i = 0; i < strings.length; i++) {
      out += strings[i];
      if (i >= vals.length) continue;
      var v = vals[i];
      if (v == null || v === false) continue;
      if (isRaw(v)) out += v[RAW];
      else if (Array.isArray(v)) out += v.map(function (x) { return isRaw(x) ? x[RAW] : esc(x); }).join('');
      else out += esc(v);
    }
    return out;
  }

  /* ------------------------------------------------------------ formatting */

  function num(v, digits) {
    if (v == null || isNaN(v)) return '-';
    return Number(v).toFixed(digits == null ? 2 : digits);
  }

  function int(v) {
    if (v == null || isNaN(v)) return '-';
    return Number(v).toLocaleString();
  }

  function pct(v, digits) {
    if (v == null || isNaN(v)) return '-';
    return (Number(v) * 100).toFixed(digits == null ? 1 : digits) + '%';
  }

  function money(v) {
    if (v == null || isNaN(v)) return '-';
    if (v === 0) return '$0';
    if (Math.abs(v) < 0.01) return '$' + Number(v).toFixed(5);
    return '$' + Number(v).toFixed(4);
  }

  function dur(ms) {
    if (ms == null || isNaN(ms)) return '-';
    ms = Number(ms);
    if (ms < 1000) return Math.round(ms) + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + 's';
    if (ms < 3600000) {
      var m = Math.floor(ms / 60000), s = Math.round((ms % 60000) / 1000);
      return m + 'm' + (s ? ' ' + s + 's' : '');
    }
    var h = Math.floor(ms / 3600000), mm = Math.round((ms % 3600000) / 60000);
    return h + 'h' + (mm ? ' ' + mm + 'm' : '');
  }

  function bytes(n) {
    if (n == null) return '-';
    if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MiB';
    if (n >= 1024) return (n / 1024).toFixed(1) + ' KiB';
    return n + ' B';
  }

  function tokens(n) {
    if (n == null) return '-';
    if (n >= 1000000) return (n / 1000000).toFixed(2) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
    return String(n);
  }

  function clock(iso) {
    if (!iso) return '-';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleString();
  }

  function clockShort(iso) {
    if (!iso) return '-';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }

  /* --------------------------------------------------------------- payload */

  var P = window.__AEVAL_PAYLOAD__ || {};
  var rawPayload = JSON.stringify(P, null, 2);

  var MODE = document.body.getAttribute('data-mode') || 'run';

  /* ------------------------------------------------------------------ state */

  var S = {
    tab: 'overview',
    selected: null,
    sortKey: null,
    sortDir: 1,
    status: 'all',
    category: '',
    reason: '',
    query: '',
    limit: 150
  };

  var root = document.getElementById('main');
  var drawer = document.getElementById('drawer');

  /* ------------------------------------------------------------------ utils */

  function el(html) {
    var t = document.createElement('template');
    t.innerHTML = html.trim();
    return t.content.firstElementChild;
  }

  function toast(msg) {
    var old = document.querySelector('.toast');
    if (old) old.remove();
    var node = el(tpl`<div class="toast">${msg}</div>`);
    document.body.appendChild(node);
    setTimeout(function () { node.remove(); }, 2200);
  }

  function copy(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { toast('Copied'); },
        function () { toast('Copy failed'); });
    } else {
      toast('Clipboard unavailable');
    }
  }

  function statusPill(t) {
    var cls, text;
    if (t.status === 'skipped') { cls = 'skip'; text = 'skipped'; }
    else if (t.status === 'failed') { cls = 'fail'; text = 'failed'; }
    else if (t.passed) { cls = 'pass'; text = 'passed'; }
    else { cls = 'fail'; text = 'not passed'; }
    return raw(tpl`<span class="pill ${cls}">${text}</span>`);
  }

  /* A non-fatal note (a grader that errored, an artifact that could not be
   * written) does not change the verdict, so it is surfaced next to the pill
   * rather than folded into it. */
  function errorFlag(t) {
    if (!t.error || t.status === 'failed') return raw('');
    return raw(tpl`<span class="v warn" title="${t.error}">note</span>`);
  }

  function scoreCell(t) {
    var cls = t.passed ? 'pass' : 'fail';
    var w = Math.max(0, Math.min(1, t.score || 0)) * 100;
    return raw(tpl`<span class="scorebar"><span class="track ${cls}"><i style="width:${w}%"></i></span><span class="num">${num(t.score, 3)}</span></span>`);
  }

  function breakdownClass(label) {
    if (/pass|fixed|completed/i.test(label)) return 'pass';
    if (/fail|error|timeout|regress/i.test(label)) return 'fail';
    if (/truncat|incomplete|max_turns|harness|warn/i.test(label)) return 'warn';
    return '';
  }

  /* --------------------------------------------------------------- header */

  function renderBrand() {
    var b = document.getElementById('brand');
    if (!b) return;
    if (MODE === 'index') {
      b.innerHTML = tpl`Evaluations<span class="brand-sub">run history</span>`;
    } else if (MODE === 'compare') {
      b.innerHTML = tpl`Comparison<span class="brand-sub">${P.compare ? P.compare.left_label + ' → ' + P.compare.right_label : ''}</span>`;
    } else {
      var r = P.run || {};
      b.innerHTML = tpl`Report<span class="brand-sub">${r.dataset_name || ''}${r.dataset_version ? '@' + r.dataset_version : ''}</span>`;
    }
  }

  function renderRunHead() {
    var r = P.run || {};
    var counts = P.counts || {};
    var statusCls = r.status === 'completed' ? 'pass' : (r.status === 'running' ? 'warn' : 'fail');

    var chips = [];
    chips.push(raw(tpl`<span class="pill ${statusCls}">${r.status}</span>`));
    if (r.dataset_name) {
      chips.push(raw(tpl`<span class="chip">dataset <b>${r.dataset_name}${r.dataset_version ? '@' + r.dataset_version : ''}</b></span>`));
    }
    if (r.dataset_hash) chips.push(raw(tpl`<span class="chip">revision <b class="mono-sm">${r.dataset_hash.slice(0, 18)}</b></span>`));
    chips.push(raw(tpl`<span class="chip">agent <b>${r.agent_kind || '-'}</b></span>`));
    if (r.agent_model) chips.push(raw(tpl`<span class="chip">model <b>${r.agent_model}</b></span>`));
    /* The mode says who acted. An oracle or baseline run stores a result with
     * exactly the shape of an agent run, so this chip is the only thing on the
     * page that stops "the reference solution passes" from being read as "the
     * model passes". It gets an emphatic class when it is not the default. */
    if (r.mode) {
      chips.push(raw(tpl`<span class="chip${r.mode === 'agent' ? '' : ' mode-alert'}">mode <b>${r.mode}</b></span>`));
    }
    chips.push(raw(tpl`<span class="chip">backend <b>${r.sandbox_backend || '-'}</b></span>`));
    if (r.sandbox_image) chips.push(raw(tpl`<span class="chip">image <b class="mono-sm">${r.sandbox_image}</b></span>`));
    chips.push(raw(tpl`<span class="chip">k=<b>${r.samples_per_task}</b> · concurrency <b>${r.concurrency}</b></span>`));
    chips.push(raw(tpl`<span class="chip">started <b>${clock(r.created_at)}</b></span>`));
    chips.push(raw(tpl`<span class="chip">duration <b>${dur(r.duration_ms)}</b></span>`));
    if (r.host_arch) chips.push(raw(tpl`<span class="chip">host <b>${r.host_arch}</b></span>`));
    if (r.tool_version) chips.push(raw(tpl`<span class="chip">tool <b>${r.tool_version}</b></span>`));

    var actions = [];
    actions.push(raw(tpl`<a class="btn ghost" href="#tasks">Tasks</a>`));

    var head = el(tpl`
      <div class="card runhead">
        <div class="row1">
          <div>
            <h1>${r.name || r.id || 'Evaluation run'}</h1>
            <div class="subtitle mono-sm">${r.id || ''}</div>
          </div>
          <div class="runhead-actions">${actions}</div>
        </div>
        <div class="chips">${chips}</div>
        <div class="chips">
          <span class="chip"><b>${int(counts.passed)}</b> / <b>${int(counts.samples)}</b> cells passed</span>
          <span class="chip"><b>${int(counts.tasks_passed)}</b> / <b>${int(counts.tasks)}</b> tasks resolved</span>
          ${counts.failed ? raw(tpl`<span class="chip">failed <b>${int(counts.failed)}</b></span>`) : ''}
          ${counts.errored ? raw(tpl`<span class="chip">errored <b>${int(counts.errored)}</b></span>`) : ''}
          ${counts.skipped ? raw(tpl`<span class="chip">skipped <b>${int(counts.skipped)}</b></span>`) : ''}
        </div>
      </div>`);
    return head;
  }

  function renderNotices() {
    var frag = document.createDocumentFragment();
    var r = P.run || {};
    if (r.error) {
      frag.appendChild(el(tpl`<div class="notice err"><b>Run error.</b> ${r.error}</div>`));
    }
    if (P.warnings && P.warnings.length) {
      var items = P.warnings.map(function (w) { return tpl`<li>${w}</li>`; });
      frag.appendChild(el(tpl`<div class="notice warn"><b>Report notes.</b><ul>${items}</ul></div>`));
    }
    if (r.notes) {
      frag.appendChild(el(tpl`<div class="notice info"><b>Notes.</b> ${r.notes}</div>`));
    }
    return frag;
  }

  /* --------------------------------------------------------------- metrics */

  function metricExtra(m) {
    var e = m.extra;
    if (!e) return '';
    if (e.reason) return e.reason;
    var parts = [];
    Object.keys(e).sort().forEach(function (k) {
      var v = e[k];
      if (v == null || typeof v === 'object') return;
      var s = typeof v === 'number' ? (Number.isInteger(v) ? int(v) : num(v, 3)) : String(v);
      if (/^(passed|total|count|resolved|cost|tokens|k)$/.test(k)) parts.push(k + '=' + s);
    });
    return parts.slice(0, 3).join(' · ');
  }

  function renderMetrics() {
    var metrics = P.metrics || [];
    if (!metrics.length) return null;
    var groups = {};
    var order = [];
    metrics.forEach(function (m) {
      var g = m.group || 'other';
      if (!groups[g]) { groups[g] = []; order.push(g); }
      groups[g].push(m);
    });
    var wrap = el('<div></div>');
    order.forEach(function (g) {
      var cards = groups[g].map(function (m) {
        var info = m.description ? raw(tpl` <span class="infobtn" title="${m.description}">ⓘ</span>`) : raw('');
        var extra = metricExtra(m);
        return raw(tpl`
          <div class="metric">
            <div class="name">${m.label || m.name}${info}</div>
            <div class="value">${m.display}</div>
            ${extra ? raw(tpl`<div class="extra">${extra}</div>`) : ''}
          </div>`);
      });
      wrap.appendChild(el(tpl`
        <div class="metric-group">
          <h3>${g}</h3>
          <div class="metrics">${cards}</div>
        </div>`));
    });
    return wrap;
  }

  /* ------------------------------------------------------------ breakdowns */

  function renderBreakdowns() {
    var bs = P.breakdowns || [];
    if (!bs.length) return null;
    var cards = bs.map(function (b) {
      var max = 0;
      (b.rows || []).forEach(function (r) { max = Math.max(max, Math.abs(r.value)); });
      if (max <= 0) max = 1;
      var rows = (b.rows || []).map(function (r) {
        var w = Math.max(1, Math.abs(r.value) / max * 100);
        var cls = r.total && r.total === r.hits ? 'pass' : (/rating|rate/i.test(b.title) && r.value < 0.5 ? 'fail' : breakdownClass(r.label));
        var fillCls = cls === 'pass' ? 'pass' : (cls === 'fail' ? 'fail' : (cls === 'warn' ? 'warn' : ''));
        return raw(tpl`
          <div class="brow">
            <span class="bl" title="${r.label}">${r.label}</span>
            <span class="bbar ${fillCls}"><i style="width:${w}%"></i></span>
            <span class="bt">${r.text}</span>
          </div>`);
      });
      return raw(tpl`
        <div class="card breakdown">
          <h3>${b.title}</h3>
          ${b.note ? raw(tpl`<div class="note">${b.note}</div>`) : ''}
          ${rows}
        </div>`);
    });
    return el(tpl`<div class="breakdowns">${cards}</div>`);
  }

  /* ------------------------------------------------------------ task table */

  /* The reward column answers "what would a trainer have been paid", which is a
   * different question from the score column. A dense contract pays the score; a
   * sparse one pays 0 or 1; and an attempt rejected by a guard pays 0 whatever it
   * scored. An empty reward_kind means the row was written before the column
   * existed, so the cell says nothing rather than printing a 0 that would read
   * as a measurement. */
  function rewardCell(t) {
    if (t.violations && t.violations.length) {
      return raw(tpl`<span class="v fail" title="${t.violations.join('; ')}">0 · rejected</span>`);
    }
    if (!t.reward_kind) {
      return raw('<span class="muted" title="this run did not record which reward contract produced the value">-</span>');
    }
    var label = t.reward > 0 ? 'pass' : 'fail';
    return raw(tpl`<span class="scorebar"><span class="track ${label}"><i style="width:${Math.max(0, Math.min(1, t.reward || 0)) * 100}%"></i></span><span class="num">${num(t.reward, 3)}</span></span><span class="tmeta muted"> ${t.reward_kind}</span>`);
  }

  var COLUMNS = [
    { key: 'task', label: 'Task', sortable: true },
    { key: 'status', label: 'Status', sortable: true },
    { key: 'score', label: 'Score', sortable: true, num: true },
    { key: 'reward', label: 'Reward', sortable: true, num: true },
    { key: 'duration_ms', label: 'Duration', sortable: true, num: true },
    { key: 'cost_usd', label: 'Cost', sortable: true, num: true },
    { key: 'total_tokens', label: 'Tokens', sortable: true, num: true },
    { key: 'turns', label: 'Turns', sortable: true, num: true },
    { key: 'tool_calls', label: 'Tools', sortable: true, num: true },
    { key: 'stop_reason', label: 'Stop reason', sortable: true },
    { key: 'detail', label: 'Detail', sortable: false }
  ];

  function filteredTasks() {
    var q = S.query.trim().toLowerCase();
    var out = (P.tasks || []).filter(function (t) {
      if (S.status === 'failed' && t.passed) return false;
      if (S.status === 'passed' && !t.passed) return false;
      if (S.status === 'errored' && t.status !== 'failed') return false;
      if (S.status === 'skipped' && t.status !== 'skipped') return false;
      if (S.category && t.category !== S.category) return false;
      if (S.reason && t.stop_reason !== S.reason) return false;
      if (q) {
        var hay = [t.id, t.category, t.difficulty, t.stop_reason, t.model, (t.tags || []).join(' '),
          t.error || '', (t.graders || []).map(function (g) { return g.grader; }).join(' ')].join(' ').toLowerCase();
        if (hay.indexOf(q) < 0) return false;
      }
      return true;
    });
    if (S.sortKey) {
      var k = S.sortKey, dir = S.sortDir;
      out.sort(function (a, b) {
        var av = a[k], bv = b[k];
        if (k === 'task') { av = a.id; bv = b.id; }
        if (k === 'status') { av = a.passed ? 1 : 0; bv = b.passed ? 1 : 0; }
        if (typeof av === 'string' || typeof bv === 'string') {
          return String(av == null ? '' : av).localeCompare(String(bv == null ? '' : bv)) * dir;
        }
        return ((av || 0) - (bv || 0)) * dir;
      });
    }
    return out;
  }

  /* The chip counts are computed over every cell rather than over the current
   * result, so the numbers stay put while the user narrows the filter. A chip
   * count that shrinks as you click a different chip makes it impossible to tell
   * whether the filter or the data changed. */
  function filterBar() {
    var all = P.tasks || [];
    function count(fn) { return all.filter(fn).length; }
    var defs = [
      ['all', 'All', all.length],
      ['failed', 'Not passed', count(function (t) { return !t.passed; })],
      ['passed', 'Passed', count(function (t) { return t.passed; })],
      ['errored', 'Errored', count(function (t) { return t.status === 'failed'; })],
      ['skipped', 'Skipped', count(function (t) { return t.status === 'skipped'; })]
    ];
    var btns = defs.map(function (d) {
      return raw(tpl`<button type="button" class="fbtn ${S.status === d[0] ? 'on' : ''}" data-status="${d[0]}">${d[1]}<span class="n">${int(d[2])}</span></button>`);
    });

    var cats = [], reasons = [];
    all.forEach(function (t) {
      if (t.category && cats.indexOf(t.category) < 0) cats.push(t.category);
      if (t.stop_reason && reasons.indexOf(t.stop_reason) < 0) reasons.push(t.stop_reason);
    });
    cats.sort(); reasons.sort();

    function selectHtml(id, label, options, current) {
      var opts = [raw(tpl`<option value="">all ${label}</option>`)].concat(options.map(function (o) {
        return raw(tpl`<option value="${o}"${o === current ? ' selected' : ''}>${o}</option>`);
      }));
      return raw(tpl`<select class="btn" id="${id}" aria-label="${label}">${opts}</select>`);
    }

    var bar = el(tpl`
      <div class="filters">
        ${btns}
        ${cats.length > 1 ? selectHtml('cat', 'categories', cats, S.category) : ''}
        ${reasons.length > 1 ? selectHtml('reason', 'stop reasons', reasons, S.reason) : ''}
        <span class="spacer"></span>
        <span class="muted small" id="rowcount"></span>
      </div>`);
    bar.querySelectorAll('[data-status]').forEach(function (b) {
      b.addEventListener('click', function () { S.status = b.getAttribute('data-status'); S.limit = 150; render(); });
    });
    var catSel = bar.querySelector('#cat');
    if (catSel) catSel.addEventListener('change', function () { S.category = catSel.value; S.limit = 150; render(); });
    var rSel = bar.querySelector('#reason');
    if (rSel) rSel.addEventListener('change', function () { S.reason = rSel.value; S.limit = 150; render(); });
    return bar;
  }

  function renderTaskTable() {
    var tasks = filteredTasks();
    var frag = document.createDocumentFragment();
    frag.appendChild(filterBar());

    var head = COLUMNS.map(function (c) {
      var arrow = '';
      if (S.sortKey === c.key) arrow = tpl`<span class="arrow">${S.sortDir > 0 ? '▲' : '▼'}</span>`;
      return raw(tpl`<th class="${c.num ? 'num' : ''}${c.sortable ? '' : ' nosort'}" data-key="${c.sortable ? c.key : ''}">${c.label} ${arrow}</th>`);
    });

    var shown = tasks.slice(0, S.limit);
    var rows = shown.map(function (t) {
      var detail = t.detail ? raw(tpl`<span class="v warn" title="${t.detail}">partial</span>`) : raw('<span class="muted">full</span>');
      return raw(tpl`
        <tr data-key="${t.key}" class="${S.selected === t.key ? 'selected' : ''}${t.status === 'skipped' ? ' gone' : ''}">
          <td class="task">
            ${t.id}${(P.counts && P.counts.samples_per_task > 1) ? raw(tpl`<span class="muted mono-sm"> #${t.sample_index}</span>`) : ''}
            <span class="tmeta">${[t.category, t.difficulty].filter(Boolean).join(' · ')}${t.tags && t.tags.length ? (([t.category, t.difficulty].filter(Boolean).length) ? ' · ' : '') + t.tags.join(', ') : ''}</span>
          </td>
          <td>${statusPill(t)}${errorFlag(t)}${t.model ? raw(tpl`<span class="tmeta muted mono-sm">${t.model}</span>`) : ''}</td>
          <td class="num">${scoreCell(t)}</td>
          <td class="num">${rewardCell(t)}</td>
          <td class="num">${dur(t.duration_ms)}</td>
          <td class="num">${t.cost_usd ? money(t.cost_usd) : raw('<span class="muted">-</span>')}${t.cost_usd && !t.cost_reliable ? raw('<span class="muted" title="endpoint did not report a price">*</span>') : ''}</td>
          <td class="num">${t.total_tokens ? tokens(t.total_tokens) : raw('<span class="muted">-</span>')}</td>
          <td class="num">${int(t.turns)}</td>
          <td class="num">${t.tool_calls ? int(t.tool_calls) + (t.tool_errors ? raw(tpl`<span class="v fail" title="${t.tool_errors} tool error(s)">!</span>`) : '') : '-'}</td>
          <td class="mono-sm">${t.stop_reason || '-'}</td>
          <td>${detail}</td>
        </tr>`);
    });

    var table = el(tpl`
      <div class="tablewrap">
        <table>
          <thead><tr>${head}</tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`);

    table.querySelectorAll('thead th[data-key]:not([data-key=""])').forEach(function (th) {
      th.addEventListener('click', function () {
        var k = th.getAttribute('data-key');
        if (!k) return;
        if (S.sortKey === k) S.sortDir = -S.sortDir;
        else { S.sortKey = k; S.sortDir = (k === 'score' || k === 'cost_usd' || k === 'duration_ms' || k === 'turns' || k === 'tool_calls' || k === 'total_tokens') ? 1 : 1; }
        render();
      });
    });
    table.querySelectorAll('tbody tr').forEach(function (tr) {
      tr.addEventListener('click', function () {
        S.selected = tr.getAttribute('data-key');
        S.tab = 'overview';
        render();
      });
    });

    frag.appendChild(table);

    var count = frag.querySelector('#rowcount');
    if (count) count.textContent = tasks.length + ' of ' + (P.tasks || []).length + ' cells';

    if (tasks.length > S.limit) {
      var more = el(tpl`<div class="bar-actions"><button type="button" class="btn" id="more">Show ${Math.min(150, tasks.length - S.limit)} more (${tasks.length - S.limit} hidden)</button></div>`);
      more.querySelector('#more').addEventListener('click', function () { S.limit += 150; render(); });
      frag.appendChild(more);
    }
    if (!tasks.length) {
      frag.appendChild(el('<div class="empty">No cells match the current filter.</div>'));
    }
    return frag;
  }

  /* ---------------------------------------------------------------- drawer */

  function selectedTask() {
    return (P.tasks || []).filter(function (t) { return t.key === S.selected; })[0] || null;
  }

  function closeDrawer() {
    S.selected = null;
    render();
  }

  function renderDrawer() {
    var t = selectedTask();
    if (!t) { drawer.hidden = true; drawer.innerHTML = ''; return; }
    drawer.hidden = false;

    var tabs = ['overview', 'trajectory', 'diff', 'graders', 'tools', 'raw'];
    var tabBtns = tabs.map(function (name) {
      return raw(tpl`<button type="button" class="tab ${S.tab === name ? 'on' : ''}" data-tab="${name}">${name}${tabCount(name, t)}</button>`);
    });

    var body = el(tpl`<div>${tabBody(S.tab, t)}</div>`);

    var head = el(tpl`
      <div class="drawer-head">
        <div>
          <h2>${t.id}${(P.counts && P.counts.samples_per_task > 1) ? raw(tpl` <span class="muted">#${t.sample_index}</span>`) : ''}</h2>
          <div class="sub">${[t.category, t.difficulty, t.status, t.stop_reason].filter(Boolean).join(' · ')}</div>
        </div>
        <div class="runhead-actions">
          <button type="button" class="btn ghost" id="drawer-close">Close</button>
        </div>
      </div>`);
    head.querySelector('#drawer-close').addEventListener('click', closeDrawer);

    var tabsEl = el(tpl`<div class="tabs">${tabBtns}</div>`);
    tabsEl.querySelectorAll('[data-tab]').forEach(function (b) {
      b.addEventListener('click', function () { S.tab = b.getAttribute('data-tab'); render(); });
    });

    drawer.innerHTML = '';
    drawer.appendChild(head);
    var bodyWrap = el('<div class="drawer-body"></div>');
    bodyWrap.appendChild(tabsEl);
    bodyWrap.appendChild(body);
    drawer.appendChild(bodyWrap);
    bodyWrap.scrollTop = 0;
  }

  function tabCount(name, t) {
    if (name === 'trajectory') return (t.events || []).length ? ' ' + (t.events.length) : '';
    if (name === 'graders') return (t.graders || []).length ? ' ' + t.graders.length : '';
    if (name === 'tools') return (t.tools || []).length ? ' ' + t.tools.length : '';
    return '';
  }

  function tabBody(name, t) {
    switch (name) {
      case 'overview': return overviewTab(t);
      case 'trajectory': return trajectoryTab(t);
      case 'diff': return diffTab(t);
      case 'graders': return gradersTab(t);
      case 'tools': return toolsTab(t);
      case 'raw': return rawTab(t);
      default: return overviewTab(t);
    }
  }

  function kv(pairs) {
    var items = pairs.filter(function (p) { return p && p[1] != null && p[1] !== ''; })
      .map(function (p) { return raw(tpl`<dt>${p[0]}</dt><dd>${p[1]}</dd>`); });
    if (!items.length) return raw('');
    return raw(tpl`<dl class="kv">${items}</dl>`);
  }

  function overviewTab(t) {
    var def = t.definition;
    var parts = [];

    if (t.error) parts.push(raw(tpl`<div class="notice err"><b>Error.</b> ${t.error}</div>`));
    if (t.detail) parts.push(raw(tpl`<div class="notice warn"><b>Partial detail.</b> ${t.detail}</div>`));
    if (t.parse_errors && t.parse_errors.length) {
      parts.push(raw(tpl`<div class="notice warn"><b>${t.parse_errors.length} unparsable stream line(s).</b> The trajectory below is incomplete.</div>`));
    }

    parts.push(el(tpl`
      <div class="card" style="padding:14px 16px;margin-bottom:14px">
        ${kv([
          ['status', t.status + (t.passed ? ' (passed)' : '')],
          ['stop reason', t.stop_reason],
          ['score', num(t.score, 4) + ' / threshold ' + num(t.threshold, 2)],
          ['reward', t.reward_kind ? raw(tpl`<span class="mono-sm">${num(t.reward, 4)}</span> <span class="muted">${t.reward_kind} contract</span>`) : ''],
          ['violations', (t.violations || []).length ? raw(tpl`<span class="v fail">${t.violations.join('; ')}</span>`) : ''],
          ['restored', (t.restored || []).length ? raw(tpl`<span class="mono-sm">${t.restored.join(', ')}</span> <span class="muted">reset to the baseline before grading</span>`) : ''],
          ['model', t.model],
          ['session', t.session_id ? raw(tpl`<span class="mono-sm">${t.session_id}</span>`) : ''],
          ['started / ended', clock(t.started_at) + ' → ' + clock(t.ended_at)],
          ['duration', dur(t.duration_ms)],
          ['cost', money(t.cost_usd) + (t.cost_reliable ? '' : ' (not reported by the endpoint)')],
          ['tokens', 'in ' + int(t.input_tokens) + ' · out ' + int(t.output_tokens) + ' · cache read ' + int(t.cache_read_tokens) + ' · cache write ' + int(t.cache_create_tokens)],
          ['turns / tools', int(t.turns) + ' turns · ' + int(t.tool_calls) + ' calls · ' + int(t.tool_errors) + ' errored'],
          ['ttft / api', dur(t.ttft_ms) + ' / ' + dur(t.api_duration_ms)],
          ['exit code', t.exit_code == null ? '' : String(t.exit_code) + (t.timed_out ? ' (timed out)' : '') + (t.turns_exceeded ? ' (turn budget exceeded)' : '')],
          ['tags', (t.tags || []).join(', ')],
          ['command', t.command_line ? raw(tpl`<span class="mono-sm">${t.command_line}</span>`) : ''],
          ['artifacts', t.artifact_dir ? raw(tpl`<span class="mono-sm">${t.artifact_dir}</span>`) : ''],
          ['workspace', t.workspace_dir ? raw(tpl`<span class="mono-sm">${t.workspace_dir}</span>`) : '']
        ])}
      </div>`));

    if (t.answer) {
      parts.push(raw(tpl`<h3 class="small muted">Final answer</h3>`));
      parts.push(raw(tpl`<pre class="code">${t.answer}</pre>`));
    }
    if (t.stderr) {
      parts.push(raw(tpl`<h3 class="small muted mt">stderr</h3>`));
      parts.push(raw(tpl`<pre class="code">${t.stderr}</pre>`));
    }
    if (def) {
      var rows = [
        ['prompt', def.prompt ? raw(tpl`<pre class="code">${def.prompt}</pre>`) : ''],
        ['system prompt', def.system_prompt ? raw(tpl`<pre class="code">${def.system_prompt}</pre>`) : ''],
        ['workspace', def.workspace],
        ['output contract', def.output],
        ['limits', def.limits],
        ['graders', (def.graders || []).join(' · ')],
        ['baseline', def.baseline ? raw(tpl`<span class="mono-sm">${def.baseline}</span>`) : ''],
        ['metadata', def.metadata ? raw(tpl`<pre class="code">${JSON.stringify(def.metadata, null, 2)}</pre>`) : '']
      ];
      parts.push(raw(tpl`<h3 class="small muted mt">Task definition</h3>`));
      parts.push(el(tpl`<div class="card" style="padding:14px 16px">${kv(rows)}</div>`));
    } else {
      parts.push(el(tpl`<div class="notice info">The task definition is not available: the artifact bundle is not reachable from this machine. The prompt lives in <code>task.json</code>, which stays under the run's working directory.</div>`));
    }
    return parts;
  }

  function trajectoryTab(t) {
    var evs = t.events || [];
    if (!evs.length) {
      return el(tpl`<div class="empty">No trajectory recorded for this cell.${t.stop_reason ? raw(tpl` Stop reason: <code>${t.stop_reason}</code>.`) : ''}</div>`);
    }
    var byId = {};
    (t.tools || []).forEach(function (c) { if (c.id) byId[c.id] = c; });
    var consumed = {};

    var nodes = evs.map(function (e) {
      switch (e.kind) {
        case 'assistant_text':
          return raw(tpl`
            <div class="ev ev-text">
              <div class="ev-head"><span class="who assistant">assistant</span>${e.timestamp ? raw(tpl`<span>${clock(e.timestamp)}</span>`) : ''}<span>#${e.index}</span></div>
              <div class="bubble">${e.text}</div>
            </div>`);
        case 'assistant_thinking':
          return raw(tpl`
            <div class="ev ev-think">
              <div class="ev-head"><span class="who">thinking</span><span>#${e.index}</span></div>
              <details><summary>reasoning (${e.text ? e.text.length : 0} chars)</summary><div class="inner"><div class="bubble think">${e.text}</div></div></details>
            </div>`);
        case 'tool_use': {
          var call = e.tool_use_id ? byId[e.tool_use_id] : null;
          if (e.tool_use_id) consumed[e.tool_use_id] = true;
          var out = call ? call.output : e.tool_output;
          var isErr = call ? call.is_error : e.tool_error;
          var ms = call && call.duration_ms ? call.duration_ms : 0;
          var inner = [];
          if (e.tool_input) {
            inner.push(raw(tpl`<details><summary>input</summary><div class="inner"><pre class="code">${prettyJSON(e.tool_input)}</pre></div></details>`));
          }
          if (out) {
            inner.push(raw(tpl`<div class="tsec"><div class="lbl">result</div><pre class="code">${out}</pre></div>`));
          } else if (!call) {
            inner.push(raw(tpl`<div class="tsec muted small">no result recorded</div>`));
          }
          return raw(tpl`
            <div class="ev ev-tool${isErr ? ' err' : ''}">
              <div class="ev-head"><span class="who tool">tool</span><span>#${e.index}</span></div>
              <div class="toolcard${isErr ? ' err' : ''}">
                <div class="th"><span class="tname">${e.tool_name || 'tool'}</span>${isErr ? raw('<span class="v fail">error</span>') : ''}<span class="tmeta">${ms ? dur(ms) : ''}</span></div>
                ${inner}
              </div>
            </div>`);
        }
        case 'tool_result': {
          if (e.tool_use_id && consumed[e.tool_use_id]) return raw('');
          return raw(tpl`
            <div class="ev ev-tool${e.tool_error ? ' err' : ''}">
              <div class="ev-head"><span class="who tool">tool result</span><span>#${e.index}</span></div>
              <div class="toolcard${e.tool_error ? ' err' : ''}"><div class="th"><span class="tname">${e.tool_name || e.tool_use_id || 'result'}</span></div>
              <div class="tsec"><pre class="code">${e.tool_output || e.text || ''}</pre></div></div>
            </div>`);
        }
        case 'result':
          return raw(tpl`
            <div class="ev ev-result">
              <div class="ev-head"><span class="who result">result</span><span>#${e.index}</span></div>
              <div class="bubble">${e.text}</div>
            </div>`);
        case 'user_message':
          return raw(tpl`
            <div class="ev">
              <div class="ev-head"><span class="who user">user</span><span>#${e.index}</span></div>
              <div class="bubble">${e.text}</div>
            </div>`);
        case 'truncated':
          return raw(tpl`
            <div class="ev ev-truncated">
              <div class="ev-head"><span class="who">gap</span></div>
              <div class="bubble trunc">${e.text}</div>
            </div>`);
        case 'init':
          return raw(tpl`
            <div class="ev">
              <div class="ev-head"><span class="who">session</span><span>#${e.index}</span></div>
              <div class="bubble">${e.text || 'session initialised'}</div>
            </div>`);
        default:
          return raw(tpl`
            <div class="ev">
              <div class="ev-head"><span class="who">${e.kind}</span><span>#${e.index}</span></div>
              <div class="bubble">${e.text || e.tool_output || ''}</div>
            </div>`);
      }
    });

    var wrap = el(tpl`
      <div>
        <div class="bar-actions">
          <button type="button" class="btn ghost" id="ev-open">Expand all</button>
          <button type="button" class="btn ghost" id="ev-close">Collapse all</button>
          <span class="muted small">${evs.length} event(s)</span>
        </div>
        <div class="timeline">${nodes}</div>
      </div>`);
    wrap.querySelector('#ev-open').addEventListener('click', function () {
      wrap.querySelectorAll('details').forEach(function (d) { d.open = true; });
    });
    wrap.querySelector('#ev-close').addEventListener('click', function () {
      wrap.querySelectorAll('details').forEach(function (d) { d.open = false; });
    });
    return wrap;
  }

  function prettyJSON(raw) {
    if (!raw) return '';
    try { return JSON.stringify(JSON.parse(raw), null, 2); } catch (e) { return raw; }
  }

  function diffTab(t) {
    if (!t.diff) {
      return el(tpl`<div class="empty">The agent produced no changes against the task baseline.</div>`);
    }
    var node = el(tpl`
      <div>
        <div class="bar-actions">
          <button type="button" class="btn ghost" id="diff-copy">Copy patch</button>
          <span class="muted small">${t.diff.length} bytes</span>
        </div>
        ${renderDiff(t.diff)}
      </div>`);
    node.querySelector('#diff-copy').addEventListener('click', function () { copy(t.diff); });
    return node;
  }

  function renderDiff(diff) {
    var lines = String(diff).split('\n');
    var html = lines.map(function (line) {
      var cls = 'ctx';
      if (/^(\+\+\+|---) /.test(line)) cls = 'file';
      else if (/^@@/.test(line)) cls = 'hunk';
      else if (/^\+/.test(line)) cls = 'add';
      else if (/^-/.test(line)) cls = 'del';
      return tpl`<span class="dl ${cls}">${line === '' ? ' ' : line}</span>`;
    });
    return raw(tpl`<div class="diff">${html}</div>`);
  }

  function gradersTab(t) {
    var gs = t.graders || [];
    if (!gs.length) return el('<div class="empty">No grader verdicts recorded.</div>');
    var cards = gs.map(function (g) {
      var cls = g.error ? 'fail' : (g.passed ? 'pass' : 'fail');
      var details = g.details ? raw(tpl`<details><summary>details</summary><div class="inner"><pre class="code">${JSON.stringify(g.details, null, 2)}</pre></div></details>`) : raw('');
      return raw(tpl`
        <div class="card" style="padding:12px 14px;margin-bottom:10px">
          <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
            <span class="v ${cls}">${g.passed ? 'pass' : 'fail'}</span>
            <b>${g.grader}</b>
            <span class="muted small mono-sm">${g.type}</span>
            <span class="chip">score <b>${num(g.score, 3)}</b></span>
            <span class="chip">weight <b>${num(g.weight, 2)}</b></span>
            ${g.required ? raw('<span class="chip tag">required</span>') : ''}
            <span class="chip">${dur(g.duration_ms)}</span>
          </div>
          ${g.reason ? raw(tpl`<div class="mt small">${g.reason}</div>`) : ''}
          ${g.error ? raw(tpl`<div class="notice err mt"><b>Grader error.</b> ${g.error}</div>`) : ''}
          ${details ? raw(tpl`<div class="mt">${details}</div>`) : ''}
        </div>`);
    });
    return el(tpl`<div>${cards}</div>`);
  }

  function toolsTab(t) {
    var cs = t.tools || [];
    if (!cs.length) return el('<div class="empty">No tool invocations recorded.</div>');
    var rows = cs.map(function (c) {
      return raw(tpl`
        <tr>
          <td class="num muted">${c.seq}</td>
          <td class="mono-sm">${c.name}${c.is_error ? raw(' <span class="v fail">error</span>') : ''}</td>
          <td class="num">${c.duration_ms ? dur(c.duration_ms) : '-'}</td>
          <td>
            ${c.input ? raw(tpl`<details><summary>input</summary><div class="inner"><pre class="code">${prettyJSON(c.input)}</pre></div></details>`) : raw('<span class="muted small">-</span>')}
            ${c.output ? raw(tpl`<details><summary>output (${bytes(c.output.length)})</summary><div class="inner"><pre class="code">${c.output}</pre></div></details>`) : ''}
          </td>
        </tr>`);
    });
    return el(tpl`
      <div class="tablewrap" style="max-height:none">
        <table><thead><tr><th class="nosort" style="width:40px">#</th><th class="nosort">Tool</th><th class="nosort num" style="width:80px">Time</th><th class="nosort">Payload</th></tr></thead>
        <tbody>${rows}</tbody></table>
      </div>`);
  }

  function rawTab(t) {
    var parts = [];
    parts.push(raw(tpl`<div class="card" style="padding:14px 16px">${kv([
      ['raw stream', t.raw_stream_path ? raw(tpl`<span class="mono-sm">${t.raw_stream_path}</span>`) : 'not stored'],
      ['artifact dir', t.artifact_dir ? raw(tpl`<span class="mono-sm">${t.artifact_dir}</span>`) : ''],
      ['workspace dir', t.workspace_dir ? raw(tpl`<span class="mono-sm">${t.workspace_dir}</span>`) : '']
    ])}</div>`));
    parts.push(raw(tpl`<h3 class="small muted mt">Stored cell record</h3>`));
    var clone = {};
    Object.keys(t).forEach(function (k) {
      if (k === 'events' || k === 'tools' || k === 'diff' || k === 'answer' || k === 'definition') return;
      clone[k] = t[k];
    });
    parts.push(raw(tpl`<pre class="code">${JSON.stringify(clone, null, 2)}</pre>`));
    if (t.definition && t.definition.raw) {
      parts.push(raw(tpl`<h3 class="small muted mt">task.json</h3><pre class="code">${t.definition.raw}</pre>`));
    }
    return el(tpl`<div>${parts}</div>`);
  }

  /* --------------------------------------------------------- index (list) */

  function renderIndex() {
    var runs = P.runs || [];
    var canDelete = !!(P.capabilities && P.capabilities.delete);
    var frag = document.createDocumentFragment();

    var stats = P.stats ? Object.keys(P.stats).map(function (k) {
      return raw(tpl`<span class="chip">${k} <b>${int(P.stats[k])}</b></span>`);
    }) : [];
    frag.appendChild(el(tpl`
      <div class="card runhead">
        <div class="row1"><div>
          <h1>Evaluation runs</h1>
          <div class="subtitle">${runs.length} run(s) · generated ${P.generated}${P.live_refresh_seconds ? raw(tpl` · refreshing every ${P.live_refresh_seconds}s`) : ''}</div>
        </div></div>
        <div class="chips">${stats}</div>
      </div>`));

    if (!runs.length) {
      frag.appendChild(el('<div class="empty">No runs recorded yet. Start one with <code>aeval run</code>.</div>'));
      return frag;
    }

    var rows = runs.map(function (r) {
      var statusCls = r.status === 'completed' ? 'pass' : (r.status === 'running' ? 'warn' : 'fail');
      var rr = r.task_count ? (r.passed_count / r.task_count) : 0;
      var cost = (r.metrics && r.metrics.total_cost_usd) || r.total_cost_usd || 0;
      var href = '/runs/' + encodeURIComponent(r.id);
      return raw(tpl`
        <tr data-run="${r.id}" data-href="${href}">
          <td class="task">${r.name || r.id}<span class="tmeta mono-sm">${r.id}</span></td>
          <td><span class="pill ${statusCls}">${r.status}</span></td>
          <td>${r.dataset_name}${r.dataset_version ? raw(tpl`<span class="muted small"> @${r.dataset_version}</span>`) : ''}</td>
          <td class="mono-sm">${r.agent_kind}${r.agent_model ? ' / ' + r.agent_model : ''}<span class="tmeta muted">${r.sandbox_backend}</span>${r.mode && r.mode !== 'agent' ? raw(tpl`<span class="tmeta mode-alert">${r.mode}</span>`) : ''}</td>
          <td class="num">${int(r.passed_count)} / ${int(r.task_count)}<span class="tmeta">${pct(rr)}</span></td>
          <td class="num">${cost ? money(cost) : '-'}</td>
          <td class="num">${dur(r.duration_ms)}</td>
          <td class="mono-sm">${clockShort(r.created_at)}</td>
          <td class="num nowrap">
            <a class="btn ghost" href="${href}">Report</a>
            ${canDelete ? raw(tpl`<button type="button" class="btn danger" data-delete="${r.id}">Delete</button>`) : ''}
          </td>
        </tr>`);
    });

    var table = el(tpl`
      <div class="section">
        <div class="tablewrap" style="max-height:none">
          <table>
            <thead><tr>
              <th class="nosort">Run</th><th class="nosort">Status</th><th class="nosort">Dataset</th>
              <th class="nosort">Agent</th><th class="nosort num">Resolved</th><th class="nosort num">Cost</th>
              <th class="nosort num">Duration</th><th class="nosort">Started</th><th class="nosort"></th>
            </tr></thead>
            <tbody>${rows}</tbody>
          </table>
        </div>
      </div>`);

    table.querySelectorAll('tbody tr').forEach(function (tr) {
      tr.addEventListener('click', function (e) {
        if (e.target.closest('button')) return;
        window.location.href = tr.getAttribute('data-href');
      });
    });
    table.querySelectorAll('[data-delete]').forEach(function (b) {
      b.addEventListener('click', function (e) {
        e.stopPropagation();
        var id = b.getAttribute('data-delete');
        if (!window.confirm('Delete run ' + id + ' and every result recorded under it?\n\nThis cannot be undone.')) return;
        b.disabled = true;
        fetch('/api/runs/' + encodeURIComponent(id) + '/delete', { method: 'POST' })
          .then(function (res) { return res.json().then(function (body) { return { ok: res.ok, body: body }; }); })
          .then(function (out) {
            if (!out.ok) { toast(out.body.error || 'Delete failed'); b.disabled = false; return; }
            toast('Deleted ' + id);
            setTimeout(function () { window.location.reload(); }, 400);
          })
          .catch(function (err) { b.disabled = false; toast('Delete failed: ' + err); });
      });
    });

    frag.appendChild(table);
    return frag;
  }

  /* ------------------------------------------------------------ comparison */

  function renderCompare() {
    var c = P.compare;
    var frag = document.createDocumentFragment();
    if (!c) {
      frag.appendChild(el('<div class="empty">No comparison payload.</div>'));
      return frag;
    }
    var s = c.summary || {};
    frag.appendChild(el(tpl`
      <div class="card runhead">
        <div class="row1"><div>
          <h1>${c.left_label} → ${c.right_label}</h1>
          <div class="subtitle">A = ${c.left_run_id} · B = ${c.right_run_id}</div>
        </div></div>
        <div class="chips">
          <span class="chip">common cells <b>${int(s.common)}</b></span>
          <span class="chip" style="color:var(--pass);border-color:var(--pass)">fixed <b>${int(s.fixed)}</b></span>
          <span class="chip" style="color:var(--fail);border-color:var(--fail)">regressed <b>${int(s.regressed)}</b></span>
          <span class="chip">still passing <b>${int(s.both_pass)}</b></span>
          <span class="chip">still failing <b>${int(s.both_fail)}</b></span>
          ${s.only_left ? raw(tpl`<span class="chip">only in A <b>${int(s.only_left)}</b></span>`) : ''}
          ${s.only_right ? raw(tpl`<span class="chip">only in B <b>${int(s.only_right)}</b></span>`) : ''}
        </div>
      </div>`));

    var mrows = (c.metrics || []).map(function (m) {
      var cls = m.verdict === 'better' ? 'better' : (m.verdict === 'worse' ? 'worse' : 'same');
      return raw(tpl`
        <tr title="${m.description || ''}">
          <td class="task">${m.label || m.name}<span class="tmeta">${m.name}${m.direction === 'down' ? ' (lower is better)' : (m.direction === 'up' ? ' (higher is better)' : '')}</span></td>
          <td class="num">${m.left_text}</td>
          <td class="num">${m.right_text}</td>
          <td class="num delta ${cls}">${m.only_left || m.only_right ? 'n/a' : m.delta_text}</td>
          <td><span class="v ${cls === 'better' ? 'pass' : (cls === 'worse' ? 'fail' : 'skip')}">${m.verdict}</span></td>
        </tr>`);
    });

    frag.appendChild(el(tpl`
      <div class="section"><h2>Metrics</h2>
        <div class="card"><div class="tablewrap" style="max-height:60vh">
          <table><thead><tr>
            <th class="nosort">Metric</th><th class="nosort num">A</th><th class="nosort num">B</th>
            <th class="nosort num">Δ</th><th class="nosort">Verdict</th>
          </tr></thead><tbody>${mrows}</tbody></table>
        </div></div>
      </div>`));

    var trows = (c.tasks || []).map(function (d) {
      var cls = d.verdict === 'fixed' ? 'pass' : (d.verdict === 'regressed' ? 'fail' : 'skip');
      return raw(tpl`
        <tr data-key="${d.key}">
          <td class="task">${d.id}${d.category ? raw(tpl`<span class="tmeta">${d.category}</span>`) : ''}</td>
          <td><span class="v ${d.left.present ? (d.left.passed ? 'pass' : 'fail') : 'skip'}">${d.left.present ? (d.left.passed ? 'pass' : 'fail') : 'absent'}</span></td>
          <td class="num">${d.left.present ? dur(d.left.duration_ms) : '-'}</td>
          <td class="num">${d.left.present && d.left.cost_usd ? money(d.left.cost_usd) : '-'}</td>
          <td><span class="v ${d.right.present ? (d.right.passed ? 'pass' : 'fail') : 'skip'}">${d.right.present ? (d.right.passed ? 'pass' : 'fail') : 'absent'}</span></td>
          <td class="num">${d.right.present ? dur(d.right.duration_ms) : '-'}</td>
          <td class="num">${d.right.present && d.right.cost_usd ? money(d.right.cost_usd) : '-'}</td>
          <td><span class="v ${cls}">${d.verdict.replace('_', ' ')}</span></td>
        </tr>`);
    });

    frag.appendChild(el(tpl`
      <div class="section"><h2>Cells</h2>
        <div class="card"><div class="tablewrap">
          <table><thead><tr>
            <th class="nosort">Task</th>
            <th class="nosort">A</th><th class="nosort num">A time</th><th class="nosort num">A cost</th>
            <th class="nosort">B</th><th class="nosort num">B time</th><th class="nosort num">B cost</th>
            <th class="nosort">Verdict</th>
          </tr></thead><tbody>${trows}</tbody></table>
        </div></div>
      </div>`));

    return frag;
  }

  /* ---------------------------------------------------------------- render */

  function render() {
    renderBrand();
    root.innerHTML = '';
    root.appendChild(renderNotices());

    if (MODE === 'index') {
      root.appendChild(renderIndex());
    } else if (MODE === 'compare') {
      root.appendChild(renderCompare());
    } else {
      root.appendChild(renderRunHead());
      var m = renderMetrics();
      if (m) { var s = el('<div class="section"><h2>Metrics</h2></div>'); s.appendChild(m); root.appendChild(s); }
      var b = renderBreakdowns();
      if (b) { var s2 = el('<div class="section"><h2>Breakdowns</h2></div>'); s2.appendChild(b); root.appendChild(s2); }
      var st = el('<div class="section" id="tasks"><h2>Cells</h2></div>');
      st.appendChild(renderTaskTable());
      root.appendChild(st);
    }
    renderDrawer();
  }

  /* --------------------------------------------------------------- controls */

  var search = document.getElementById('global-search');
  if (search) {
    var timer = null;
    search.addEventListener('input', function () {
      clearTimeout(timer);
      timer = setTimeout(function () { S.query = search.value; S.limit = 150; render(); }, 120);
    });
  }

  var themeBtn = document.getElementById('theme-toggle');
  if (themeBtn) {
    themeBtn.addEventListener('click', function () {
      var cur = document.documentElement.getAttribute('data-theme');
      var order = ['auto', 'light', 'dark'];
      var next = order[(order.indexOf(cur) + 1) % order.length];
      document.documentElement.setAttribute('data-theme', next);
      try { localStorage.setItem('aeval-theme', next); } catch (e) { /* private mode */ }
      toast('Theme: ' + next);
    });
  }

  var exportBtn = document.getElementById('export-btn');
  if (exportBtn) {
    exportBtn.addEventListener('click', function () {
      var blob = new Blob([rawPayload], { type: 'application/json' });
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = (MODE === 'index' ? 'runs' : (P.run && P.run.id) || MODE) + '.json';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(function () { URL.revokeObjectURL(a.href); }, 5000);
    });
  }

  var printBtn = document.getElementById('print-btn');
  if (printBtn) printBtn.addEventListener('click', function () { window.print(); });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && S.selected) closeDrawer();
    if (e.key === '/' && document.activeElement !== search) {
      e.preventDefault();
      if (search) search.focus();
    }
  });

  try {
    var saved = localStorage.getItem('aeval-theme');
    if (saved) document.documentElement.setAttribute('data-theme', saved);
  } catch (e) { /* private mode */ }

  render();
})();
