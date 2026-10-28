'use strict';
(() => {
  const views = {feed: ['▤', 'Feed', 'Unacknowledged occurrences needing triage'], acknowledged: ['✓', 'Acknowledged', 'Occurrences an operator is investigating'], snoozed: ['◷', 'Snoozed', 'Scoped rules with a fixed return at 06:00 UTC'], closed: ['☑', 'Closed investigations', 'Completed investigations; source health is unchanged'], chai_reports: ['✧', 'Chai reports', 'Scheduled findings, grouped by task with latest and history']};
  const categories = {cluster_fleet: 'Cluster fleet', cloud_cleanup: 'Cloud cleanup', ci_platform: 'CI platform', repo_automation: 'Repo automation', other: 'Other'};
  const el = id => document.getElementById(id);
  const state = {snapshot: null, view: 'feed', category: '', selected: '', selectedReport: '', reportDisclosures: new Set(), query: '', color: '', fetching: false, sending: false, pending: null, pendingUnconfirmed: false, pendingUser: '', storageUser: null, storageError: '', dialogOccurrence: null, dialogKind: '', lastFocus: null, fetchError: ''};
  const sopCache = new Map();
  const sopDisclosures = new Set();
  const node = (tag, text, className) => { const n = document.createElement(tag); if (text !== undefined && text !== null) n.textContent = String(text); if (className) n.className = className; return n; };
  const append = (parent, ...children) => { children.filter(Boolean).forEach(child => parent.append(child)); return parent; };
  const button = (label, handler, className = 'btn') => { const b = node('button', label, className); b.type = 'button'; b.addEventListener('click', handler); return b; };
  const text = value => String(value || '');
  const date = value => { const d = new Date(value); return Number.isFinite(d.getTime()) ? d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC') : 'Not available'; };
  const items = () => state.snapshot?.occurrences || [];
  const ready = () => state.snapshot?.status?.phase === 'ready' && !state.fetchError && !state.sending && !state.pending && !state.storageError && state.storageUser === state.snapshot?.user;
  const id = occurrence => occurrence.identity.id;
  const workflow = occurrence => occurrence.state || {};
  const displayTitle = occurrence => text(occurrence.message.title).replace(/\[\s*ops_color=[^\]]*\]/gi, '').replace(/\s+/g, ' ').trim() || 'Untitled occurrence';
  const outstandingInvestigation = occurrence => ['sent', 'delivery_uncertain'].includes(occurrence.investigation?.status);
  const sorted = occurrences => [...occurrences].sort((a, b) => Date.parse(b.message.reported_at) - Date.parse(a.message.reported_at));
  const isReport = occurrence => occurrence.view === 'chai_reports' || occurrence.classification.category === 'chai_reports';
  const matchesSearch = occurrence => !state.query || [occurrence.message.title, occurrence.message.text, occurrence.message.source, ...(occurrence.message.identifiers || [])].join(' ').toLowerCase().includes(state.query.toLowerCase());
  const visible = () => sorted(items().filter(o => (state.view === 'chai_reports' ? isReport(o) : !isReport(o) && o.view === state.view) && (state.view === 'chai_reports' || ((!state.category || o.classification.category === state.category) && (!state.color || o.classification.color === state.color))) && matchesSearch(o)));
  function link(url, label) {
    try { const parsed = new URL(url); if (parsed.protocol !== 'https:' || parsed.username || parsed.password) return null; const a = node('a', label); a.href = parsed.href; a.target = '_blank'; a.rel = 'noopener noreferrer'; return a; } catch { return null; }
  }
  function badge(o) { const color = isReport(o) ? 'green' : ['red', 'orange', 'yellow'].includes(o.classification.color) ? o.classification.color : 'orange'; return node('span', color === 'green' ? 'Green · Chai report' : color[0].toUpperCase() + color.slice(1), 'badge ' + color); }
  function announce(message) { el('outcome').textContent = message; }
  function error(message) { const shown = state.storageError || message; el('action-error').hidden = !shown; el('action-error').textContent = shown; }
  function pendingKey(user) { return 'ops-alert-dashboard.pending.v1.' + encodeURIComponent(user); }
  function pendingExpired() {
    const timestamp = state.pending?.occurrence_id?.match(/^[^:]+:(\d+)\.(\d{1,6})$/);
    const start = Date.parse(state.snapshot?.status?.cutover);
    if (!timestamp || !Number.isFinite(start)) return false;
    const reported = Number(timestamp[1]) * 1000 + Number((timestamp[2] + '000').slice(0, 3));
    return Number.isSafeInteger(reported) && reported < start && !items().some(o => id(o) === state.pending.occurrence_id);
  }
  function validPending(request) {
    const fields = ['occurrence_id', 'kind', 'expected_revision', 'request_id', 'pattern', 'question'];
    return request && typeof request === 'object' && !Array.isArray(request) && Object.keys(request).every(key => fields.includes(key) && typeof request[key] === 'string') && ['acknowledge', 'unacknowledge', 'close', 'reopen', 'snooze', 'unsnooze', 'investigate'].includes(request.kind) && !!request.occurrence_id && !!request.expected_revision && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(request.request_id);
  }
  function restorePending(user) {
    if (state.sending || (state.storageUser === user && !state.storageError)) return;
    try {
      if (!user || typeof user !== 'string') throw new Error('Authenticated operator is unavailable.');
      const saved = sessionStorage.getItem(pendingKey(user));
      const retained = saved ? JSON.parse(saved) : null;
      if (retained && (retained.user !== user || !validPending(retained.request))) throw new Error('The retained request is invalid; review this browser session before writing.');
      state.storageUser = user; state.storageError = '';
      state.pending = retained?.request || null; state.pendingUser = retained ? user : '';
      // Reload cannot establish whether a retained attempt reached the server.
      state.pendingUnconfirmed = !!retained;
    } catch (err) {
      state.storageError = 'Cannot restore retained action requests. Actions are paused to prevent duplicate writes. ' + (err.message || 'Session storage is unavailable.');
    }
    error('');
  }
  function retainPending(user, request) {
    try {
      const saved = JSON.stringify({user, request});
      sessionStorage.setItem(pendingKey(user), saved);
      if (sessionStorage.getItem(pendingKey(user)) !== saved) throw new Error('Session storage did not retain the request.');
      return true;
    } catch {
      state.storageError = 'Cannot retain the action request in session storage. No write was started by this attempt. Allow session storage and refresh before trying again.';
      error(''); render(); return false;
    }
  }
  function clearPending(user, request) {
    try {
      sessionStorage.removeItem(pendingKey(user));
      if (sessionStorage.getItem(pendingKey(user)) !== null) throw new Error('Retained request was not removed.');
      if (state.pending?.request_id === request.request_id && state.pendingUser === user) { state.pending = null; state.pendingUser = ''; state.pendingUnconfirmed = false; }
    } catch {
      state.storageError = 'This browser could not clear its retained request. Actions are paused. Restore session storage and refresh to reconcile the same request.';
      error('');
    }
  }
  function dismissExpired() {
    if (!pendingExpired() || state.sending || state.snapshot?.status?.phase !== 'ready' || state.fetchError || state.storageError || state.pendingUser !== state.snapshot?.user || state.storageUser !== state.snapshot?.user) return;
    clearPending(state.pendingUser, state.pending);
    if (!state.pending) { error(''); announce('Expired local retry dismissed. Delivery remains unconfirmed; check the original Slack thread. No Slack action was sent or undone.'); }
    render();
  }
  function fact(list, label, value) { append(list, append(node('div'), node('dt', label), node('dd', value))); }
  function renderStatus() {
    const coverage = state.snapshot?.status;
    const box = el('coverage');
    const phase = coverage?.phase || 'catching_up';
    box.className = 'banner ' + (state.fetchError || phase === 'degraded' ? 'error' : phase === 'ready' ? 'ready' : 'warning');
    box.textContent = state.fetchError || (phase === 'ready' ? 'Coverage ready' : phase === 'degraded' ? 'Coverage degraded · actions paused' : 'Catching up on retained Slack history · actions paused');
    if (coverage) box.textContent += ' · Last complete sync: ' + date(coverage.last_sync) + ' · Window starts: ' + date(coverage.cutover) + (coverage.error ? ' · ' + coverage.error : '');
    el('demo').hidden = !state.snapshot?.demo;
    el('operator').textContent = state.snapshot?.user ? 'Operator · ' + state.snapshot.user : 'Connecting…';
    el('refresh').disabled = state.fetching;
    el('retry-panel').hidden = !state.pending || state.sending;
    const expired = pendingExpired();
    el('retry-text').textContent = state.pending ? 'Delivery is unconfirmed. Request ' + state.pending.request_id + (expired ? ' belongs to an alert outside the history window. Check the original Slack thread. You can dismiss this local retry to continue; dismissal does not confirm or undo delivery.' : ' is retained. Check the thread or refresh, then retry this same request to reconcile it. No second request will be created.') : '';
    const blocked = state.snapshot?.status?.phase !== 'ready' || !!state.fetchError || state.sending || !!state.storageError || state.pendingUser !== state.snapshot?.user || state.storageUser !== state.snapshot?.user;
    el('retry').disabled = blocked || expired;
    el('dismiss-expired').hidden = !expired;
    el('dismiss-expired').disabled = blocked;
    if (state.storageError) error('');
  }
  function renderNav() {
    el('navigation').replaceChildren();
    for (const [key, view] of Object.entries(views)) {
      const b = button(view[0] + ' ' + view[1], () => { state.view = key; state.category = ''; state.selected = ''; error(''); render(); }, key === 'chai_reports' ? 'report-nav' : '');
      if (state.view === key) b.setAttribute('aria-current', 'page');
      if (key !== 'chai_reports') append(b, node('span', items().filter(o => !isReport(o) && o.view === key).length, 'count'));
      el('navigation').append(b);
    }
  }
  function renderFilters() {
    el('filters').replaceChildren();
    el('filters').hidden = state.view === 'chai_reports';
    el('color-label').hidden = state.view === 'chai_reports';
    for (const [key, label] of Object.entries(categories)) {
      const b = button('', () => { state.category = state.category === key ? '' : key; render(); }, 'filter');
      b.setAttribute('aria-pressed', String(state.category === key));
      append(b, node('span', label), node('strong', items().filter(o => !isReport(o) && o.view === state.view && o.classification.category === key).length));
      el('filters').append(b);
    }
  }
  function render() {
    renderStatus(); renderNav(); renderFilters();
    el('title').textContent = views[state.view][1]; el('subtitle').textContent = views[state.view][2];
    const content = el('content'); content.replaceChildren();
    if (!state.snapshot) { content.append(node('p', 'Waiting for the first snapshot…', 'empty')); return; }
    const list = visible();
    if (state.view === 'chai_reports') { renderReports(content, sorted(items().filter(isReport))); return; }
    if (!list.some(o => id(o) === state.selected)) state.selected = list[0] ? id(list[0]) : '';
    const workspace = node('div', null, 'workspace');
    const listBox = node('section', null, 'list'); listBox.setAttribute('aria-label', 'Alert occurrences');
    append(listBox, append(node('div', null, 'list-head'), node('span', list.length + ' occurrence' + (list.length === 1 ? '' : 's')), node('span', 'Latest report first')));
    const cards = node('div', null, 'list-items');
    for (const o of list) {
      const b = button('', () => { state.selected = id(o); render(); }, 'alert-card ' + o.classification.color);
      b.setAttribute('aria-pressed', String(id(o) === state.selected));
      append(b, append(node('span', null, 'row-top'), badge(o), node('small', categories[o.classification.category] || 'Other')), node('strong', displayTitle(o)), node('span', (o.message.identifiers || []).join(' · '), 'identifier'), append(node('span', null, 'row-meta'), node('span', o.message.source), node('time', date(o.message.reported_at))));
      if (o.snooze) b.append(node('span', 'Snoozed until ' + date(o.snooze.until), 'identifier'));
      if (o.investigation) b.append(node('span', 'Chai · ' + statusLabel(o.investigation.status), 'identifier'));
      cards.append(b);
    }
    if (!list.length) cards.append(node('p', 'No occurrences match this view and these filters.', 'empty'));
    listBox.append(cards);
    const detail = node('section', null, 'inspector'); detail.setAttribute('aria-label', 'Selected occurrence details');
    const selected = list.find(o => id(o) === state.selected);
    if (selected) renderDetail(detail, selected); else detail.append(node('p', 'Choose another view or clear the filters to inspect an occurrence.', 'empty'));
    append(workspace, listBox, detail); content.append(workspace);
  }
  function action(label, kind, occurrence) {
    const b = button(label, () => kind === 'snooze' || kind === 'investigate' ? openDialog(kind, occurrence) : sendNew(kind, occurrence));
    b.disabled = !ready(); return b;
  }
  function statusLabel(status) { return ({sent: 'Sent · awaiting reply', reply_received: 'Reply received', delivery_uncertain: 'Delivery uncertain', failed: 'Delivery failed'})[status] || status; }
  function renderDetail(container, o) {
    const s = workflow(o);
    append(container, node('p', 'Occurrence inspector · ' + (categories[o.classification.category] || 'Other'), 'eyebrow'), node('h2', displayTitle(o)), node('span', (o.message.identifiers || []).join(' · '), 'identifier'), append(node('div', null, 'states'), badge(o), node('span', views[o.view]?.[1] || o.view, 'workflow')));
    const facts = node('dl', null, 'facts');
    fact(facts, 'Source', o.message.source); fact(facts, 'Reported', date(o.message.reported_at)); fact(facts, 'Channel', o.identity.channel); fact(facts, 'Classification', (categories[o.classification.category] || 'Other') + ' · ' + o.classification.origin); fact(facts, 'Occurrence', id(o));
    if (s.acknowledged_by) fact(facts, 'Acknowledged', s.acknowledged_by + ' · ' + date(s.acknowledged_at));
    if (s.closed_by) fact(facts, 'Closed', s.closed_by + ' · ' + date(s.closed_at));
    container.append(facts);
    const slack = link(o.message.slack_url, '↗ Open source thread in Slack'); if (slack) container.append(slack);
    const actions = node('div', null, 'actions');
    if (s.closed_by || s.closed_at) {
      actions.append(action('↶ Reopen investigation', 'reopen', o));
      if (o.snooze) actions.append(action('◷ Unsnooze rule', 'unsnooze', o));
    }
    else {
      actions.append(action(s.acknowledged_by ? '↶ Undo acknowledge' : '✓ Acknowledge', s.acknowledged_by ? 'unacknowledge' : 'acknowledge', o));
      if (o.snooze) actions.append(action('◷ Unsnooze rule', 'unsnooze', o));
      else { const b = action('◷ Snooze until 06:00 UTC', 'snooze', o); b.disabled ||= !(o.message.identifiers || []).length; if (!(o.message.identifiers || []).length) b.title = 'A parsed source identifier is required'; actions.append(b); }
      actions.append(action('☑ Close investigation', 'close', o));
    }
    const ask = action('✧ Ask Chai', 'investigate', o); ask.disabled ||= outstandingInvestigation(o); actions.append(ask); container.append(actions);
    container.append(node('p', ready() ? 'Actions are saved in this Slack thread before the dashboard confirms them.' : state.pending ? 'Resolve the unconfirmed request before creating another action.' : 'Actions are available when coverage is ready and the snapshot is current.', 'fine'));
    renderSOPs(container, o);
    renderMetrics(container, o);
    if (o.snooze) {
      const box = node('div', null, 'investigation');
      append(box, node('h3', '◷ Active snooze rule'), node('p', o.snooze.pattern, 'evidence'), node('p', 'Scope: channel ' + o.snooze.channel + ' · source ' + o.snooze.source + ' · until ' + date(o.snooze.until), 'fine'), node('p', 'By ' + o.snooze.actor + '. Unsnooze cancels this rule across its scope. Expiry restores the previous workflow; repeated firings do not extend it.', 'fine')); container.append(box);
    }
    if (o.investigation) {
      const investigation = o.investigation;
      const box = node('div', null, 'investigation');
      append(box, node('h3', '✧ Chai · ' + statusLabel(investigation.status)), node('p', investigation.question, 'evidence'), node('p', 'Requested by ' + investigation.requested_by + ' · ' + date(investigation.requested_at) + ' · ' + investigation.request_id, 'fine'));
      if (investigation.reply) append(box, node('h3', 'Chai reply'), node('p', investigation.reply, 'evidence'));
      container.append(box);
    }
    append(container, node('h3', 'Source evidence', 'section-head'), node('p', o.message.text || 'No message text supplied.', 'evidence'), node('p', 'A resolved message does not close an investigation automatically.', 'fine'), node('h3', 'Thread evidence & shared history', 'section-head'));
    for (const reply of o.message.replies || []) {
      const box = node('article', null, 'reply');
      append(box, node('p', (reply.is_chai ? 'Chai · ' : '') + reply.user + ' · ' + slackDate(reply.timestamp), 'fine'), node('p', reply.text, 'reply-text')); container.append(box);
    }
    if (!(o.message.replies || []).length) container.append(node('p', 'No retained thread replies.', 'fine'));
  }
  function sopURL(value) {
    try {
      const url = new URL(value);
      return url.origin === 'https://github.com' && !url.username && !url.password && !url.search && /^\/openshift\/release\/blob\/(main|master)\/docs\/dptp-triage-sop\/[^/]+\.md$/.test(url.pathname) ? url : null;
    } catch { return null; }
  }
  function sopLink(value, source) {
    // GitHub resolves repository-root links inside the selected branch.
    if (!value || /[\u0000-\u0020\\]/.test(value) || value.startsWith('//')) return null;
    try {
      const base = new URL(source);
      const root = base.origin + base.pathname.split('/').slice(0, 5).join('/');
      const url = new URL(value.startsWith('/') ? root + value : value, base);
      return link(url.href, '');
    } catch { return null; }
  }
  function sopInline(container, source, url) {
    const tokens = /(`+)([^`]*?)\1|\[([^\]\n]+)\]\(([^\s)]+)\)|\*\*(.+?)\*\*/g;
    let cursor = 0;
    for (const match of source.matchAll(tokens)) {
      container.append(node('span', source.slice(cursor, match.index)));
      if (match[1]) container.append(node('code', match[2]));
      else if (match[3]) {
        const anchor = sopLink(match[4], url);
        if (anchor) { sopInline(anchor, match[3], url); container.append(anchor); }
        else container.append(node('span', match[0]));
      } else {
        const strong = node('strong'); sopInline(strong, match[5], url); container.append(strong);
      }
      cursor = match.index + match[0].length;
    }
    container.append(node('span', source.slice(cursor)));
  }
  function sopHeading(line) {
    return line.match(/^ {0,3}(#{1,6})\s+(.+?)\s*#*\s*$/);
  }
  function sopFence(line) { return line.match(/^\s*(`{3,}|~{3,})(.*)$/); }
  function closesSOPFence(line, fence) {
    const match = sopFence(line);
    return match && match[1][0] === fence[0] && match[1].length >= fence.length && !match[2].trim();
  }
  function sopSection(markdown, url) {
    const lines = markdown.replace(/\r\n?/g, '\n').split('\n');
    let fragment = new URL(url).hash.slice(1);
    try { fragment = decodeURIComponent(fragment); } catch { /* Keep malformed anchors unmatched. */ }
    if (!fragment) return {lines, note: ''};
    const headings = [];
    const slugs = new Map();
    let fence = '';
    for (let index = 0; index < lines.length; index++) {
      if (fence) { if (closesSOPFence(lines[index], fence)) fence = ''; continue; }
      const opening = sopFence(lines[index]);
      if (opening) { fence = opening[1]; continue; }
      const heading = sopHeading(lines[index]);
      if (!heading) continue;
      const slug = heading[2].replace(/\[([^\]]+)\]\([^)]*\)/g, '$1').replace(/<[^>]*>/g, '').replace(/[`*_~]/g, '').toLowerCase().replace(/[^\p{L}\p{N}_\-\s]/gu, '').replace(/\s/g, '-');
      let unique = slug;
      let suffix = 0;
      while (slugs.has(unique)) unique = slug + '-' + (++suffix);
      slugs.set(unique, true);
      headings.push({index, level: heading[1].length, slug: unique});
    }
    const start = headings.findIndex(heading => heading.slug === fragment);
    if (start < 0) return {lines, note: 'Linked section was not found; showing the full SOP.'};
    const heading = headings[start];
    const end = headings.slice(start + 1).find(next => next.level <= heading.level)?.index || lines.length;
    return {lines: lines.slice(heading.index, end), note: 'Linked section'};
  }
  function sopCells(line) { return line.trim().replace(/^\|/, '').replace(/\|$/, '').split(/(?<!\\)\|/).map(cell => cell.trim().replace(/\\\|/g, '|')); }
  function renderSOPMarkdown(container, markdown, url) {
    const section = sopSection(markdown, url);
    if (section.note) container.append(node('p', section.note, 'fine sop-section-note'));
    const lines = section.lines;
    let paragraph = [];
    let lists = [];
    const inline = (tag, value) => { const element = node(tag); sopInline(element, value, url); return element; };
    const flush = () => { if (paragraph.length) container.append(inline('p', paragraph.join('\n'))); paragraph = []; };
    for (let index = 0; index < lines.length; index++) {
      const line = lines[index];
      const opening = sopFence(line);
      if (opening) {
        flush(); lists = [];
        const command = [];
        while (++index < lines.length && !closesSOPFence(lines[index], opening[1])) command.push(lines[index]);
        container.append(append(node('pre'), node('code', command.join('\n')))); continue;
      }
      if (!line.trim()) { flush(); continue; }
      const heading = sopHeading(line);
      if (heading) { flush(); lists = []; container.append(inline('h' + Math.min(6, heading[1].length + 2), heading[2])); continue; }
      if (line.includes('|') && /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[index + 1] || '')) {
        flush(); lists = [];
        const table = node('table');
        const head = node('tr'); sopCells(line).forEach(cell => head.append(inline('th', cell)));
        table.append(append(node('thead'), head));
        const body = node('tbody'); index++;
        while (index + 1 < lines.length && lines[index + 1].trim() && lines[index + 1].includes('|')) {
          const row = node('tr'); sopCells(lines[++index]).forEach(cell => row.append(inline('td', cell))); body.append(row);
        }
        table.append(body); container.append(append(node('div', null, 'sop-table'), table)); continue;
      }
      const bullet = line.match(/^(\s*)([-+*]|\d+[.)])\s+(.+)$/);
      if (bullet) {
        flush(); const indent = bullet[1].length; const type = /^\d/.test(bullet[2]) ? 'ol' : 'ul';
        while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
        if (lists.length && lists[lists.length - 1].indent === indent && lists[lists.length - 1].type !== type) lists.pop();
        if (!lists.length || lists[lists.length - 1].indent < indent) {
          const list = node(type);
          if (type === 'ol') list.start = parseInt(bullet[2], 10);
          (lists.length ? lists[lists.length - 1].last : container).append(list);
          lists.push({indent, type, list, last: null});
        }
        const current = lists[lists.length - 1]; current.last = inline('li', bullet[3]); current.list.append(current.last); continue;
      }
      if (lists.length && /^\s+/.test(line)) { lists[lists.length - 1].last.append(inline('p', line.trim())); continue; }
      lists = []; paragraph.push(line);
    }
    flush();
  }
  async function loadSOP(occurrence, url) {
    if (sopCache.get(url)?.loading || typeof sopCache.get(url)?.markdown === 'string') return;
    const entry = {loading: true, error: ''}; sopCache.set(url, entry); render();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 12000);
    try {
      const response = await fetch('/api/sop?occurrence_id=' + encodeURIComponent(id(occurrence)) + '&url=' + encodeURIComponent(url), {credentials: 'same-origin', signal: controller.signal, headers: {Accept: 'application/json'}});
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || 'SOP is unavailable (HTTP ' + response.status + ').');
      if (typeof result.markdown !== 'string' || result.markdown.length > 262144) throw new Error('The SOP response is invalid or too large.');
      entry.markdown = result.markdown;
    } catch (err) {
      entry.error = err.name === 'AbortError' ? 'SOP request timed out after 12 seconds.' : err.message || 'SOP could not be loaded.';
    } finally { window.clearTimeout(timeout); entry.loading = false; render(); }
  }
  function renderSOPContent(body, occurrence, url) {
    body.replaceChildren();
    const entry = sopCache.get(url);
    if (typeof entry?.markdown === 'string') renderSOPMarkdown(body, entry.markdown, url);
    else if (entry?.error) append(body, node('p', entry.error, 'error-text'), button('Retry loading SOP', () => loadSOP(occurrence, url)));
    else body.append(node('p', entry?.loading ? 'Loading SOP…' : 'Expand to load the SOP.', 'fine'));
  }
  function renderSOPs(container, occurrence) {
    const urls = [...new Set((occurrence.message.sop_urls || []).filter(value => typeof value === 'string' && sopURL(value)))];
    if (!urls.length) return;
    const section = node('section', null, 'sops');
    section.setAttribute('aria-label', 'Alert SOPs'); section.append(node('h3', 'Triage SOP' + (urls.length > 1 ? 's' : '')));
    for (const url of urls) {
      const details = node('details', null, 'sop');
      const key = id(occurrence) + '\n' + url;
      details.open = sopDisclosures.has(key);
      const name = new URL(url).pathname.split('/').pop();
      const summary = node('summary', name.replace(/\.md$/, '').replace(/[-_]/g, ' '));
      const original = link(url, '↗ Open original SOP on GitHub');
      const heading = append(node('div', null, 'sop-heading'), original, details);
      details.append(summary);
      const body = node('div', null, 'sop-body'); body.setAttribute('aria-live', 'polite');
      if (details.open) renderSOPContent(body, occurrence, url);
      details.append(body);
      details.addEventListener('toggle', () => {
        if (details.open) { sopDisclosures.add(key); renderSOPContent(body, occurrence, url); if (!sopCache.has(url)) loadSOP(occurrence, url); }
        else sopDisclosures.delete(key);
      });
      section.append(heading);
    }
    container.append(section);
  }
  const metricsCache = new Map();
  const metricsDisclosures = new Set();
  const metricsSelections = new Map();
  const metricAlerts = o => Array.isArray(o.message.prometheus_alerts) ? [...new Set(o.message.prometheus_alerts.filter(a => typeof a === 'string' && a.length))] : [];
  const metricKey = (o, alert) => JSON.stringify([id(o), alert]);
  const metricsEnabled = () => state.snapshot?.monitoring?.enabled === true && state.snapshot.monitoring.source === 'core-ci';
  async function loadMetrics(o, alert, refresh = false) {
    if (!metricsEnabled() || !metricAlerts(o).includes(alert)) return;
    const key = metricKey(o, alert);
    if (metricsCache.get(key)?.loading || (!refresh && metricsCache.get(key)?.data)) return;
    const entry = {loading: true, error: ''}; metricsCache.set(key, entry); render();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 20000);
    try {
      const response = await fetch('/api/metrics?occurrence_id=' + encodeURIComponent(id(o)) + '&alert=' + encodeURIComponent(alert), {credentials: 'same-origin', signal: controller.signal, headers: {Accept: 'application/json'}});
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || 'Metrics unavailable (HTTP ' + response.status + ').');
      if (result.source !== 'core-ci' || result.alert !== alert || !result.rule || !result.window || !Array.isArray(result.series)) throw new Error('Unexpected metrics response from core-ci.');
      entry.data = result;
    } catch (err) {
      entry.error = err.name === 'AbortError' ? 'Metrics request timed out after 20 seconds.' : err.message || 'Metrics could not be loaded.';
    } finally { window.clearTimeout(timeout); entry.loading = false; render(); }
  }
  function metricsChart(container, data) {
    const start = Date.parse(data.window.start), end = Date.parse(data.window.end), step = Number(data.window.step_seconds) * 1000;
    const valid = p => Number.isFinite(Date.parse(p?.at)) && Date.parse(p.at) >= start && Date.parse(p.at) <= end && typeof p.value === 'number' && Number.isFinite(p.value);
    const series = data.series.slice(0, 12);
    const values = series.flatMap(s => (Array.isArray(s.points) ? s.points : []).filter(valid).map(p => p.value));
    if (!data.series.length) { container.append(node('p', 'No series returned. This is unavailable evidence, not a healthy result.', 'fine')); return; }
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start || !Number.isFinite(step) || step <= 0 || !values.length) {
      container.append(node('p', 'No finite samples available in the reported window. Missing data is not zero or a healthy result.', 'fine')); return;
    }
    const scale = Math.max(1, ...values.map(v => Math.abs(v)));
    let low = Math.min(...values.map(v => v / scale)), high = Math.max(...values.map(v => v / scale));
    if (low === high) { low -= 0.1; high += 0.1; }
    const svgNode = (tag, attrs, content) => {
      const n = document.createElementNS('http://www.w3.org/2000/svg', tag);
      for (const [key, value] of Object.entries(attrs || {})) n.setAttribute(key, String(value));
      if (content !== undefined) n.textContent = content;
      return n;
    };
    const svg = svgNode('svg', {viewBox: '0 0 640 200', role: 'img', 'aria-label': values.length + ' finite samples from ' + date(data.window.start) + ' to ' + date(data.window.end) + '. Gaps indicate missing samples. Values are expression results, not live firing state.', class: 'metrics-chart'});
    svg.append(svgNode('title', {}, 'Core-ci alert expression results. Each plotted sample has a time and value tooltip.'));
    svg.append(svgNode('path', {d: 'M65 15 V165 H625', class: 'metrics-axis'}));
    svg.append(svgNode('text', {x: 60, y: 20, 'text-anchor': 'end'}, String(Math.max(...values))), svgNode('text', {x: 60, y: 165, 'text-anchor': 'end'}, String(Math.min(...values))), svgNode('text', {x: 65, y: 190}, date(data.window.start)), svgNode('text', {x: 625, y: 190, 'text-anchor': 'end'}, date(data.window.end)));
    const legend = node('ul', null, 'metrics-legend');
    series.forEach((s, index) => {
      const points = Array.isArray(s.points) ? s.points : [];
      const label = Object.entries(s.labels || {}).map(([k, v]) => k + '=' + String(v)).join(', ') || 'Unlabelled series';
      const className = 'metrics-series metrics-color-' + index % 6;
      let segment = [], previous = null, last = null, count = 0;
      const flush = () => { if (segment.length > 1) svg.append(svgNode('polyline', {points: segment.join(' '), class: className})); segment = []; };
      for (const p of points) {
        const at = Date.parse(p?.at);
        if (!valid(p)) { flush(); previous = null; continue; }
        if (previous !== null && (at <= previous || at - previous > step * 1.5)) flush();
        const x = 65 + (at - start) / (end - start) * 560, y = 165 - (p.value / scale - low) / (high - low) * 150;
        segment.push(x.toFixed(2) + ',' + y.toFixed(2));
        const dot = svgNode('circle', {cx: x.toFixed(2), cy: y.toFixed(2), r: 2, class: className});
        dot.append(svgNode('title', {}, label + ' · ' + date(p.at) + ' · value ' + p.value)); svg.append(dot);
        previous = at; last = p; count++;
      }
      flush();
      legend.append(node('li', label + ' · ' + count + ' samples' + (last ? ' · last ' + last.value + ' at ' + date(last.at) : ' · no finite samples'), 'metrics-color-' + index % 6));
    });
    append(container, svg, legend);
    if (data.series.length > series.length) container.append(node('p', 'Chart limited to the first 12 series.', 'fine'));
  }
  function renderMetricsContent(body, o, alert) {
    body.replaceChildren();
    body.append(node('p', 'Read-only inspection of source core-ci. The Slack notification may refer to another source. Metric values do not establish current firing state or change workflow status.', 'fine'));
    if (!metricsEnabled()) { body.append(node('p', 'Core-ci metrics are disabled or not configured for this dashboard.', 'fine')); return; }
    const entry = metricsCache.get(metricKey(o, alert));
    if (entry?.loading) { body.append(node('p', 'Loading from core-ci…', 'fine')); return; }
    if (entry?.error) body.append(node('p', entry.error, 'error-text'));
    body.append(button(entry?.error ? 'Retry metrics' : entry?.data ? 'Refresh metrics' : 'Load from core-ci', () => loadMetrics(o, alert, true)));
    if (!entry?.data) return;
    const data = entry.data, rule = data.rule;
    if (data.demo === true) body.append(node('p', 'Synthetic demo metrics', 'metrics-demo'));
    const facts = node('dl', null, 'facts');
    fact(facts, 'Source', data.source); fact(facts, 'Alert', alert); fact(facts, 'Rule', rule.namespace + ' / ' + rule.resource); fact(facts, 'Group', rule.group || 'Not available'); fact(facts, 'For', rule.for || 'Not specified');
    fact(facts, 'Window', date(data.window.start) + ' to ' + date(data.window.end) + ' · step ' + data.window.step_seconds + 's');
    append(body, facts, node('pre', rule.expression, 'metrics-expression'), node('p', 'Expression results may exclude non-firing periods. Missing or nonfinite samples are gaps, never zero-filled.', 'fine'));
    metricsChart(body, data);
    for (const warning of Array.isArray(data.warnings) ? data.warnings : []) body.append(node('p', warning, 'error-text'));
    if (data.truncated) body.append(node('p', 'Metrics response was truncated; some evidence is unavailable.', 'error-text'));
  }
  function renderMetrics(container, o) {
    const alerts = metricAlerts(o);
    if (!alerts.length) return;
    const details = node('details', null, 'metrics');
    details.open = metricsDisclosures.has(id(o));
    details.append(node('summary', 'Core-ci metrics'));
    const body = node('div', null, 'metrics-body'); body.setAttribute('aria-live', 'polite');
    const selected = metricsSelections.get(id(o));
    const alert = alerts.includes(selected) ? selected : alerts[0];
    metricsSelections.set(id(o), alert);
    if (alerts.length > 1) {
      const label = node('label', 'Alert rule '), select = node('select');
      for (const name of alerts) { const option = node('option', name); option.value = name; option.selected = name === alert; select.append(option); }
      select.setAttribute('aria-label', 'Alert rule to inspect in core-ci');
      select.addEventListener('change', () => { metricsSelections.set(id(o), select.value); renderMetricsContent(body, o, select.value); });
      label.append(select); details.append(label);
    }
    details.append(body);
    if (details.open) renderMetricsContent(body, o, alert);
    details.addEventListener('toggle', () => {
      if (details.open) { metricsDisclosures.add(id(o)); renderMetricsContent(body, o, metricsSelections.get(id(o)) || alert); }
      else metricsDisclosures.delete(id(o));
    });
    container.append(details);
  }
  function slackDate(value) { return /^\d+\.\d+$/.test(text(value)) ? date(Number(value) * 1000) : date(value); }
  const reportLabels = {
    pending_workloads_scan: 'Pending workloads',
    capacity_autoscale_snapshot: 'Capacity & autoscaling',
    ci_control_plane_deployments: 'Control plane deployments',
    prow_app_ci_health: 'Prow & app.ci',
    build_farm_jobs_infra: 'Build farm jobs & infrastructure',
    csr_nodes_machine_health: 'CSRs, nodes & machines'
  };
  function reportLabel(task) { return Object.hasOwn(reportLabels, task) ? reportLabels[task] : task; }
  function taskName(o) {
    if (text(o.message.scheduled_task).trim()) return text(o.message.scheduled_task).trim();
    const match = (text(o.message.title) + '\n' + text(o.message.text)).match(/\[Scheduled task:\s*([^\]\n]+)\]/i);
    return match ? match[1].split('|')[0].trim().replace(/^`+|`+$/g, '') || 'Scheduled report' : text(o.message.title).trim() || 'Scheduled report';
  }
  function reportBody(o) {
    // Only remove a complete leading task header. Unexpected source formats stay intact.
    return text(o.message.text).replace(/^\s*\[Scheduled task:[^\]\n]+\]\s*\n?/i, '').trim();
  }
  function reportHeadline(o) {
    return reportBody(o).split('\n').find(line => line.trim())?.trim() || 'No report text supplied.';
  }
  function renderReportBody(container, source) {
    const lines = source.split('\n');
    let paragraph = [];
    let bullets = null;
    const flush = () => {
      if (paragraph.length) container.append(node('p', paragraph.join('\n')));
      paragraph = [];
    };
    for (let index = 0; index < lines.length; index++) {
      const line = lines[index];
      const bullet = line.match(/^\s*(?:[•*-]|\d+[.)])\s+(.+)$/);
      if (!line.trim()) { flush(); bullets = null; continue; }
      if (bullet) {
        flush();
        if (!bullets) { bullets = node('ul'); container.append(bullets); }
        bullets.append(node('li', bullet[1]));
        continue;
      }
      bullets = null;
      const next = lines[index + 1] || '';
      const heading = line.match(/^#{1,6}\s+(.+)$/);
      if (heading || (!paragraph.length && line.length <= 160 && /^\s*[•*-]\s+/.test(next))) {
        flush(); container.append(node('h3', heading ? heading[1] : line)); continue;
      }
      paragraph.push(line);
    }
    flush();
    if (!source) container.append(node('p', 'No report text supplied.', 'muted'));
  }
  function reportCoverage(source) {
    const lines = source.split('\n');
    const excerpt = [];
    for (let index = 0; index < lines.length; index++) {
      const label = lines[index].trim().replace(/^(?:[•*-]|\d+[.)])\s+/, '').replace(/^#{1,6}\s+/, '').replace(/^\*{1,2}(.+?)\*{1,2}:?$/, '$1');
      if (!/^(?:coverage(?: and limits| gap| gaps| limits?| limitations)?|registry and coverage|limitations?|limits?|scope)(?::.*)?$/i.test(label)) continue;
      excerpt.push(lines[index]);
      // Explicit section headings can introduce bullets; never absorb unrelated prose.
      if (label.includes(':') && label.split(':').slice(1).join(':').trim()) continue;
      let next = index + 1;
      while (next < lines.length && !lines[next].trim()) next++;
      while (next < lines.length && /^\s*(?:[•*-]|\d+[.)])\s+/.test(lines[next])) {
        excerpt.push(lines[next++]);
        while (next < lines.length && /^\s+\S/.test(lines[next]) && !/^\s*(?:[•*-]|\d+[.)])\s+/.test(lines[next])) excerpt.push(lines[next++]);
      }
      index = next - 1;
    }
    return excerpt.join('\n');
  }
  function reportDisclosure(key, className) {
    const details = node('details', null, className);
    details.open = state.reportDisclosures.has(key);
    details.addEventListener('toggle', () => {
      if (details.open) state.reportDisclosures.add(key);
      else state.reportDisclosures.delete(key);
    });
    return details;
  }
  function originalReport(o) {
    const details = reportDisclosure('source:' + id(o), 'report-source');
    append(details, node('summary', 'Original Slack message'), node('p', o.message.text || 'No message text supplied.', 'evidence'));
    return details;
  }
  function renderReports(container, reports) {
    container.append(node('p', 'Green identifies scheduled Chai reports, not infrastructure health. Findings and coverage are source text; reports are excluded from alert counts.', 'banner'));
    const groups = new Map();
    for (const report of reports) {
      const key = taskName(report);
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(report);
    }
    const query = state.query.toLowerCase();
    const matching = [...groups].filter(([task, history]) => [task, reportLabel(task)].some(label => label.toLowerCase().includes(query)) || history.some(matchesSearch));
    if (!matching.length) { container.append(node('p', 'No scheduled reports match this search in the retained history.', 'empty')); return; }
    if (!matching.some(([task]) => task === state.selectedReport)) state.selectedReport = matching[0][0];
    const workspace = node('div', null, 'report-workspace');
    const overview = node('section', null, 'report-overview'); overview.setAttribute('aria-label', 'Scheduled report tasks');
    append(overview, append(node('div', null, 'list-head'), node('span', matching.length + ' tasks'), node('span', 'Latest per task')));
    for (const [task, history] of matching) {
      const latest = history[0];
      const card = button('', () => { state.selectedReport = task; render(); }, 'report-task');
      card.setAttribute('aria-pressed', String(task === state.selectedReport));
      append(card, node('strong', reportLabel(task)), node('span', task, 'identifier'), node('time', date(latest.message.reported_at), 'report-time'), node('span', reportHeadline(latest), 'report-headline'));
      overview.append(card);
    }
    const [task, history] = matching.find(([key]) => key === state.selectedReport);
    const latest = history[0];
    const detail = node('article', null, 'report-detail'); detail.setAttribute('aria-label', 'Selected scheduled report');
    append(detail, append(node('div', null, 'report-detail-meta'), badge(latest), node('time', 'Latest · ' + date(latest.message.reported_at), 'fine')), node('h2', reportLabel(task)), node('p', task, 'identifier'), link(latest.message.slack_url, '↗ Open latest report in Slack'));
    const body = reportBody(latest);
    const coverage = reportCoverage(body);
    if (coverage) append(detail, append(node('aside', null, 'report-coverage'), node('h3', 'Source coverage & limits'), node('p', coverage)));
    const readable = node('div', null, 'report-body'); renderReportBody(readable, body);
    append(detail, readable, originalReport(latest));
    const previous = history.slice(1);
    const archive = reportDisclosure('archive:' + task, 'report-archive');
    archive.append(node('summary', 'Earlier reports · ' + previous.length + ' retained'));
    for (const report of previous) {
      const entry = reportDisclosure('history:' + id(report), 'report-history');
      const content = node('div', null, 'report-body'); renderReportBody(content, reportBody(report));
      append(entry, node('summary', date(report.message.reported_at)), link(report.message.slack_url, '↗ Open report in Slack'), content, originalReport(report));
      archive.append(entry);
    }
    if (!previous.length) archive.append(node('p', 'No earlier reports retained for this task.', 'fine'));
    append(detail, archive); append(workspace, overview, detail); container.append(workspace);
  }
  function nextSix() { const until = new Date(); until.setUTCHours(6, 0, 0, 0); if (until <= new Date()) until.setUTCDate(until.getUTCDate() + 1); return until; }
  const escapeRegex = value => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  function openDialog(kind, occurrence) {
    if (!ready() || (kind === 'investigate' && outstandingInvestigation(occurrence))) return;
    state.dialogKind = kind; state.dialogOccurrence = occurrence; state.lastFocus = document.activeElement;
    const snooze = kind === 'snooze';
    el('dialog-title').textContent = snooze ? 'Snooze · scoped rule preview' : 'Ask Chai · request preview';
    el('dialog-context').textContent = displayTitle(occurrence) + ' · ' + occurrence.message.source + ' · channel ' + occurrence.identity.channel;
    el('draft-label').textContent = snooze ? 'Identifier regular expression (RE2 syntax)' : 'Question for Chai';
    el('draft').maxLength = snooze ? 512 : 4000;
    el('draft').value = snooze ? '^(?:' + (occurrence.message.identifiers || []).map(escapeRegex).join('|') + ')$' : 'Please investigate ' + displayTitle(occurrence) + '. Use the source evidence and thread context, explain likely causes and suggest read-only checks. State uncertainties and do not perform remediation.';
    el('confirm').textContent = snooze ? '◷ Save snooze rule' : '✧ Send request to Chai';
    el('form-error').textContent = ''; updatePreview(); el('action-dialog').showModal(); el('draft').focus();
  }
  function closeDialog() { el('action-dialog').close(); state.dialogOccurrence = null; state.lastFocus?.focus(); }
  function updatePreview() {
    const o = state.dialogOccurrence; if (!o) return;
    const draft = el('draft').value.trim();
    if (state.dialogKind === 'investigate') {
      el('preview').textContent = 'DPTP-bot → configured Chai identity\nDestination: the selected source thread ' + id(o) + '\n\n' + draft + '\n\nThe server includes the source permalink, request ID, and read-only evidence instructions. Sending does not acknowledge or close this occurrence.';
      return;
    }
    const scoped = items().filter(other => !isReport(other) && other.identity.channel === o.identity.channel && other.message.source === o.message.source);
    let matching = 'Match count is validated by the server on save.';
    // Browser preview deliberately accepts only literal alternatives; Go validates the full RE2 rule.
    const literal = '^(' + (o.message.identifiers || []).map(escapeRegex).join('|') + ')$';
    const defaultPattern = '^(?:' + (o.message.identifiers || []).map(escapeRegex).join('|') + ')$';
    if (draft === defaultPattern || draft === literal) { const identifiers = new Set(o.message.identifiers || []); matching = scoped.filter(other => (other.message.identifiers || []).some(identifier => identifiers.has(identifier))).length + ' retained occurrences match this identifier set in scope.'; }
    el('preview').textContent = 'Scope: channel ' + o.identity.channel + ' / source ' + o.message.source + '\nPattern: ' + draft + '\nReturn: ' + date(nextSix()) + ' (server computes the exact deadline)\n' + matching + '\n\nEdited regex rules are evaluated by the server. Matching new occurrences share this rule. Repeat firings never extend it. Slack messages continue; previous workflow is restored on expiry. Unsnooze cancels the rule throughout this scope.';
  }
  async function refresh() {
    if (state.fetching) return;
    state.fetching = true; renderStatus();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 12000);
    try {
      const response = await fetch('/api/snapshot', {credentials: 'same-origin', cache: 'no-store', signal: controller.signal, headers: {Accept: 'application/json'}});
      if (!response.ok) throw new Error(response.status === 401 || response.status === 403 ? 'Authentication required or operator access denied. Sign in through the configured SSO proxy, then refresh.' : 'Snapshot failed (HTTP ' + response.status + '). Actions are paused.');
      const snapshot = await response.json();
      if (!snapshot.status || !Array.isArray(snapshot.occurrences)) throw new Error('The server returned an invalid snapshot. Actions are paused.');
      state.snapshot = snapshot; state.fetchError = ''; restorePending(snapshot.user);
    } catch (err) { state.fetchError = err.name === 'AbortError' ? 'Snapshot timed out after 12 seconds. Actions are paused; refresh to retry.' : err.message || 'Snapshot unavailable. Actions are paused.'; }
    finally { window.clearTimeout(timeout); state.fetching = false; render(); }
  }
  async function sendNew(kind, occurrence, draft = '') {
    if (!ready()) return;
    if (!globalThis.crypto?.randomUUID) { error('A secure browser context with crypto.randomUUID is required for actions.'); return; }
    const request = {occurrence_id: id(occurrence), kind, expected_revision: occurrence.identity.revision, request_id: crypto.randomUUID()};
    if (kind === 'snooze') request.pattern = draft;
    if (kind === 'investigate') request.question = draft;
    const user = state.snapshot.user;
    if (!retainPending(user, request)) return;
    state.pending = request; state.pendingUser = user; state.pendingUnconfirmed = false;
    await sendPending();
  }
  async function sendPending() {
    if (!state.pending || pendingExpired() || state.sending || state.snapshot?.status?.phase !== 'ready' || state.fetchError || state.storageError || state.pendingUser !== state.snapshot.user) return;
    const request = state.pending;
    const user = state.pendingUser;
    if (!retainPending(user, request)) return;
    const previouslyUnconfirmed = state.pendingUnconfirmed;
    state.pendingUnconfirmed = true;
    state.sending = true; error(''); announce('Saving request in the source thread…'); render();
    let refreshAfter = false;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 25000);
    try {
      const response = await fetch('/api/actions', {method: 'POST', credentials: 'same-origin', signal: controller.signal, headers: {'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': state.snapshot.csrf_token || ''}, body: JSON.stringify(request)});
      let result;
      try { result = await response.json(); } catch { throw new Error('The server response could not be confirmed.'); }
      if (!response.ok) {
        const uncertain = result.code === 'delivery_uncertain' || (response.status >= 500 && !['not_ready', 'delivery_failed'].includes(result.code));
        const unresolved = uncertain || previouslyUnconfirmed;
        if (!unresolved) clearPending(user, request);
        error((result.error || 'Action failed (HTTP ' + response.status + ').') + (unresolved ? ' Delivery remains unconfirmed; retry the retained request only.' : ''));
        if (response.status === 401 || response.status === 403) state.fetchError = 'Authentication or CSRF validation failed. Refresh to restore your authenticated session.';
        refreshAfter = result.code === 'conflict' || result.code === 'not_ready';
        announce('');
      } else {
        if (!result.occurrence?.identity) throw new Error('The server did not confirm the saved occurrence.');
        clearPending(user, request); state.selected = id(result.occurrence); state.view = result.occurrence.view; state.category = ''; state.color = ''; el('color').value = '';
        const index = items().findIndex(o => id(o) === id(result.occurrence));
        if (index >= 0) state.snapshot.occurrences[index] = result.occurrence;
        announce('Saved · ' + ({acknowledge: 'Acknowledged', unacknowledge: 'Acknowledgement undone', close: 'Investigation closed', reopen: 'Investigation reopened', snooze: 'Snooze rule saved', unsnooze: 'Snooze rule canceled', investigate: 'Chai request saved'})[request.kind] + '.');
        refreshAfter = true;
      }
    } catch (err) { error((err.name === 'AbortError' ? 'Request timed out after 25 seconds.' : err.message || 'Request interrupted.') + ' Delivery is unconfirmed. The original request ID is retained for a safe retry.'); announce(''); }
    finally { window.clearTimeout(timeout); state.sending = false; render(); }
    if (refreshAfter) await refresh();
  }
  el('refresh').addEventListener('click', refresh);
  el('retry').addEventListener('click', sendPending);
  el('dismiss-expired').addEventListener('click', dismissExpired);
  el('search').addEventListener('input', event => { state.query = event.target.value; render(); });
  el('color').addEventListener('change', event => { state.color = event.target.value; render(); });
  el('draft').addEventListener('input', updatePreview);
  el('cancel').addEventListener('click', closeDialog); el('dialog-close').addEventListener('click', closeDialog);
  el('action-dialog').addEventListener('cancel', event => { event.preventDefault(); closeDialog(); });
  el('action-form').addEventListener('submit', event => {
    event.preventDefault(); const o = state.dialogOccurrence; const draft = el('draft').value.trim();
    if (!draft) { el('form-error').textContent = 'Enter a value before sending.'; el('draft').focus(); return; }
    if (!ready()) { el('form-error').textContent = 'Coverage or session is unavailable. Close this preview and refresh before sending.'; return; }
    if (items().find(current => id(current) === id(o))?.identity.revision !== o.identity.revision) { el('form-error').textContent = 'This occurrence changed while the preview was open. Close the preview and review the current state.'; return; }
    const kind = state.dialogKind; closeDialog(); sendNew(kind, o, draft);
  });
  refresh(); window.setInterval(refresh, 15000);
})();
