package main

import (
	"os/exec"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// chatUnitHarness exercises the isolated chat controller without browser or network access.
const chatUnitHarness = `
const fs = require('node:fs'), vm = require('node:vm');
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.attributes = {}; this.handlers = {}; this.value = ''; this.textContent = ''; this.scrollHeight = 0; this.scrollTop = 0; this.clientHeight = 0; }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, handler) { this.handlers[name] = handler; }
  removeEventListener(name) { delete this.handlers[name]; }
  focus() { this.focused = true; }
}
const store = new Map(), timers = new Map(); let timerID = 0;
const doc = {hidden: false, handlers: {}, createElement: tag => new Element(tag), addEventListener(name, fn) {this.handlers[name] = fn;}, removeEventListener(name) {delete this.handlers[name];}};
const sandbox = {URL, Date, TextEncoder, AbortController, Map, Set, console, document: doc, crypto: {randomUUID: () => '11111111-1111-4111-8111-111111111111'}, sessionStorage: {getItem: key => store.get(key) || null, setItem: (key, value) => store.set(key, value), removeItem: key => store.delete(key)}, window: {setTimeout: (fn, delay) => {timers.set(++timerID, {fn, delay}); return timerID;}, clearTimeout: id => timers.delete(id)}};
const context = vm.createContext(sandbox);
vm.runInContext(fs.readFileSync('web/chat.js', 'utf8'), context);
vm.runInContext(` + "`" + `
globalThis.root = document.createElement('div');
globalThis.calls = []; globalThis.queue = []; globalThis.authErrors = 0; globalThis.rendered = [];
globalThis.reply = (body, status = 200) => ({ok: status < 400, status, body});
globalThis.conversation = (id = 'one', extra = {}) => ({id, title: 'Investigation ' + id, created_at: '2026-10-30T12:00:00Z', updated_at: '2026-10-30T12:00:00Z', messages: [], ...extra});
globalThis.snapshot = (conversations = [], extra = {}) => reply({enabled: true, user: 'alice', status: {phase: 'ready'}, conversations, ...extra});
globalThis.transport = async (method, body, signal) => {calls.push({method, body, retained: Array.from(store.values()), signal}); const value = queue.shift(); if (value instanceof Error) throw value; return value || snapshot();};
globalThis.setup = () => {globalThis.chat = window.OpsChaiChat.create({root, request: transport, formatDate: value => 'Local ' + value, onAuthError: () => authErrors++, renderBody: (text, reference) => {rendered.push({text, reference}); const node = document.createElement('p'); node.textContent = text; return node;}}); chat.update({user: 'alice', status: {phase: 'ready'}}); return chat;};
globalThis.findAll = (className, parent = root) => [...(parent.className === className ? [parent] : []), ...parent.children.flatMap(child => findAll(className, child))];
globalThis.find = className => findAll(className)[0];
globalThis.input = value => {find('chai-draft').value = value; find('chai-draft').handlers.input();};
globalThis.click = className => find(className).handlers.click();
globalThis.settle = async () => {for (let i = 0; i < 12; i++) await Promise.resolve();};
globalThis.open = async () => {chat.open(); await settle();};
globalThis.posts = () => calls.filter(call => call.method === 'POST');
globalThis.content = node => node.textContent + node.children.map(content).join('');
` + "`" + `, context);
context.store = store; context.timers = timers;
Promise.resolve(vm.runInContext('(async () => {' + process.argv[1] + '})()', context)).then(value => process.stdout.write(JSON.stringify(value)), error => {console.error(error); process.exitCode = 1;});
`

func TestChaiChatController(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for chat controller unit tests")
	}
	for _, tc := range []struct {
		name       string
		expression string
		want       string
	}{
		{
			name:       "open and minimize never submit and retain editable draft",
			expression: `setup(); await open(); input('Draft question'); chat.close(); chat.open(); await settle(); return [posts().length, find('chai-draft').value, find('chai-panel').hidden, find('chai-launcher').attributes['aria-expanded']];`,
			want:       `[0,"Draft question",false,"true"]`,
		},
		{
			name:       "selection and per conversation drafts survive refresh and switching",
			expression: `setup(); queue.push(snapshot([conversation('one'), conversation('two')])); await open(); await click('chai-picker'); await findAll('chai-history-item')[0].handlers.click(); input('First draft'); await click('chai-picker'); await findAll('chai-history-item')[1].handlers.click(); input('Second draft'); await click('chai-picker'); await findAll('chai-history-item')[0].handlers.click(); chat.close(); queue.push(snapshot([conversation('one'), conversation('two')])); chat.open(); await settle(); return [find('chai-draft').value, find('chai-picker').textContent];`,
			want:       `["First draft","Investigation one ▾"]`,
		},
		{
			name:       "explicit send persists exact request before transport",
			expression: `setup(); await open(); input('A question'); queue.push(reply({conversation: conversation()})); await click('chai-send'); const sent = posts()[0]; return [sent.body, JSON.parse(sent.retained[0]), store.size, find('chai-draft').value];`,
			want:       `[{"request_id":"11111111-1111-4111-8111-111111111111","text":"A question"},{"request":{"request_id":"11111111-1111-4111-8111-111111111111","text":"A question"},"uncertain":true},0,""]`,
		},
		{
			name:       "uncertain transport retry uses retained payload despite edited draft",
			expression: `setup(); await open(); input('Original'); queue.push(new Error('Connection lost')); await click('chai-send'); input('Edited'); queue.push(reply({conversation: conversation()})); await click('chai-retry'); return [posts().map(item => item.body.text), posts().map(item => item.body.request_id), store.size];`,
			want:       `[["Original","Original"],["11111111-1111-4111-8111-111111111111","11111111-1111-4111-8111-111111111111"],0]`,
		},
		{
			name:       "definite initial validation rejection releases guard and keeps draft",
			expression: `setup(); await open(); input('Question'); queue.push(reply({code: 'validation', error: 'Invalid question'}, 400)); await click('chai-send'); return [store.size, find('chai-draft').value, find('chai-send').disabled];`,
			want:       `[0,"Question",false]`,
		},
		{
			name:       "definite retry rejection cannot release earlier ambiguous guard",
			expression: `setup(); await open(); input('Question'); queue.push(reply({error: 'Upstream unavailable'}, 502)); await click('chai-send'); queue.push(reply({code: 'not_ready'}, 503)); await click('chai-retry'); return [store.size, find('chai-send').disabled, find('chai-retry').hidden];`,
			want:       `[1,true,false]`,
		},
		{
			name:       "reload restores guard without automatic post and snapshot confirms request",
			expression: `setup(); await open(); input('Question'); queue.push(new Error('Lost')); await click('chai-send'); chat.destroy(); setup(); queue.push(snapshot([conversation('confirmed', {messages: [{request_id: '11111111-1111-4111-8111-111111111111', text: 'Question', timestamp: '2026-10-30T12:00:00Z', is_operator: true}]})])); await open(); return [posts().length, store.size, find('chai-picker').textContent];`,
			want:       `[1,0,"Investigation confirmed ▾"]`,
		},
		{
			name:       "storage failure prevents post",
			expression: `setup(); await open(); input('Question'); sessionStorage.setItem = () => {throw new Error('Unavailable');}; await click('chai-send'); return [posts().length, find('chai-send').disabled, content(find('chai-error')).includes('session storage')];`,
			want:       `[0,true,true]`,
		},
		{
			name:       "UTF8 limit counts bytes and prevents oversized post",
			expression: `setup(); await open(); input('😀'.repeat(1001)); await click('chai-send'); return [posts().length, find('chai-send').disabled, find('chai-counter').textContent, find('chai-draft').attributes['aria-invalid']];`,
			want:       `[0,true,"4004 / 4000 bytes","true"]`,
		},
		{
			name:       "operator swap clears private content drafts and separates pending guard",
			expression: `setup(); queue.push(snapshot([conversation('private')])); await open(); input('Alice private'); queue.push(new Error('Lost')); await click('chai-send'); chat.update({user: 'bob', status: {phase: 'ready'}}); await settle(); return [find('chai-draft').value, content(find('chai-thread')).includes('private'), find('chai-retry').hidden, Array.from(store.keys())];`,
			want:       `["",false,true,["ops-alert-dashboard.chai.pending.v1.alice"]]`,
		},
		{
			name:       "poll identity mismatch pauses and clears private content",
			expression: `setup(); await open(); input('Private'); queue.push(snapshot([conversation('other')], {user: 'bob'})); await chat.refresh(); return [authErrors, find('chai-draft').value, find('chai-send').disabled, timers.size];`,
			want:       `[1,"",true,0]`,
		},
		{
			name:       "alert opening finds linked history after initial fetch without sending",
			expression: `setup(); queue.push(snapshot([conversation('linked', {source_occurrence_id: 'alert'})])); chat.open({identity: {id: 'alert'}, message: {title: 'Alert'}}); await settle(); return [find('chai-picker').textContent, find('chai-draft').value, posts().length];`,
			want:       `["Investigation linked ▾","",0]`,
		},
		{
			name:       "alert default remains editable and includes occurrence only on explicit send",
			expression: `setup(); chat.open({occurrenceID: 'alert', title: 'Alert'}); await settle(); const initial = find('chai-draft').value; input('Custom question'); queue.push(reply({conversation: conversation()})); await click('chai-send'); return [initial, posts()[0].body.occurrence_id, posts()[0].body.text];`,
			want:       `["Analyze this alert: likely cause, evidence, and next read-only checks.","alert","Custom question"]`,
		},
		{
			name:       "safe markdown helper and local Slack timestamps without unsafe source link",
			expression: `setup(); queue.push(snapshot([conversation('one', {slack_url: 'javascript:alert(1)', messages: [{text: '<img onerror=evil()>', timestamp: '1793361600.000001', is_chai: true}]})])); await open(); await click('chai-picker'); await click('chai-history-item'); return [findAll('chai-link').length, rendered.at(-1), content(find('chai-thread')).includes('Local 2026-10-30')];`,
			want:       `[0,{"text":"<img onerror=evil()>","reference":"2026-10-30T12:00:00.000Z"},true]`,
		},
		{
			name:       "hidden document stops polling and concurrent refresh never overlaps",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); chat.open(); await chat.refresh(); const count = calls.length; resolve(snapshot()); await settle(); document.hidden = true; document.handlers.visibilitychange(); await chat.refresh(); return [count, calls.length, timers.size];`,
			want:       `[1,1,0]`,
		},
		{
			name:       "editing during delivery preserves new text in confirmed conversation",
			expression: `setup(); await open(); input('Original'); let resolve; queue.push(new Promise(done => {resolve = done;})); const sending = click('chai-send'); input('Next question'); resolve(reply({conversation: conversation()})); await sending; return [find('chai-draft').value, find('chai-picker').textContent, posts().length];`,
			want:       `["Next question","Investigation one ▾",1]`,
		},
		{
			name:       "stale operator response cannot restore private conversations",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); chat.open(); chat.update({user: 'bob', status: {phase: 'ready'}}); await settle(); resolve(snapshot([conversation('Alice private')])); await settle(); return [content(find('chai-history')).includes('Alice private'), find('chai-draft').value, find('chai-send').disabled];`,
			want:       `[false,"",true]`,
		},
		{
			name:       "first not ready rejection releases guard and keeps editable text",
			expression: `setup(); await open(); input('Question'); queue.push(reply({code: 'not_ready', error: 'Slack is syncing'}, 503)); await click('chai-send'); return [store.size, find('chai-draft').value, find('chai-retry').hidden];`,
			want:       `[0,"Question",true]`,
		},
		{
			name:       "selected conversation supplies active interest only to explicit cache reads",
			expression: `setup(); queue.push(snapshot([conversation('active')])); await open(); const initial = calls[0].body; await click('chai-picker'); await click('chai-history-item'); queue.push(snapshot([conversation('active')])); await chat.refresh(); return [initial, calls.at(-1).body, posts().length];`,
			want:       `[null,{"conversation_id":"active"},0]`,
		},
		{
			name:       "edited alert draft follows linked conversation found by initial poll",
			expression: `setup(); let resolve; queue.push(new Promise(done => {resolve = done;})); chat.open({occurrenceID: 'alert', title: 'Alert'}); input('Edited alert question'); resolve(snapshot([conversation('linked', {source_occurrence_id: 'alert'})])); await settle(); return [find('chai-picker').textContent, find('chai-draft').value, posts().length];`,
			want:       `["Investigation linked ▾","Edited alert question",0]`,
		},
		{
			name:       "late pre-send snapshot cannot erase confirmed conversation or change send target",
			expression: `setup(); queue.push(snapshot([conversation('history')])); await open(); let resolve; queue.push(new Promise(done => {resolve = done;})); const staleRead = chat.refresh(); input('New question'); queue.push(reply({conversation: conversation('confirmed')})); await click('chai-send'); resolve(snapshot([])); await staleRead; const label = find('chai-picker').textContent; const history = content(find('chai-history')); input('Continue confirmed'); queue.push(reply({conversation: conversation('confirmed')})); await click('chai-send'); return [label, history.includes('Investigation confirmed'), history.includes('Investigation history'), posts()[1].body.conversation_id];`,
			want:       `["Investigation confirmed ▾",true,true,"confirmed"]`,
		},
		{
			name:       "missing selected conversation never appears to be a new conversation",
			expression: `setup(); queue.push(snapshot([conversation('selected')])); await open(); await click('chai-picker'); await click('chai-history-item'); input('Retained draft'); queue.push(snapshot([])); await chat.refresh(); return [find('chai-picker').textContent, find('chai-draft').value, find('chai-send').disabled];`,
			want:       `["Conversation unavailable ▾","Retained draft",true]`,
		},
		{
			name:       "unreadable chat recovery warns and pauses sends without disabling alert coverage",
			expression: `setup(); queue.push(snapshot([conversation('healthy')], {status: {phase: 'degraded', error: 'Chat sending is paused because retained conversation history is unreadable. Alert workflow actions remain available.'}})); await open(); input('Draft remains editable'); return [find('chai-send').disabled, find('chai-draft').value, find('chai-error').textContent, content(find('chai-history')).includes('Investigation healthy'), posts().length];`,
			want:       `[true,"Draft remains editable","Chat sending is paused because retained conversation history is unreadable. Alert workflow actions remain available.",true,0]`,
		},
		{
			name:       "rate limited poll backs off and never posts",
			expression: `setup(); queue.push({...reply({error: 'Slow down'}, 429), retryAfter: 120}); await open(); const delays = Array.from(timers.values()).map(item => item.delay); await chat.refresh(); return [calls.length, posts().length, delays.every(value => value > 100000)];`,
			want:       `[1,0,true]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(node, "-e", chatUnitHarness, tc.expression)
			command.Env = append(command.Environ(), "NODE_OPTIONS=")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run chat unit fixture: %v\n%s", err, output)
			}
			if diff := cmp.Diff(tc.want, string(output)); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
