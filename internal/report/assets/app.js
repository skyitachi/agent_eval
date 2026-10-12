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
 *
 * The two rules that keep that honest, both learned from bugs:
 *   1. A value interpolated into a template must already be escaped or wrapped
 *      in raw(). A tpl() result that is interpolated unwrapped is escaped a
 *      second time and prints its own markup (the diff view did).
 *   2. A node is appended, never interpolated. tpl() stringifies what it is
 *      given, so handing it an element built by el() prints "[object
 *      HTMLDivElement]" where the element should have been (every drawer tab
 *      did). Rendering the mistake as a named marker is the fallback; not
 *      making it is the fix.
 */
(function () {
  'use strict';

  /* ------------------------------------------------------------ escaping */

  var RAW = 'aeval:raw';

  function raw(s) {
    var o = {};
    o[RAW] = String(s);
    /* Stringifies back to its own markup, so "a" + raw(x) is the fragment and
     * not "[object Object]". The baseline rows were written that way -- a
     * plain string plus, on the next line, a wrapped fragment -- and printed
     * "[object Object]" where the delta should have been. toString() makes
     * that form correct instead of merely wrong, and it is what a fragment
     * means anyway. */
    o.toString = function () { return o[RAW]; };
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

  /* A value this file never meant to interpolate -- a DOM node built for
   * appendChild, or a plain object -- used to fall through to String(v) and
   * print "[object HTMLDivElement]" in the middle of a report: the mistake
   * looked like data, so nobody could tell a broken card from a card whose
   * content was that string. It is a bug here, not payload, so it is rendered
   * as a marker that names itself instead of as a plausible-looking value. */
  function unrenderable(v) {
    var name = (v && v.constructor && v.constructor.name) || typeof v;
    return '<span class="bad" title="a template cannot render a ' + esc(name) +
      '; append the node, or wrap the value in raw()">\u26a0 unrendered ' + esc(name) + '</span>';
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
      else if (typeof v === 'object') out += unrenderable(v);
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

  /* A cell's status is about the harness; passed is about the agent. They are
   * separate because "ran out of its budget" and "did the wrong thing" are
   * different findings, and "there was nothing to score it against" is a third. */
  function statusPill(t) {
    var cls, text;
    if (t.status === 'error') { cls = 'fail'; text = 'harness error'; }
    else if (t.status === 'timeout') { cls = 'warn'; text = 'timed out'; }
    else if (!t.graded) { cls = 'warn'; text = 'not graded'; }
    else if (t.passed) { cls = 'pass'; text = 'passed'; }
    else { cls = 'fail'; text = 'failed'; }
    return raw(tpl`<span class="pill ${cls}">${text}</span>`);
  }

  /* A non-fatal note (a grader that errored, an artifact that could not be
   * written) does not change the verdict, so it is surfaced next to the pill
   * rather than folded into it. */
  function errorFlag(t) {
    if (!t.error || t.status === 'error') return raw('');
    return raw(tpl`<span class="v warn" title="${t.error}">note</span>`);
  }

  function scoreCell(t) {
    var cls = t.passed ? 'pass' : 'fail';
    var w = Math.max(0, Math.min(1, t.score || 0)) * 100;
    return raw(tpl`<span class="scorebar"><span class="track ${cls}"><i style="width:${w}%"></i></span><span class="num">${num(t.score, 3)}</span></span>`);
  }

  /* How many tool calls the cell made, with a badge on the count when some of
   * them failed.
   *
   * Both halves are markup, so the cell is built as one fragment and handed
   * over as one value. Adding a raw() fragment to a plain string stringifies
   * it back to its own source, and a template escapes whatever it is given, so
   * the form this replaced printed the badge as text: the Tools column read
   * `6<span class="v fail" title="2 tool error(s)">!</span>`, markup on screen
   * beside eleven columns that were fine. Nothing else caught it -- the file,
   * the payload and the exit code were all correct, and "[object " (the
   * stringified-node mistake this file already guards against) is not what
   * this one prints. */
  function toolCell(t) {
    if (!t.tool_calls) return '-';
    var badge = t.tool_errors
      ? raw(tpl`<span class="v fail" title="${t.tool_errors} tool error(s)">!</span>`)
      : '';
    return raw(tpl`${int(t.tool_calls)}${badge}`);
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
    } else if (MODE === 'sweep') {
      var sw = P.sweep || {};
      b.innerHTML = tpl`Attribution<span class="brand-sub">${sw.axis ? sw.axis + ' sweep' : ''}</span>`;
    } else {
      var r = P.run || {};
      b.innerHTML = tpl`Report<span class="brand-sub">${r.dataset_name || ''}</span>`;
    }
  }

  var RUN_STATUS_CLASS = { completed: 'pass', running: 'warn', interrupted: 'warn', failed: 'fail' };

  function renderRunHead() {
    var r = P.run || {};
    var counts = P.counts || {};
    var statusCls = RUN_STATUS_CLASS[r.status] || 'fail';

    var chips = [];
    chips.push(raw(tpl`<span class="pill ${statusCls}">${r.status}</span>`));
    if (r.dataset_name) {
      chips.push(raw(tpl`<span class="chip">dataset <b>${r.dataset_name}</b></span>`));
    }
    if (r.dataset_hash) chips.push(raw(tpl`<span class="chip">revision <b class="mono-sm">${r.dataset_hash.slice(0, 18)}</b></span>`));
    chips.push(raw(tpl`<span class="chip">agent <b>${r.agent_kind || '-'}</b></span>`));
    /* The model chip is never omitted, which is a change from rendering it only
     * when the label was set. The label is what the command line asked for, so
     * it is empty on every run that did not pass --model -- and drawing nothing
     * then left the reader to choose between "no model was requested", "the
     * CLI's default was used" and "nobody recorded it", three facts that look
     * identical on a page. model.text says which of them it is, and it is
     * written on the server so the browser is not re-deciding where the warning
     * belongs. */
    var modelCls = r.model && r.model.substitute ? ' mode-alert' : '';
    chips.push(raw(tpl`<span class="chip${modelCls}">model <b>${r.model ? r.model.text : 'not recorded'}</b></span>`));
    chips.push(raw(tpl`<span class="chip">backend <b>${r.backend || '-'}</b></span>`));
    /* Skills are always shown, including when nothing is known about them.
     * The skill set is not one of the labels above -- it is the plugin
     * directories plus the flags that decide whether they survive -- and a
     * header that omits it is a header claiming every run configured the same
     * agent. "not recorded" is a distinct claim from "none declared", so the
     * two are never rendered the same way. */
    if (r.skills) {
      /* Alert on the contradiction, not on absence: directories the run
       * declared and the flags then removed are worth colouring, while a run
       * that declared none is simply a run with no skills. */
      var alertCls = r.skills.removed_by ? ' mode-alert' : '';
      chips.push(raw(tpl`<span class="chip${alertCls}">skills <b>${r.skills.text}</b></span>`));
    } else {
      chips.push(raw(tpl`<span class="chip">skills <b>not recorded</b></span>`));
    }
    /* The sweep coordinates are rendered because a run that is one point of a
     * sweep reads very differently from a standalone one: its numbers are only
     * meaningful next to the control level's. */
    if (r.factor_axis) {
      chips.push(raw(tpl`<span class="chip mode-alert">sweep <b>${r.factor_axis}=${r.factor_level || '?'}</b></span>`));
    }
    chips.push(raw(tpl`<span class="chip">k=<b>${r.samples_per_task}</b> · concurrency <b>${r.concurrency}</b></span>`));
    /* Whether each session's tasks shared one workspace and ran in order. It
     * belongs beside k because that is the reading it corrects: a chained run
     * still reports so many samples, but the cells of one chain start from each
     * other's state, so they are one continuation rather than repeats of one
     * measurement. */
    if (r.chain_sessions) {
      chips.push(raw('<span class="chip mode-alert">sessions <b>chained in order</b></span>'));
    }
    chips.push(raw(tpl`<span class="chip">started <b>${clock(r.started_at)}</b></span>`));
    chips.push(raw(tpl`<span class="chip">duration <b>${dur(r.duration_ms)}</b></span>`));
    if (r.tool_version) chips.push(raw(tpl`<span class="chip">tool <b>${r.tool_version}</b></span>`));

    var head = el(tpl`
      <div class="card runhead">
        <div class="row1">
          <div>
            <h1>${r.name || r.id || 'Evaluation run'}</h1>
            <div class="subtitle mono-sm">${r.id || ''}</div>
          </div>
        </div>
        <div class="chips">${chips}</div>
        <div class="chips">
          <span class="chip"><b>${int(counts.passed)}</b> / <b>${int(counts.graded)}</b> graded cells passed</span>
          <span class="chip"><b>${int(counts.cells)}</b> cells over <b>${int(counts.tasks)}</b> tasks</span>
          ${counts.failed ? raw(tpl`<span class="chip">failed <b>${int(counts.failed)}</b></span>`) : ''}
          ${counts.timeouts ? raw(tpl`<span class="chip">timed out <b>${int(counts.timeouts)}</b></span>`) : ''}
          ${counts.ungraded ? raw(tpl`<span class="chip">ungraded <b>${int(counts.ungraded)}</b></span>`) : ''}
          ${counts.harness_errors ? raw(tpl`<span class="chip">harness errors <b>${int(counts.harness_errors)}</b></span>`) : ''}
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
      /* raw(), because these are interpolated into another template below:
       * an unwrapped tpl result reaches that template as a string and is
       * escaped a second time, which prints the markup instead of rendering
       * it. Every value that a template interpolates has to arrive either
       * escaped already or wrapped. */
      var items = P.warnings.map(function (w) { return raw(tpl`<li>${w}</li>`); });
      frag.appendChild(el(tpl`<div class="notice warn"><b>Report notes.</b><ul>${items}</ul></div>`));
    }
    if (r.notes) {
      frag.appendChild(el(tpl`<div class="notice info"><b>Notes.</b> ${r.notes}</div>`));
    }
    return frag;
  }

  /* --------------------------------------------------------------- metrics */

  /* Metrics are a flat list, not a grouped one: the grouping that used to be
   * here came from a metric registry this build does not have, and inventing
   * groups for seven run-level numbers would be decoration.
   *
   * The direction hint is spelled out rather than left to colour. A value is
   * not good or bad on its own -- what makes 400k tokens good is that the last
   * run used 600k -- so the card says which way is better and leaves the
   * judgement to the reader. */
  function renderMetrics() {
    var metrics = P.metrics || [];
    if (!metrics.length) return null;
    var cards = metrics.map(function (m) {
      var info = m.note ? raw(tpl` <span class="infobtn" title="${m.note}">ⓘ</span>`) : raw('');
      var hint = m.direction === 'down' ? 'lower is better'
        : (m.direction === 'up' ? 'higher is better' : '');
      return raw(tpl`
        <div class="metric">
          <div class="name">${m.label || m.key}${info}${hint ? raw(tpl`<span class="dirhint"> · ${hint}</span>`) : ''}</div>
          <div class="value">${m.display}</div>
          ${m.unit ? raw(tpl`<div class="extra">${m.unit}</div>`) : ''}
        </div>`);
    });
    return el(tpl`<div class="metrics">${cards}</div>`);
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

  /* The baseline column answers "what did this cost against the attempt that
   * is on record", which is a different question from the score column. The
   * time delta is measured against the recorded session's *busy* time, not its
   * wall clock: that session had a human in the loop and spent most of its wall
   * time waiting for one, so a wall-clock comparison would flatter every run. */
  function baselineCell(t) {
    var b = t.baseline;
    if (!b) return raw('<span class="muted" title="this task declares no recorded baseline">-</span>');
    /* A recording keeps what it knows and omits the rest. A delta against a
     * number that was never captured is not zero, it is unknown, and printing
     * "+0s" would read as "no change" rather than as "never measured". */
    var parts = [];
    if (b.has_tokens) {
      parts.push(raw(tpl`<span class="delta ${signClass(b.token_delta, true)}" title="${int(b.tokens)} tokens in the recorded attempt">${signedInt(b.token_delta)} tok</span>`));
    }
    if (b.has_time) {
      parts.push(raw(tpl`<span class="delta ${signClass(b.ms_delta, true)}" title="${dur(b.ms)} busy time in the recorded attempt">${signedDur(b.ms_delta)}</span>`));
    }
    if (!parts.length) {
      return raw('<span class="muted" title="the recorded attempt carries neither a token total nor a busy time to compare against">-</span>');
    }
    return raw(tpl`<span class="baseline">${parts}</span>`);
  }

  /* Lower is better for everything a baseline is compared on, so the class is
   * chosen by sign rather than by a caller-supplied flag. */
  function signClass(v, lowerIsBetter) {
    if (!v) return 'same';
    var good = lowerIsBetter ? v < 0 : v > 0;
    return good ? 'better' : 'worse';
  }

  function signedInt(v) {
    if (v == null || isNaN(v)) return '-';
    return (v > 0 ? '+' : '') + int(v);
  }

  function signedDur(v) {
    if (v == null || isNaN(v)) return '-';
    if (!v) return '0s';
    return (v > 0 ? '+' : '-') + dur(Math.abs(v));
  }

  var COLUMNS = [
    { key: 'task', label: 'Task', sortable: true },
    { key: 'status', label: 'Status', sortable: true },
    { key: 'score', label: 'Score', sortable: true, num: true },
    { key: 'baseline', label: 'vs baseline', sortable: false, num: true },
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
      if (S.status === 'failed' && !(t.graded && !t.passed)) return false;
      if (S.status === 'passed' && !t.passed) return false;
      if (S.status === 'ungraded' && t.graded) return false;
      if (S.status === 'timeout' && t.status !== 'timeout') return false;
      if (S.status === 'error' && t.status !== 'error') return false;
      if (S.category && t.category !== S.category) return false;
      if (S.reason && t.stop_reason !== S.reason) return false;
      if (q) {
        var hay = [t.id, t.category, t.stop_reason, (t.tags || []).join(' '),
          t.error || '', (t.graders || []).map(function (g) { return g.name; }).join(' ')].join(' ').toLowerCase();
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
      ['failed', 'Failed', count(function (t) { return t.graded && !t.passed; })],
      ['passed', 'Passed', count(function (t) { return t.passed; })],
      ['ungraded', 'Not graded', count(function (t) { return !t.graded; })],
      ['timeout', 'Timed out', count(function (t) { return t.status === 'timeout'; })],
      ['error', 'Harness error', count(function (t) { return t.status === 'error'; })]
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
      /* Wrapped, because the header is built by one template and this fragment
       * is interpolated into it: an unwrapped tpl() result is a plain string,
       * so the arrow was escaped a second time and the sorted column read
       * `Task <span class="arrow">▲</span>`. Nothing shows until a reader
       * clicks a header to sort, which is why it went unnoticed. */
      var arrow = '';
      if (S.sortKey === c.key) arrow = raw(tpl`<span class="arrow">${S.sortDir > 0 ? '▲' : '▼'}</span>`);
      return raw(tpl`<th class="${c.num ? 'num' : ''}${c.sortable ? '' : ' nosort'}" data-key="${c.sortable ? c.key : ''}">${c.label} ${arrow}</th>`);
    });

    var shown = tasks.slice(0, S.limit);
    var rows = shown.map(function (t) {
      var detail = t.detail ? raw(tpl`<span class="v warn" title="${t.detail}">partial</span>`) : raw('<span class="muted">full</span>');
      return raw(tpl`
        <tr data-key="${t.key}" class="${S.selected === t.key ? 'selected' : ''}">
          <td class="task">
            ${t.id}${(P.counts && P.counts.samples_per_task > 1) ? raw(tpl`<span class="muted mono-sm"> #${t.sample_index}</span>`) : ''}
            <span class="tmeta">${[t.category].concat(t.tags || []).filter(Boolean).join(' · ')}</span>
          </td>
          <td>${statusPill(t)}${errorFlag(t)}</td>
          <td class="num">${scoreCell(t)}</td>
          <td class="num">${baselineCell(t)}</td>
          <td class="num">${dur(t.duration_ms)}</td>
          <td class="num">${t.cost_usd ? money(t.cost_usd) : raw('<span class="muted">-</span>')}</td>
          <td class="num">${t.total_tokens ? tokens(t.total_tokens) : raw('<span class="muted">-</span>')}</td>
          <td class="num">${int(t.turns)}</td>
          <td class="num">${toolCell(t)}</td>
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

    /* Each tab returns a node; append it. Interpolating the node here is the
     * mistake that broke every tab -- a template stringifies it. */
    var body = document.createElement('div');
    body.appendChild(tabBody(S.tab, t));

    var head = el(tpl`
      <div class="drawer-head">
        <div>
          <h2>${t.id}${(P.counts && P.counts.samples_per_task > 1) ? raw(tpl` <span class="muted">#${t.sample_index}</span>`) : ''}</h2>
          <div class="sub">${[t.category, t.status, t.stop_reason].filter(Boolean).join(' · ')}</div>
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

  /* The recording's identity, when the run that compared against it kept one.
   * Empty on a row written before schema v3, which is the absence of a record
   * rather than a recording that had no agent -- so nothing is printed, and
   * the card does not claim to know who made the recording. */
  function recordedBy(b) {
    var parts = [];
    if (b.agent_name) parts.push(b.agent_name + (b.agent_model ? ' (model ' + b.agent_model + ')' : ''));
    if (b.source) parts.push('captured by ' + b.source);
    return parts.join(', ');
  }

  /* A replay against a recording made by a different agent is a real
   * comparison -- it is what replaying a collected session means -- but its
   * delta is the difference between two agents, not the effect of one. Saying
   * so is the whole difference between a number and a claim, so it is a notice
   * rather than a fourth row of figures. */
  function crossAgentNote(b) {
    if (!b || !b.different_agent) return '';
    var ran = (P.run && P.run.agent_kind) ? P.run.agent_kind : 'this attempt\u2019s agent';
    return raw(tpl`The recording was made by <b>${b.agent_name}</b>; this attempt ran <b>${ran}</b>. The deltas below are the difference between two agents, not the effect of ${ran}.`);
  }

  function baselineRows(t) {
    var b = t.baseline;
    if (!b) return [];
    var rows = [
      ['baseline', b.id ? raw(tpl`<span class="mono-sm">${b.id}</span>${b.ref ? raw(tpl` <span class="muted">${b.ref}</span>`) : ''}`) : '']
    ];
    if (b.has_tokens) {
      rows.push(['tokens', raw(tpl`${int(t.total_tokens)} now vs ${int(b.tokens)} recorded → <span class="delta ${signClass(b.token_delta, true)}">${signedInt(b.token_delta)}</span>`)]);
    } else {
      rows.push(['tokens', int(t.total_tokens) + ' now; the recording carries no token total to compare against']);
    }
    if (b.has_time) {
      rows.push(['busy time', raw(tpl`${dur(t.duration_ms)} now vs ${dur(b.ms)} recorded → <span class="delta ${signClass(b.ms_delta, true)}">${signedDur(b.ms_delta)}</span>`)]);
    } else {
      rows.push(['busy time', dur(t.duration_ms) + ' now; the recording carries no busy time to compare against']);
    }
    rows.push(['turns', int(t.turns) + ' now vs ' + int(b.turns) + ' recorded']);
    rows.push(['recorded by', recordedBy(b)]);
    /* The recording's own caveats when it declared any, and the one thing the
     * framework knows about every collected baseline when it did not. Two
     * caveat rows in one card is how both stop being read, so it is one row. */
    rows.push(
      ['note', b.notes ? b.notes : 'the recorded time is the session\u2019s busy time (API plus tool), not its wall clock: it had a human in the loop']
    );
    return rows;
  }

  /* Returns a node, like every other tab, and the drawer appends it. This tab
   * used to return an array mixing raw strings with nodes built by el(), which
   * the caller interpolated into a template -- and a template stringifies what
   * it is given, so two of the three cards rendered as "[object
   * HTMLDivElement]" and the one that survived was the one that happened to be
   * a string. Appending cannot have that failure mode. */
  function overviewTab(t) {
    var frag = document.createDocumentFragment();

    if (t.error) frag.appendChild(el(tpl`<div class="notice err"><b>Error.</b> ${t.error}</div>`));
    if (t.detail) frag.appendChild(el(tpl`<div class="notice warn"><b>Partial detail.</b> ${t.detail}</div>`));

    frag.appendChild(el(tpl`
      <div class="card" style="padding:14px 16px;margin-bottom:14px">
        ${kv([
          ['status', t.status + (t.graded ? (t.passed ? ' · passed' : ' · failed') : ' · nothing to score against')],
          ['stop reason', t.stop_reason],
          ['model', t.model],
          ['score', t.graded ? num(t.score, 4) + ' / threshold ' + num(t.threshold, 2) : ''],
          ['duration', dur(t.duration_ms)],
          ['cost', money(t.cost_usd)],
          ['tokens', 'in ' + int(t.input_tokens) + ' · out ' + int(t.output_tokens) + ' · cache read ' + int(t.cache_read_tokens) + ' · cache write ' + int(t.cache_create_tokens)],
          ['turns / tools', int(t.turns) + ' turns · ' + int(t.tool_calls) + ' calls · ' + int(t.tool_errors) + ' errored'],
          ['tags', (t.tags || []).join(', ')]
        ])}
      </div>`));

    var brows = baselineRows(t);
    if (brows.length) {
      frag.appendChild(el(tpl`<h3 class="small muted mt">Against the recorded baseline</h3>`));
      var cross = crossAgentNote(t.baseline);
      if (cross) frag.appendChild(el(tpl`<div class="notice warn">${cross}</div>`));
      frag.appendChild(el(tpl`<div class="card" style="padding:14px 16px">${kv(brows)}</div>`));
    }

    var answers = answerSection(t);
    if (answers) frag.appendChild(answers);
    return frag;
  }

  /* The two conclusions, side by side. Comparing what each attempt *cost* is
   * only half the question; the half a reader actually cares about is whether
   * they concluded the same thing, and that answer lives in prose that the
   * numbers do not carry. So when the recording kept a final answer the two are
   * shown as columns, and when it did not, the absence is stated instead of
   * being rendered as an empty box that reads like a rendering bug. */
  function answerSection(t) {
    var b = t.baseline;
    var recorded = b && b.output ? String(b.output) : '';
    if (!t.answer && !recorded) return null;

    var frag = document.createDocumentFragment();
    frag.appendChild(el(tpl`<h3 class="small muted mt">Final answer</h3>`));
    if (!recorded) {
      frag.appendChild(el(tpl`<pre class="code">${t.answer}</pre>`));
      if (b) {
        frag.appendChild(el(tpl`<p class="small muted">The recorded attempt kept no final answer to compare against${b.id ? raw(tpl` (<span class="mono-sm">${b.id}</span>)`) : ''}.</p>`));
      }
      return frag;
    }
    frag.appendChild(el(tpl`
      <div class="grid2">
        <div>
          <div class="small muted">this attempt</div>
          ${t.answer ? raw(tpl`<pre class="code">${t.answer}</pre>`) : raw('<div class="muted small">produced no final answer</div>')}
        </div>
        <div>
          <div class="small muted">recorded (baseline)</div>
          <pre class="code">${recorded}</pre>
        </div>
      </div>`));
    return frag;
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
      return raw(tpl`<span class="dl ${cls}">${line === '' ? ' ' : line}</span>`);
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
            <b>${g.name}</b>
            <span class="muted small mono-sm">${g.type}</span>
            <span class="chip">score <b>${num(g.score, 3)}</b></span>
            <span class="chip">weight <b>${num(g.weight, 2)}</b></span>
            ${g.required ? raw('<span class="chip tag">required</span>') : ''}
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
    var frag = document.createDocumentFragment();
    frag.appendChild(el(tpl`<div class="card" style="padding:14px 16px">${kv([
      ['status', t.status],
      ['stop reason', t.stop_reason],
      ['error', t.error]
    ])}</div>`));
    frag.appendChild(el(`<h3 class="small muted mt">Stored cell record</h3>`));
    /* The bulk fields are excluded because they are already on their own tabs;
     * repeating them here would duplicate megabytes of text in the DOM. */
    var clone = {};
    Object.keys(t).forEach(function (k) {
      if (k === 'events' || k === 'tools' || k === 'diff' || k === 'answer' || k === 'graders') return;
      clone[k] = t[k];
    });
    frag.appendChild(el(tpl`<pre class="code">${JSON.stringify(clone, null, 2)}</pre>`));
    return frag;
  }

  /* --------------------------------------------------------- index (list) */

  /* Mirrors report.FileName on the Go side. The two must agree or the index
   * links to files that are not there, and an id comes from a dataset file, so
   * it is sanitised rather than trusted. */
  function reportFileName(id) {
    var name = String(id || '').replace(/[^A-Za-z0-9_-]/g, '-').replace(/^-+|-+$/g, '');
    if (!name) name = 'run';
    if (name.length > 80) name = name.slice(0, 80);
    return 'report-' + name + '.html';
  }

  function renderIndex() {
    var runs = P.runs || [];
    var frag = document.createDocumentFragment();

    frag.appendChild(el(tpl`
      <div class="card runhead">
        <div class="row1"><div>
          <h1>${P.title || 'Evaluation runs'}</h1>
          <div class="subtitle">${runs.length} run(s) · generated ${clock(P.generated)}</div>
        </div></div>
      </div>`));

    if (!runs.length) {
      frag.appendChild(el('<div class="empty">No runs recorded yet. Start one with <code>aeval run</code>.</div>'));
      return frag;
    }

    var rows = runs.map(function (r) {
      var statusCls = RUN_STATUS_CLASS[r.status] || 'fail';
      var href = reportFileName(r.id);
      return raw(tpl`
        <tr data-href="${href}">
          <td class="task">${r.name || r.id}<span class="tmeta mono-sm">${r.id}</span></td>
          <td><span class="pill ${statusCls}">${r.status}</span></td>
          <td>${r.dataset_name}</td>
          <td class="mono-sm">${r.agent_kind}${r.agent_model ? ' / ' + r.agent_model : ''}${r.factor_axis ? raw(tpl`<span class="tmeta mode-alert">${r.factor_axis}=${r.factor_level}</span>`) : ''}</td>
          <td class="num">${int(r.counts.passed)} / ${int(r.counts.graded)}<span class="tmeta">${r.pass_rate == null ? 'not graded' : pct(r.pass_rate)}</span></td>
          <td class="num">${tokens(r.tokens)}</td>
          <td class="num">${r.cost_usd ? money(r.cost_usd) : '-'}</td>
          <td class="num">${dur(r.duration_ms)}</td>
          <td class="mono-sm">${clockShort(r.started_at)}</td>
          <td class="num nowrap"><a class="btn ghost" href="${href}">Report</a></td>
        </tr>`);
    });

    var table = el(tpl`
      <div class="section">
        <div class="tablewrap" style="max-height:none">
          <table>
            <thead><tr>
              <th class="nosort">Run</th><th class="nosort">Status</th><th class="nosort">Dataset</th>
              <th class="nosort">Agent</th><th class="nosort num">Passed</th><th class="nosort num">Tokens</th>
              <th class="nosort num">Cost</th><th class="nosort num">Duration</th><th class="nosort">Started</th>
              <th class="nosort"></th>
            </tr></thead>
            <tbody>${rows}</tbody>
          </table>
        </div>
      </div>`);

    table.querySelectorAll('tbody tr').forEach(function (tr) {
      tr.addEventListener('click', function (e) {
        if (e.target.closest('a')) return;
        window.location.href = tr.getAttribute('data-href');
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
      var hint = m.direction === 'down' ? ' (lower is better)' : (m.direction === 'up' ? ' (higher is better)' : '');
      return raw(tpl`
        <tr>
          <td class="task">${m.label || m.key}<span class="tmeta">${m.key}${hint}</span></td>
          <td class="num">${m.left_text}</td>
          <td class="num">${m.right_text}</td>
          <td class="num delta ${cls}">${m.delta_text}</td>
          <td><span class="v ${cls === 'better' ? 'pass' : (cls === 'worse' ? 'fail' : 'skip')}">${String(m.verdict).replace('_', ' ')}</span></td>
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
      function side(ref) {
        if (!ref.present) return [raw('<span class="v skip">absent</span>'), '-', '-', '-'];
        var label = !ref.graded ? 'ungraded' : (ref.passed ? 'pass' : 'fail');
        var v = !ref.graded ? 'skip' : (ref.passed ? 'pass' : 'fail');
        return [
          raw(tpl`<span class="v ${v}">${label}</span>`),
          dur(ref.duration_ms),
          ref.cost_usd ? money(ref.cost_usd) : '-',
          tokens(ref.total_tokens)
        ];
      }
      var L = side(d.left), R = side(d.right);
      return raw(tpl`
        <tr data-key="${d.key}">
          <td class="task">${d.id}${d.category ? raw(tpl`<span class="tmeta">${d.category}</span>`) : ''}</td>
          <td>${L[0]}</td><td class="num">${L[1]}</td><td class="num">${L[2]}</td><td class="num">${L[3]}</td>
          <td>${R[0]}</td><td class="num">${R[1]}</td><td class="num">${R[2]}</td><td class="num">${R[3]}</td>
          <td><span class="v ${cls}">${d.verdict}</span></td>
        </tr>`);
    });

    frag.appendChild(el(tpl`
      <div class="section"><h2>Cells</h2>
        <div class="card"><div class="tablewrap">
          <table><thead><tr>
            <th class="nosort">Task</th>
            <th class="nosort">A</th><th class="nosort num">A time</th><th class="nosort num">A cost</th><th class="nosort num">A tokens</th>
            <th class="nosort">B</th><th class="nosort num">B time</th><th class="nosort num">B cost</th><th class="nosort num">B tokens</th>
            <th class="nosort">Verdict</th>
          </tr></thead><tbody>${trows}</tbody></table>
        </div></div>
      </div>`));

    return frag;
  }

  /* -------------------------------------------------------------- sweep */

  /* A sweep's answer is a set of paired differences, so the page leads with the
   * findings and puts the raw pairs underneath. The caveats are not hidden
   * behind a disclosure: the single most likely misreading of this page is that
   * it measured more than one factor at a time, and a footnote does not stop
   * that. */
  function renderSweep() {
    var sw = P.sweep;
    var frag = document.createDocumentFragment();
    if (!sw) {
      frag.appendChild(el('<div class="empty">No attribution payload.</div>'));
      return frag;
    }

    var caveats = (sw.caveats || []).map(function (c) { return raw(tpl`<li>${c}</li>`); });
    frag.appendChild(el(tpl`
      <div class="card runhead">
        <div class="row1"><div>
          <h1>${sw.name || sw.sweep_id}</h1>
          <div class="subtitle">one axis varied: <b>${sw.axis}</b> · control <b>${sw.control}</b></div>
        </div></div>
        ${caveats.length ? raw(tpl`<div class="notice warn"><b>What this sweep cannot show.</b><ul>${caveats}</ul></div>`) : ''}
      </div>`));

    var lrows = (sw.levels || []).map(function (l) {
      var s = l.summary || {};
      // "varies nothing" on a non-control level is not a cosmetic detail: that
      // level is measuring run-to-run variance, not a factor, and a reader who
      // cannot see it will read movement into noise.
      var varies = (l.changed && l.changed.length)
        ? 'varies ' + l.changed.join(', ')
        : (l.is_control ? 'the shared configuration, unchanged' : 'varies nothing');
      return raw(tpl`
        <tr>
          <td class="task">${l.name}${l.is_control ? raw(' <span class="chip tag">control</span>') : ''}<span class="tmeta">${varies}</span>${l.notes ? raw(tpl`<span class="tmeta">${l.notes}</span>`) : ''}</td>
          <td><span class="pill ${RUN_STATUS_CLASS[l.status] || 'fail'}">${l.status}</span></td>
          <td class="num">${l.pass_rate == null ? '-' : pct(l.pass_rate)}<span class="tmeta">${int(s.passed)} / ${int(s.graded)}</span></td>
          <td class="num">${tokens(s.tokens)}</td>
          <td class="num">${money(s.cost_usd)}</td>
          <td class="num">${dur(s.duration_ms)}</td>
          <td class="num">${int(s.timeouts)} / ${int(s.harness_errors)}</td>
        </tr>`);
    });
    frag.appendChild(el(tpl`
      <div class="section"><h2>Levels</h2>
        <div class="card"><div class="tablewrap" style="max-height:none">
          <table><thead><tr>
            <th class="nosort">Level</th><th class="nosort">Status</th><th class="nosort num">Pass rate</th>
            <th class="nosort num">Tokens</th><th class="nosort num">Cost</th><th class="nosort num">Wall clock</th>
            <th class="nosort num">Timeouts / errors</th>
          </tr></thead><tbody>${lrows}</tbody></table>
        </div></div>
      </div>`));

    (sw.comparisons || []).forEach(function (cmp) {
      var srows = (cmp.stats || []).map(function (s) {
        return raw(tpl`
          <tr class="${s.primary ? 'primary-metric' : ''}">
            <td class="task">${s.label}${s.primary ? raw(' <span class="chip tag">correctness</span>') : ''}
              <span class="tmeta">${s.metric}${s.lower_is_better ? ' · lower is better' : ' · higher is better'}</span></td>
            <td class="num">${num(s.control_mean, 2)}</td>
            <td class="num">${num(s.level_mean, 2)}</td>
            <td class="num delta ${s.mean_delta === 0 ? 'same' : (s.lower_is_better === (s.mean_delta < 0) ? 'better' : 'worse')}">${s.mean_delta > 0 ? '+' : ''}${num(s.mean_delta, 2)} ${s.unit || ''}</td>
            <td class="num">${int(s.wins)} / ${int(s.losses)} / ${int(s.ties)}</td>
            <td class="num">${s.testable ? num(s.p, 3) : 'n/a'}</td>
          </tr>`);
      });
      var pairs = (cmp.pairs || []).filter(function (p) { return p.verdict !== 'same'; });
      var prows = pairs.map(function (p) {
        return raw(tpl`
          <tr>
            <td class="task">${p.task_id}${p.category ? raw(tpl`<span class="tmeta">${p.category}</span>`) : ''}</td>
            <td class="num">${p.control && p.control.pass_rate != null ? num(p.control.pass_rate, 0) + '%' : '-'}</td>
            <td class="num">${p.level && p.level.pass_rate != null ? num(p.level.pass_rate, 0) + '%' : '-'}</td>
            <td class="num delta ${p.verdict === 'fixed' ? 'better' : 'worse'}">${p.delta && p.delta.tokens != null ? signedInt(Math.round(p.delta.tokens)) + ' tok' : '-'}</td>
            <td><span class="v ${p.verdict === 'fixed' ? 'pass' : (p.verdict === 'broke' ? 'fail' : 'skip')}">${p.verdict}</span></td>
          </tr>`);
      });
      frag.appendChild(el(tpl`
        <div class="section">
          <h2>${cmp.level} against ${cmp.control}</h2>
          <div class="card"><div class="tablewrap" style="max-height:none">
            <table><thead><tr>
              <th class="nosort">Metric</th><th class="nosort num">${cmp.control}</th><th class="nosort num">${cmp.level}</th>
              <th class="nosort num">Mean delta</th><th class="nosort num">win / loss / tie</th><th class="nosort num">p</th>
            </tr></thead><tbody>${srows}</tbody></table>
          </div></div>
          ${prows.length ? raw(tpl`
            <div class="card" style="margin-top:12px"><div class="tablewrap">
              <table><thead><tr>
                <th class="nosort">Task</th><th class="nosort num">pass rate (${cmp.control})</th>
                <th class="nosort num">pass rate (${cmp.level})</th><th class="nosort num">tokens delta</th><th class="nosort">Verdict</th>
              </tr></thead><tbody>${prows}</tbody></table>
            </div></div>`) : ''}
        </div>`));
    });

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
    } else if (MODE === 'sweep') {
      root.appendChild(renderSweep());
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
