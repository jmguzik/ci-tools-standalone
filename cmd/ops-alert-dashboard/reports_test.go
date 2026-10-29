package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// reportsUnitHarness exercises pure report helpers without browser globals or private source fixtures.
const reportsUnitHarness = `
const fs = require('node:fs'), vm = require('node:vm');
const context = vm.createContext({});
vm.runInContext(fs.readFileSync('web/reports.js', 'utf8'), context);
vm.runInContext("globalThis.fixture = (id, text, run = '', at = '2026-10-29T10:00:00Z') => ({identity: {id, channel: 'C_SYNTHETIC'}, message: {text, scheduled_task: 'pending_workloads_scan', scheduled_run: run, reported_at: at, source: 'synthetic', slack_url: 'https://example.org/' + id}}); globalThis.metrics = text => OpsReports.pending(fixture('root', text)); globalThis.values = result => [result.observations.map(o => [o.context, o.value, o.coverage]), result.partial];", context);
process.stdout.write(JSON.stringify(vm.runInContext(process.argv[1], context)));
`

func TestReportHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for report helper unit tests")
	}
	for _, tc := range []struct {
		name       string
		expression string
		want       string
	}{
		{name: "explicit metric and namespace subsets", expression: `values(metrics('• app.ci: 8 Pending (team: 2; another-team: 3), 27 nodes.'))`, want: `[[["app.ci",8,"observed"]],false]`},
		{name: "dash markdown rows", expression: "values(metrics('**Pending snapshot**\\n- **build02** — **14 Pending**: one sampled pod.'))", want: `[[["build02",14,"observed"]],false]`},
		{name: "numeric headings inheritance", expression: `values(metrics('Pending-workload scan — synthetic run\nScheduling / startup blockers\n• build02 — 14 Pending: sampled pod.\n• build03 — 24: includes 7 recent pods.\nPersistent dependency failures\n• app.ci — 8: details.'))`, want: `[[["app.ci",8,"observed"],["build02",14,"observed"],["build03",24,"observed"]],false]`},
		{name: "legacy priority sections", expression: `values(metrics('Priority checks\n• build11 — 18. Six sampled pods.\nAdditional blockers\n• hosted-mgmt — 88 across hosted namespaces.'))`, want: `[[["build11",18,"observed"],["hosted-mgmt",88,"observed"]],false]`},
		{name: "compact row shared pending unit", expression: `values(metrics('build04:21; build10:18; build12:8 Pending. None older than 10m.'))`, want: `[[["build04",21,"observed"],["build10",18,"observed"],["build12",8,"observed"]],false]`},
		{name: "inline snapshot totals", expression: `values(metrics('Other snapshot totals: build01 14 (11 overprovisioning-* plus 3 recent); build05 18, build06 24, build11 21 and build12 16, mostly recent startup pods.'))`, want: `[[["build01",14,"observed"],["build05",18,"observed"],["build06",24,"observed"],["build11",21,"observed"],["build12",16,"observed"]],false]`},
		{name: "zero remains observed", expression: `values(metrics('build01: 0 Pending\nbuild02 — no Pending pods observed.'))`, want: `[[["build01",0,"observed"],["build02",0,"observed"]],false]`},
		{name: "zero incompatible metric units rejected", expression: `['jobs','nodes','namespaces','CSRs','Machines','replicas','builds','containers'].map(unit => values(metrics('build01: no Pending ' + unit + ' observed.')))`, want: `[[[],true],[[],true],[[],true],[[],true],[[],true],[[],true],[[],true],[[],true]]`},
		{name: "positive incompatible metric units rejected", expression: `['jobs','nodes','namespaces','CSRs','Machines','replicas','builds','containers'].map(unit => values(metrics('Pending snapshot\nbuild01: 3 Pending ' + unit + ' observed.')))`, want: `[[[],true],[[],true],[[],true],[[],true],[[],true],[[],true],[[],true],[[],true]]`},
		{name: "compact incompatible unit does not establish scope", expression: `values(metrics('build01: 3; build02: 4 Pending CSRs'))`, want: `[[],true]`},
		{name: "snapshot qualifier accepted", expression: `values(metrics('build01: 3 Pending at the snapshot.'))`, want: `[[["build01",3,"observed"]],false]`},
		{name: "scan snapshot zero and first snapshot accepted", expression: `values(metrics('Other snapshot totals\n• build08: 0 Pending at the scan snapshot.\n• build07: 4 Pending at the first snapshot; the sampled pod started.'))`, want: `[[["build07",4,"observed"],["build08",0,"observed"]],false]`},
		{name: "same line access contradiction conflicts", expression: `values(metrics('build01: 3 Pending; build01 authentication failed, workload state is unknown.'))`, want: `[[["build01",null,"conflict"]],true]`},
		{name: "access in another clause keeps own context", expression: `values(metrics('build01: 3 Pending; build02 authentication failed, workload state is unknown.'))`, want: `[[["build01",3,"observed"],["build02",null,"unavailable"]],true]`},
		{name: "multipart fences retain scope", expression: "let a = fixture('a', '```text\\nbuild01: 3 Pending', 'run'), b = fixture('b', 'build02: 7 Pending\\n```', 'run'); values(OpsReports.pending(OpsReports.runs([a,b])[0]))", want: `[[],true]`},
		{name: "multipart closed fence resumes metrics", expression: "let a = fixture('a', '```text\\nbuild01: 3 Pending', 'run'), b = fixture('b', 'build02: 7 Pending\\n```\\nbuild03: 4 Pending', 'run'); values(OpsReports.pending(OpsReports.runs([a,b])[0]))", want: `[[["build03",4,"observed"]],false]`},
		{name: "unavailable distinct from zero", expression: `values(metrics('Coverage gap: build13 could not be inspected (authentication denied).'))`, want: `[[["build13",null,"unavailable"]],true]`},
		{name: "rejected authentication attempts preserve unavailable context", expression: `values(metrics('Coverage gap: build13 rejected both authentication attempts; its workload state is unknown.'))`, want: `[[["build13",null,"unavailable"]],true]`},
		{name: "inline coverage statement", expression: `values(metrics('Inspected several contexts; build13 authentication failed twice, so its workload state is unknown.'))`, want: `[[["build13",null,"unavailable"]],true]`},
		{name: "coverage association scoped to clause", expression: `values(metrics('build01: 3 Pending; build13 rejected authentication.'))`, want: `[[["build01",3,"observed"],["build13",null,"unavailable"]],true]`},
		{name: "contradictory counts never choose latest", expression: `values(metrics('build01: 3 Pending\nbuild01: 4 Pending'))`, want: `[[["build01",null,"conflict"]],true]`},
		{name: "contradictory access and snapshot", expression: `values(metrics('build01: 3 Pending\nbuild01: unavailable, authentication failed.'))`, want: `[[["build01",null,"conflict"]],true]`},
		{name: "duplicate identical counts", expression: `values(metrics('build01: 3 Pending\nbuild01: 3 Pending'))`, want: `[[["build01",3,"observed"]],false]`},
		{name: "qualifiers and section retained", expression: `let result = metrics('Other scan observations\n• build05: 174 Pending, system pods; sampled pod started, not a stuck queue.'); [result.observations[0].evidence, result.observations[0].notes]`, want: `[["• build05: 174 Pending, system pods; sampled pod started, not a stuck queue."],["Other scan observations"]]`},
		{name: "unsupported context stays partial", expression: `values(metrics('build01: 3 Pending\nnew-ci: 17 Pending'))`, want: `[[["build01",3,"observed"]],true]`},
		{name: "unknown format preserves fallback", expression: `values(metrics('There are several affected clusters.'))`, want: `[[],true]`},
		{name: "unlisted contexts not invented", expression: `values(metrics('build01: 3 Pending'))`, want: `[[["build01",3,"observed"]],false]`},
		{name: "unscoped number not a pending metric", expression: `values(metrics('build01: 27 nodes, 3 namespaces and a 19h old sampled pod.'))`, want: `[[],true]`},
		{name: "unknown heading ends inheritance", expression: `values(metrics('Pending snapshot\nbuild01: 3\n## Node counts\nbuild02: 27'))`, want: `[[["build01",3,"observed"]],true]`},
		{name: "plain unknown heading ends inheritance", expression: `values(metrics('Pending snapshot\nbuild01: 3\nNode counts\nbuild02: 27'))`, want: `[[["build01",3,"observed"]],true]`},
		{name: "wrong task cannot supply pending", expression: `let o = fixture('root', 'build01: 3 Pending'); o.message.scheduled_task = 'capacity_scan'; values(OpsReports.pending(o))`, want: `[[],false]`},
		{name: "legacy visible task header", expression: `let o = fixture('root', '[Scheduled task: pending_workloads_scan | Instructions: synthetic/path]\nbuild01: 3 Pending'); delete o.message.scheduled_task; values(OpsReports.pending(o))`, want: `[[["build01",3,"observed"]],false]`},
		{name: "explicit metadata overrides visible task", expression: `let o = fixture('root', '[Scheduled task: pending_workloads_scan]\nbuild01: 3 Pending'); o.message.scheduled_task = 'other'; values(OpsReports.pending(o))`, want: `[[],false]`},
		{name: "code fences and quoted examples excluded", expression: "values(metrics('```text\\nbuild01: 13 Pending\\n```\\n> build02: 17 Pending\\n&gt; build03: 15 Pending\\nExample: \\\"build04: 19 Pending\\\"\\nbuild05: 2 Pending'))", want: `[[["build05",2,"observed"]],false]`},
		{name: "html cannot supply numeric facts", expression: `values(metrics('<script>build01: 3 Pending</script>\n<img src=x onerror=evil()>\nbuild02: 4 Pending'))`, want: `[[["build02",4,"observed"]],true]`},
		{name: "unsafe numbers rejected", expression: `values(metrics('build01: 9007199254740992 Pending\nbuild02: -1 Pending\nbuild03: 2.5 Pending\nbuild04: 1,234 Pending'))`, want: `[[],true]`},
		{name: "inline snapshot incompatible units", expression: `values(metrics('Other snapshot totals: build01 27 nodes; build02 3 Pending'))`, want: `[[["build02",3,"observed"]],true]`},
		{name: "compact desired readiness not pending", expression: `values(metrics('build01: 2 desired, 1 ready; build02: 3 Pending'))`, want: `[[["build02",3,"observed"]],true]`},
		{name: "namespace subsets not total", expression: `values(metrics('Pending snapshot\nbuild01: 2 in team namespace\nbuild02: 3 overprovisioning pods\nbuild03: 4 Pending jobs'))`, want: `[[],true]`},
		{name: "inline code metric example excluded", expression: "values(metrics('`build01: 7 Pending`\\nbuild02: 4 Pending'))", want: `[[["build02",4,"observed"]],false]`},
		{name: "fully linked metric examples excluded", expression: `values(metrics('[build01: 8 Pending](https://example.org/old-example)\n<https://example.org/old-example|build02: 9 Pending>\nbuild03: 4 Pending'))`, want: `[[["build03",4,"observed"]],false]`},
		{name: "formatted linked context remains supported", expression: `values(metrics('[build01](https://example.org/context): 4 Pending'))`, want: `[[["build01",4,"observed"]],false]`},
		{name: "aggregate multipart parse budget", expression: `let a = fixture('a', 'x'.repeat(249999) + '\n', 'run'), b = fixture('b', 'build02: 8 Pending', 'run'); let report = OpsReports.runs([a,b])[0]; [report.message.text.length <= 250000, report.parts.length, values(OpsReports.pending(report))]`, want: `[true,2,[[],true]]`},
		{name: "truncated metric never becomes fact", expression: `values(metrics('x'.repeat(249980) + '\nbuild01: 3 Pending jobs and more'))`, want: `[[],true]`},
		{name: "global count qualifiers retained", expression: `metrics('Counts include recent and overprovisioning pods.\nbuild01: 7 Pending').observations[0].notes`, want: `["Counts include recent and overprovisioning pods"]`},
		{name: "age and node suffixes rejected in section", expression: `values(metrics('Pending snapshot\nbuild01: 12h old\nbuild02: 27 nodes\nbuild03: 6 namespaces\nbuild04: 5 jobs'))`, want: `[[],true]`},
		{name: "context names exact", expression: `values(metrics('build00: 2 Pending\nbuild100: 3 Pending\nbuild01-extra: 8 Pending\nbuild99: 4 Pending'))`, want: `[[["build99",4,"observed"]],true]`},
		{name: "thread replies excluded", expression: `let o = fixture('root', 'build01: 3 Pending'); o.thread = [{is_chai: true, text: 'build01: 9 Pending'}]; values(OpsReports.pending(o))`, want: `[[["build01",3,"observed"]],false]`},
		{name: "multipart chronological root and original parts", expression: `let a = fixture('first', 'build01: 3 Pending', 'run', '2026-10-29T09:00:00Z'), b = fixture('second', 'build02: 4 Pending', 'run'); let report = OpsReports.runs([b,a])[0]; [report.identity.id,report.message.reported_at,report.message.text,report.parts.map(p=>p.message.slack_url), values(OpsReports.pending(report))]`, want: `["first","2026-10-29T09:00:00Z","build01: 3 Pending\n\nbuild02: 4 Pending",["https://example.org/first","https://example.org/second"],[[["build01",3,"observed"],["build02",4,"observed"]],false]]`},
		{name: "distinct and unkeyed reports stay separate", expression: `OpsReports.runs([fixture('a','one'),fixture('b','two'),fixture('c','three','one'),fixture('d','four','two')]).length`, want: `4`},
		{name: "same run different identity stays separate", expression: `let a = fixture('a','one','run'), b = fixture('b','two','run'), c = fixture('c','three','run'), d = fixture('d','four','run'); b.message.scheduled_task = 'other'; c.identity.channel = 'C_OTHER'; d.message.source = 'other'; OpsReports.runs([a,b,c,d]).length`, want: `4`},
		{name: "missing grouping identity stays separate", expression: `let a = fixture('a','one','run'), b = fixture('b','two','run'); delete a.identity.channel; delete b.identity.channel; OpsReports.runs([a,b]).length`, want: `2`},
		{name: "malformed run metadata stays separate", expression: `[' ',{},['run'],42,'bad\nkey','x'.repeat(513)].map(run => OpsReports.runs([fixture('a','one',run),fixture('b','two',run)]).length)`, want: `[2,2,2,2,2,2]`},
		{name: "duplicate IDs retained once", expression: `let a = fixture('a','one','run'); OpsReports.runs([a,a]).map(r=>r.parts.length)`, want: `[1]`},
		{name: "duplicate ID different channel retained", expression: `let a = fixture('a','one','run'), b = fixture('a','two','run'); b.identity.channel = 'C_OTHER'; OpsReports.runs([a,b]).length`, want: `2`},
		{name: "logical run sorting uses first part", expression: `OpsReports.runs([fixture('a','one','run','2026-10-29T08:00:00Z'),fixture('b','two','run','2026-10-29T12:00:00Z'),fixture('c','three','other','2026-10-29T10:00:00Z')]).map(r=>r.identity.id)`, want: `["c","a"]`},
		{name: "deterministic equal timestamps", expression: `let a = fixture('a','one','run'), b = fixture('b','two','run'); JSON.stringify(OpsReports.runs([a,b])) === JSON.stringify(OpsReports.runs([b,a]))`, want: `true`},
		{name: "untrusted header adjacency never groups", expression: `let a = fixture('a','[Scheduled task: pending_workloads_scan]\nbuild01: 2 Pending'), b = fixture('b','build02: 4 Pending'); delete b.message.scheduled_task; OpsReports.runs([a,b]).length`, want: `2`},
		{name: "large input marked partial", expression: `values(metrics('build01: 3 Pending\n' + 'x'.repeat(250001)))`, want: `[[["build01",3,"observed"]],true]`},
		{name: "malformed collections safe", expression: `[OpsReports.runs(null),OpsReports.runs([null,{}, {message: null}]), OpsReports.pending({})]`, want: `[[],[],{"observations":[],"partial":false}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(node, "-e", reportsUnitHarness, tc.expression)
			got, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("evaluating report helper: %v\n%s", err, got)
			}
			if diff := cmp.Diff(tc.want, strings.TrimSpace(string(got))); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}
