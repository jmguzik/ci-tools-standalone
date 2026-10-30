'use strict';
(() => {
  const defaultPrompt = 'Analyze this alert: likely cause, evidence, and next read-only checks.';
  const bytes = value => new TextEncoder().encode(value).length;
  const storageKey = user => 'ops-alert-dashboard.chai.pending.v1.' + encodeURIComponent(user);
  const validPending = value => value && typeof value.request_id === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value.request_id) && typeof value.text === 'string' && !!value.text.trim() && bytes(value.text) <= 4000 && (!value.conversation_id || typeof value.conversation_id === 'string') && (!value.occurrence_id || typeof value.occurrence_id === 'string');
  const instant = value => /^\d{10}(?:\.\d+)?$/.test(String(value)) ? new Date(Number(value) * 1000).toISOString() : value;
  /** Keep untrusted Slack links outside executable and credential-bearing URL schemes. */
  const safeURL = value => {
    if (typeof value !== 'string' || /[\u0000-\u0020\\]/.test(value)) return null;
    try { const url = new URL(value); return url.protocol === 'https:' && !url.username && !url.password ? url.href : null; } catch { return null; }
  };
  /** Own one non-modal chat window without changing dashboard navigation or workflow state. */
  function create(options) {
    const root = options.root, doc = root.ownerDocument || document;
    const make = (tag, text, className) => { const element = doc.createElement(tag); if (text != null) element.textContent = String(text); if (className) element.className = className; return element; };
    const button = (text, action, className = 'chai-icon-button') => { const element = make('button', text, className); element.type = 'button'; element.addEventListener('click', action); return element; };
    const date = options.formatDate || (value => new Date(instant(value)).toLocaleString());
    const state = {user: '', snapshot: null, conversations: [], enabled: false, status: null, open: false, history: false, selected: '', context: null, drafts: new Map(), pending: null, pendingUncertain: false, sending: false, loading: false, paused: false, error: '', syncError: false, storageError: '', generation: 0, conversationRevision: 0, timer: null, failures: 0, nextPoll: 0, destroyed: false};
    const launcher = button('✧ Ask Chai', () => state.open ? close() : open(), 'chai-launcher');
    launcher.setAttribute('aria-expanded', 'false'); launcher.setAttribute('aria-controls', 'chai-chat-panel');
    const panel = make('section', null, 'chai-panel'); panel.id = 'chai-chat-panel'; panel.hidden = true; panel.setAttribute('role', 'region'); panel.setAttribute('aria-label', 'Chai assistant');
    const header = make('div', null, 'chai-header'), heading = make('div', null, 'chai-heading'); heading.append(make('strong', '✧ Chai'), make('span', 'Shared Slack conversation', 'chai-subtitle'));
    const controls = make('div', null, 'chai-controls'), minimize = button('−', close); minimize.setAttribute('aria-label', 'Minimize Chai'); controls.append(button('+ New', newConversation, 'chai-new'), minimize); header.append(heading, controls);
    const picker = button('New conversation ▾', () => { state.history = !state.history; render(); }, 'chai-picker'); picker.setAttribute('aria-expanded', 'false'); picker.setAttribute('aria-controls', 'chai-chat-history');
    const history = make('div', null, 'chai-history'); history.id = 'chai-chat-history';
    const context = make('div', null, 'chai-context'), thread = make('div', null, 'chai-thread'); thread.setAttribute('role', 'log'); thread.setAttribute('aria-label', 'Conversation messages'); thread.setAttribute('aria-live', 'polite');
    const status = make('div', null, 'chai-status'); status.setAttribute('role', 'status');
    const error = make('div', null, 'chai-error'); error.setAttribute('role', 'alert');
    const retainedText = make('div', null, 'chai-pending-preview');
    const composer = make('div', null, 'chai-composer'), label = make('label', 'Message Chai'); label.htmlFor = 'chai-chat-draft';
    const draft = make('textarea', null, 'chai-draft'); draft.id = 'chai-chat-draft'; draft.rows = 3; draft.placeholder = 'Ask a question…'; draft.setAttribute('aria-describedby', 'chai-chat-counter');
    draft.addEventListener('input', () => { state.drafts.set(draftKey(), draft.value); renderComposer(); });
    const footer = make('div', null, 'chai-composer-footer'), counter = make('span', null, 'chai-counter'); counter.id = 'chai-chat-counter';
    const send = button('Send to Chai', sendNew, 'chai-send'), retry = button('Retry retained request', sendPending, 'chai-retry'); footer.append(counter, send); composer.append(label, draft, make('span', '@chai-bot is added automatically', 'chai-subtitle'), footer);
    panel.append(header, picker, history, context, thread, status, error, retainedText, retry, composer); root.append(panel, launcher);
    const selected = () => state.conversations.find(item => item.id === state.selected);
    const draftKey = () => state.selected || (state.context ? 'alert:' + state.context.id : 'new');
    const ready = () => !!state.user && !state.paused && state.enabled && state.status?.phase === 'ready' && state.snapshot?.status?.phase === 'ready' && !state.storageError && !state.syncError;
    /** Persist the exact delivery key before a write can reach Slack. */
    function savePending(value, uncertain = state.pendingUncertain) {
      try { const serialized = JSON.stringify({request: value, uncertain}); sessionStorage.setItem(storageKey(state.user), serialized); if (sessionStorage.getItem(storageKey(state.user)) !== serialized) throw new Error('Storage unavailable'); state.pending = value; state.pendingUncertain = uncertain; return true; }
      catch { state.storageError = 'The request could not be retained in session storage. Sending is paused to prevent duplicates.'; render(); return false; }
    }
    /** Release a delivery guard only when its removal can be verified locally. */
    function clearPending() {
      try { sessionStorage.removeItem(storageKey(state.user)); if (sessionStorage.getItem(storageKey(state.user)) !== null) throw new Error('Storage unavailable'); state.pending = null; state.pendingUncertain = false; return true; }
      catch { state.storageError = 'The confirmed request could not be cleared from session storage. Sending is paused.'; return false; }
    }
    /** Recover this operator's guarded request without automatically resending it. */
    function restorePending() {
      try { const raw = sessionStorage.getItem(storageKey(state.user)); if (!raw) return; const retained = JSON.parse(raw); if (!validPending(retained.request) || typeof retained.uncertain !== 'boolean') throw new Error('Invalid retained request'); state.pending = retained.request; state.pendingUncertain = retained.uncertain; }
      catch { state.storageError = 'A retained Chai request could not be read safely. Restore session storage before sending.'; }
    }
    /** Resolve retained delivery uncertainty only from a matching confirmed archive request. */
    function reconcile() {
      if (!state.pending) return;
      const conversation = state.conversations.find(item => (item.messages || []).some(message => message.request_id === state.pending.request_id));
      if (!conversation) return;
      const request = state.pending;
      if (!clearPending()) return;
      const key = request.conversation_id || (request.occurrence_id ? 'alert:' + request.occurrence_id : 'new');
      if (state.drafts.get(key)?.trim() === request.text) state.drafts.delete(key);
      if (!state.selected && (!state.context || state.context.id === request.occurrence_id)) { state.selected = conversation.id; state.context = null; }
      state.error = '';
    }
    /** Keep edits available while readiness and retained requests control sending. */
    function renderComposer() {
      const count = bytes(draft.value.trim()); counter.textContent = count + ' / 4000 bytes';
      draft.setAttribute('aria-invalid', count > 4000 ? 'true' : 'false');
      send.disabled = !ready() || !!state.selected && !selected() || !!state.pending || state.sending || count === 0 || count > 4000;
      retainedText.hidden = !state.pending || state.paused; retainedText.textContent = state.pending && !state.paused ? 'Retained message: ' + state.pending.text : '';
      retry.hidden = !state.pending; retry.disabled = !ready() || state.sending;
    }
    /** Render cached conversation state and preserve the operator's current draft. */
    function render() {
      if (state.destroyed) return;
      panel.hidden = !state.open; launcher.setAttribute('aria-expanded', String(state.open));
      const conversation = selected(); picker.textContent = (conversation?.title || (state.selected ? 'Conversation unavailable' : 'New conversation')) + ' ▾'; picker.setAttribute('aria-expanded', String(state.history));
      history.hidden = !state.history; history.replaceChildren(button('+ New conversation', newConversation, 'chai-new'), make('div', 'History · Last 48 hours', 'chai-subtitle'));
      const list = make('div', null, 'chai-history-list');
      for (const item of state.conversations) {
        const entry = button('', () => { state.selected = item.id; state.context = null; state.history = false; render(); draft.focus(); }, 'chai-history-item');
        entry.setAttribute('aria-pressed', String(item.id === state.selected)); entry.append(make('strong', item.title || 'Conversation'), make('small', date(item.updated_at || item.created_at))); list.append(entry);
      }
      if (!state.conversations.length) list.append(make('p', 'No conversations in the last 48 hours.', 'chai-empty')); history.append(list);
      context.hidden = !state.context; context.textContent = state.context ? 'About alert · ' + state.context.title : '';
      const nearBottom = thread.scrollHeight - thread.scrollTop - thread.clientHeight < 64;
      thread.replaceChildren();
      if (conversation) {
        if (safeURL(conversation.slack_url)) { const link = make('a', 'Open in Slack', 'chai-link'); link.href = safeURL(conversation.slack_url); link.target = '_blank'; link.rel = 'noopener noreferrer'; thread.append(link); }
        for (const message of conversation.messages || []) {
          const article = make('article', null, 'chai-message'), meta = make('div', null, 'chai-message-meta');
          meta.append(make('strong', message.is_chai ? 'Chai' : message.author === state.user ? 'You' : message.author || 'Slack participant'), make('time', date(instant(message.timestamp))));
          const body = make('div', null, 'chai-message-body');
          if (options.renderBody) { const rendered = options.renderBody(message.text || '', instant(message.timestamp)); if (rendered) body.append(rendered); }
          else body.textContent = message.text || '';
          article.append(meta, body); thread.append(article);
        }
      } else { const empty = make('div', null, 'chai-empty'); empty.append(make('h3', state.selected ? 'Conversation unavailable' : 'What can Chai help with?'), make('p', state.selected ? 'This conversation is outside the current snapshot. Refresh or choose a conversation from history. Your draft is preserved.' : 'Investigate an alert or ask about CI operations. Replies appear here and in Slack.')); thread.append(empty); }
      if (nearBottom) thread.scrollTop = thread.scrollHeight;
      status.textContent = state.sending ? 'Sending to Chai…' : state.pending ? 'Delivery is unconfirmed. Retry only the retained request; its text and request ID are preserved.' : state.paused ? 'Session expired. Refresh the dashboard to sign in again.' : !state.user ? 'Waiting for your session…' : state.loading && !state.status ? 'Loading conversations…' : !state.enabled ? 'Chai conversations are unavailable.' : state.status?.phase !== 'ready' || state.snapshot?.status?.phase !== 'ready' ? 'Waiting for Slack coverage before sending.' : state.loading ? 'Syncing…' : state.status?.last_sync ? 'Synced ' + date(state.status.last_sync) : 'Replies refresh while this window is open.';
      const recoveryError = state.snapshot?.status?.phase === 'ready' && state.status?.phase === 'degraded' ? state.status.error : '';
      error.textContent = state.storageError || state.error || recoveryError; error.hidden = !error.textContent;
      draft.value = state.drafts.get(draftKey()) || ''; renderComposer();
    }
    /** Poll only visible open chats and respect the most recent server backoff. */
    function schedule(delay = 5000) {
      window.clearTimeout(state.timer); state.timer = null;
      if (!state.destroyed && state.open && !doc.hidden && !state.paused && state.user) state.timer = window.setTimeout(refresh, Math.max(delay, state.nextPoll - Date.now()));
    }
    /** Bound transport time without interpreting a timeout as a rejected write. */
    async function request(method, body) {
      const controller = new AbortController(), timeout = window.setTimeout(() => controller.abort(), method === 'GET' ? 12000 : 25000);
      try { return await options.request(method, body, controller.signal); } finally { window.clearTimeout(timeout); }
    }
    /** Remove private display state when the authenticated identity is no longer verified. */
    function pause() { state.paused = true; window.clearTimeout(state.timer); state.conversations = []; state.drafts.clear(); state.selected = ''; state.context = null; options.onAuthError?.(); }
    /** Ignore superseded cache reads so they cannot undo a confirmed send or identity change. */
    async function refresh() {
      if (state.destroyed || state.loading || !state.open || doc.hidden || state.paused || !state.user) return;
      if (Date.now() < state.nextPoll) { schedule(); return; }
      const generation = state.generation, conversationRevision = state.conversationRevision; state.loading = true;
      try {
        const response = await request('GET', state.selected ? {conversation_id: state.selected} : null); if (generation !== state.generation || conversationRevision !== state.conversationRevision) return;
        if (response.status === 401 || response.status === 403) { pause(); return; }
        if (!response.ok) { state.nextPoll = Date.now() + Math.max(Number(response.retryAfter) * 1000 || 0, Math.min(60000, 5000 * 2 ** Math.min(state.failures + 1, 4))); throw new Error(response.body?.error || 'Conversation sync failed.'); }
        const body = response.body; if (body?.user && body.user !== state.user) { pause(); return; } if (!body || !Array.isArray(body.conversations) || !body.status || typeof body.enabled !== 'boolean') throw new Error('The server returned an invalid conversation snapshot.');
        state.conversations = body.conversations; state.status = body.status; state.enabled = body.enabled; state.failures = 0; state.nextPoll = 0; state.error = ''; state.syncError = false; reconcile();
        if (state.context) { const linked = state.conversations.find(item => item.source_occurrence_id === state.context.id); if (linked) { const draftValue = state.drafts.get(draftKey()); if (draftValue && draftValue !== defaultPrompt && !state.drafts.has(linked.id)) state.drafts.set(linked.id, draftValue); state.selected = linked.id; state.context = null; } }
      } catch (err) { if (generation === state.generation && conversationRevision === state.conversationRevision) { state.syncError = true; state.error = err.name === 'AbortError' ? 'Conversation sync timed out. Your draft is preserved.' : err.message || 'Conversation sync is unavailable.'; state.failures++; state.nextPoll = Math.max(state.nextPoll, Date.now() + Math.min(60000, 5000 * 2 ** Math.min(state.failures, 4))); } }
      finally { if (generation === state.generation) { state.loading = false; render(); schedule(); } }
    }
    /** Freeze one explicit question and its destination before allocating a delivery key. */
    async function sendNew() {
      const text = draft.value.trim(); if (!ready() || state.selected && !selected() || state.pending || state.sending || !text || bytes(text) > 4000) return;
      if (!globalThis.crypto?.randomUUID) { state.error = 'A secure browser context is required to create a request ID.'; render(); return; }
      const payload = {request_id: crypto.randomUUID(), text}; if (state.selected) payload.conversation_id = state.selected; else if (state.context) payload.occurrence_id = state.context.id;
      if (savePending(payload, false)) await sendPending();
    }
    /** Reuse the retained delivery key and keep uncertainty through subsequent failures. */
    async function sendPending() {
      if (!ready() || !state.pending || state.sending) return;
      const payload = state.pending, user = state.user, generation = state.generation;
      const previouslyUncertain = state.pendingUncertain;
      if (!savePending(payload, true)) return;
      state.sending = true; state.error = ''; render();
      try {
        const response = await request('POST', payload); if (generation !== state.generation || user !== state.user) return;
        if (response.status === 401 || response.status === 403) { pause(); state.error = 'Delivery could not be confirmed before the session ended. The request is retained.'; return; }
        if (!response.ok) {
          const code = response.body?.code;
          if (!previouslyUncertain && ['validation', 'invalid_request', 'invalid_text', 'not_ready', 'disabled', 'conversation_not_found', 'not_found'].includes(code)) clearPending();
          state.error = response.body?.error || 'Delivery could not be confirmed. Retry the retained request.'; return;
        }
        if (!response.body?.conversation?.id || !Array.isArray(response.body.conversation.messages)) throw new Error('The server did not confirm the conversation.');
        state.conversationRevision++;
        const conversation = response.body.conversation, key = payload.conversation_id || (payload.occurrence_id ? 'alert:' + payload.occurrence_id : 'new');
        if (!clearPending()) return;
        const currentDraft = state.drafts.get(key);
        if (currentDraft?.trim() === payload.text) state.drafts.delete(key);
        else if (draftKey() === key && key !== conversation.id && currentDraft) { state.drafts.set(conversation.id, currentDraft); state.drafts.delete(key); }
        state.conversations = [conversation, ...state.conversations.filter(item => item.id !== conversation.id)];
        if (draftKey() === key) { state.selected = conversation.id; state.context = null; state.history = false; }
      } catch (err) { if (generation === state.generation) state.error = (err.name === 'AbortError' ? 'Request timed out.' : err.message || 'Request interrupted.') + ' Delivery is unconfirmed. The original request ID is retained for retry.'; }
      finally { if (generation === state.generation) { state.sending = false; render(); schedule(); } }
    }
    /** Start an unsent standalone draft without touching existing Slack threads. */
    function newConversation() { state.selected = ''; state.context = null; state.history = false; render(); draft.focus(); }
    /** Resume the selected chat or bind an alert draft to its existing source thread. */
    function open(occurrence) {
      state.open = true;
      if (occurrence) {
        const id = occurrence.identity?.id || occurrence.occurrenceID;
        const existing = state.conversations.find(item => item.source_occurrence_id === id);
        state.selected = existing?.id || ''; state.context = existing ? null : {id, title: occurrence.message?.title || occurrence.title || 'Selected alert'};
        if (!existing && !state.drafts.has(draftKey())) state.drafts.set(draftKey(), defaultPrompt);
      }
      render(); draft.focus(); refresh();
    }
    /** Minimize the popup while retaining its selected conversation and unsent drafts. */
    function close() { state.open = false; state.history = false; window.clearTimeout(state.timer); render(); launcher.focus(); }
    /** Reset private state on operator changes and take readiness from the main snapshot. */
    function update(snapshot) {
      const user = snapshot?.user || '';
      if (user !== state.user) {
        state.generation++; window.clearTimeout(state.timer); state.user = user; state.conversations = []; state.drafts.clear(); state.selected = ''; state.context = null; state.pending = null; state.storageError = ''; state.error = ''; state.syncError = false; state.enabled = false; state.status = null; state.loading = false; state.sending = false; state.failures = 0; state.nextPoll = 0;
        if (user) restorePending();
      }
      state.snapshot = snapshot; state.paused = !user || snapshot?.authPaused === true; render(); if (state.open) refresh();
    }
    /** Suspend foreground polling when the browser tab is hidden. */
    function visibility() { if (doc.hidden) window.clearTimeout(state.timer); else if (state.open) refresh(); }
    doc.addEventListener('visibilitychange', visibility);
    /** Invalidate outstanding reads before removing this window and its listeners. */
    function destroy() { state.destroyed = true; state.generation++; window.clearTimeout(state.timer); doc.removeEventListener('visibilitychange', visibility); root.replaceChildren(); }
    render(); return {update, open, close, refresh, destroy};
  }
  window.OpsChaiChat = {create};
})();
