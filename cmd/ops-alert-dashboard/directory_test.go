package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// directoryClock supplies deterministic expiry and Retry-After boundaries.
type directoryClock struct{ now time.Time }

// Now implements the optional metadata clock.
func (c *directoryClock) Now() time.Time { return c.now }

// directoryFixture is one detached browser page for deterministic metadata tests.
func directoryFixture() opsalerts.AlertThread {
	return opsalerts.AlertThread{Root: opsalerts.ThreadMessage{Text: "Hello <@U_ONE> :custom:", Author: opsalerts.ThreadPerson{ID: "U_ONE", Name: "U_ONE"}}, People: map[string]opsalerts.ThreadPerson{"U_ONE": {ID: "U_ONE", Name: "U_ONE"}}, Emojis: map[string]string{}, Warnings: []string{}}
}

// TestSlackDirectoryCache checks singular profile APIs, bounded refresh and negative caching.
func TestSlackDirectoryCache(t *testing.T) {
	cases := []struct {
		name     string
		results  []slackHTTPResult
		wantName string
		warning  bool
		expire   bool
	}{
		{name: "success", results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Ada","image_72":"https://avatars.slack-edge.com/ada.png"}}}`}, {body: `{"ok":true,"emoji":{"custom":"alias:target","target":"https://emoji.slack-edge.com/ok.png","unsafe":"https://evil.example/a.png"}}`}}, wantName: "Ada"},
		{name: "missing scopes cached", results: []slackHTTPResult{{body: `{"ok":false,"error":"missing_scope"}`}, {body: `{"ok":false,"error":"missing_scope"}`}}, wantName: "U_ONE", warning: true},
		{name: "wrong identity stays unresolved", results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_OTHER","profile":{"display_name":"Wrong identity"}}}`}, {body: `{"ok":true,"emoji":{}}`}}, wantName: "U_ONE", warning: true},
		{name: "expiry retries once", results: []slackHTTPResult{{body: `{"ok":false,"error":"missing_scope"}`}, {body: `{"ok":true,"emoji":{}}`}, {body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Ada"}}}`}, {body: `{"ok":true,"emoji":{}}`}}, wantName: "Ada", expire: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: tc.results}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryFixture()
			directory.EnrichThread(context.Background(), &page)
			if tc.expire {
				clock.now = clock.now.Add(time.Hour)
			}
			page = directoryFixture()
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(tc.wantName, page.Root.Author.Name); diff != "" {
				t.Errorf("unexpected name (-want +got):\n%s", diff)
			}
			if (len(page.Warnings) > 0) != tc.warning {
				t.Fatalf("warnings %v", page.Warnings)
			}
			if diff := cmp.Diff(len(tc.results), len(fake.forms)); diff != "" {
				t.Errorf("unexpected call count (-want +got):\n%s", diff)
			}
			if fake.paths[0] != "/api/users.info" || fake.forms[0].Get("user") != "U_ONE" || fake.forms[0].Get("users") != "" {
				t.Fatalf("undocumented profile request %v %v", fake.paths, fake.forms)
			}
			if tc.name == "success" {
				if diff := cmp.Diff(map[string]string{"custom": "alias:target", "target": "https://emoji.slack-edge.com/ok.png"}, page.Emojis); diff != "" {
					t.Errorf("unexpected emoji (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestSlackDirectoryBounds exercises deterministic eviction and workspace emoji limits.
func TestSlackDirectoryBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emoji bool
	}{{name: "identity limit"}, {name: "emoji dictionary limit", emoji: true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{}
			directory := &slackDirectory{client: fakeSlackClient(fake), cache: directoryCache{people: map[string]directoryPerson{}}, clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			if tc.emoji {
				values := map[string]string{}
				for i := range 5000 {
					values[fmt.Sprintf("custom_%04d", i)] = "https://emoji.slack-edge.com/custom.png"
				}
				data, err := json.Marshal(map[string]any{"ok": true, "emoji": values})
				if err != nil {
					t.Fatal(err)
				}
				fake.results = []slackHTTPResult{{body: string(data)}}
				directory.lookupEmojis(context.Background())
				if diff := cmp.Diff(4096, len(directory.cache.emojis)); diff != "" {
					t.Errorf("unexpected emoji bound (-want +got):\n%s", diff)
				}
				return
			}
			for i := range 512 {
				directory.cache.people[fmt.Sprintf("U_%03d", i)] = directoryPerson{}
			}
			fake.results = []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_NEW","name":"New participant"}}`}}
			page := directoryFixture()
			page.Root.Text = ""
			page.Root.Author = opsalerts.ThreadPerson{ID: "U_NEW", Name: "U_NEW"}
			page.People = map[string]opsalerts.ThreadPerson{}
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(512, len(directory.cache.people)); diff != "" {
				t.Errorf("unexpected identity bound (-want +got):\n%s", diff)
			}
			if _, exists := directory.cache.people["U_000"]; exists {
				t.Fatal("eviction tie-break was not deterministic")
			}
		})
	}
}

// directoryConcurrentRead shares only the production directory, never mutable result maps.
func directoryConcurrentRead(directory *slackDirectory, start <-chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	<-start
	page := directoryFixture()
	directory.EnrichThread(context.Background(), &page)
}

// TestSlackDirectoryCoalescing proves concurrent browser misses share one API result.
func TestSlackDirectoryCoalescing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readers int
	}{{name: "many readers", readers: 12}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}, {body: `{"ok":true,"emoji":{}}`}}}
			transport := &directoryHTTPFake{fake: fake}
			directory := &slackDirectory{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			start := make(chan struct{})
			var done sync.WaitGroup
			for range tc.readers {
				done.Add(1)
				go directoryConcurrentRead(directory, start, &done)
			}
			close(start)
			done.Wait()
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected metadata requests (-want +got):\n%s", diff)
			}
		})
	}
}

// directoryPersonPage isolates profile metadata requests from mentions and emoji requests.
func directoryPersonPage(id string) opsalerts.AlertThread {
	return opsalerts.AlertThread{Root: opsalerts.ThreadMessage{Author: opsalerts.ThreadPerson{ID: id, Name: id}}, People: map[string]opsalerts.ThreadPerson{id: {ID: id, Name: id}}, Emojis: map[string]string{}, Warnings: []string{}}
}

// TestSlackDirectoryMethodBackoff shares rate and permission cooldowns across different identities.
func TestSlackDirectoryMethodBackoff(t *testing.T) {
	cases := []struct {
		name, prefix string
		rate         bool
	}{{name: "users rate limit", prefix: "U", rate: true}, {name: "users missing scope", prefix: "U"}, {name: "bots rate limit", prefix: "B", rate: true}, {name: "bots missing scope", prefix: "B"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := slackHTTPResult{body: `{"ok":false,"error":"missing_scope"}`}
			wait := 5 * time.Minute
			if tc.rate {
				first = slackHTTPResult{status: 429}
				wait = time.Hour
			}
			response := fmt.Sprintf(`{"ok":true,"user":{"id":"%s_THREE","name":"Recovered"}}`, tc.prefix)
			if tc.prefix == "B" {
				response = fmt.Sprintf(`{"ok":true,"bot":{"id":"%s_THREE","name":"Recovered"}}`, tc.prefix)
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{first, {body: response}}}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryPersonPage(tc.prefix + "_ONE")
			directory.EnrichThread(context.Background(), &page)
			clock.now = clock.now.Add(wait - time.Nanosecond)
			page = directoryPersonPage(tc.prefix + "_TWO")
			directory.EnrichThread(context.Background(), &page)
			if len(fake.forms) != 1 {
				t.Fatal("different identity bypassed shared cooldown")
			}
			if len(page.Warnings) == 0 {
				t.Fatal("cooldown was not surfaced safely")
			}
			if _, exists := directory.cache.people[tc.prefix+"_TWO"]; exists {
				t.Fatal("unattempted identity was negatively cached")
			}
			clock.now = clock.now.Add(time.Nanosecond)
			page = directoryPersonPage(tc.prefix + "_THREE")
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected boundary request count (-want +got):\n%s", diff)
			}
			if page.Root.Author.Name != "Recovered" {
				t.Fatalf("metadata did not recover: %#v", page)
			}
		})
	}
}

// TestSlackDirectoryTTL checks positive, negative and emoji expiry strictly before and at the boundary.
func TestSlackDirectoryTTL(t *testing.T) {
	cases := []struct {
		name                  string
		negative, emoji, rate bool
		ttl                   time.Duration
	}{
		{name: "profile positive", ttl: time.Hour},
		{name: "profile negative", negative: true, ttl: 5 * time.Minute},
		{name: "emoji positive", emoji: true, ttl: time.Hour},
		{name: "emoji long Retry-After", emoji: true, rate: true, ttl: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := slackHTTPResult{body: `{"ok":true,"user":{"id":"U_ONE","name":"First"}}`}
			last := slackHTTPResult{body: `{"ok":true,"user":{"id":"U_ONE","name":"Second"}}`}
			if tc.negative {
				first.body = `{"ok":false,"error":"user_not_found"}`
			}
			if tc.emoji {
				first.body = `{"ok":true,"emoji":{"custom":"https://emoji.slack-edge.com/first.png"}}`
				last.body = `{"ok":true,"emoji":{"custom":"https://emoji.slack-edge.com/second.png"}}`
			}
			if tc.rate {
				first = slackHTTPResult{status: 429}
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{first, last}}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryPersonPage("U_ONE")
			if tc.emoji {
				page = directoryPersonPage("")
				page.Root.Text = ":custom:"
			}
			directory.EnrichThread(context.Background(), &page)
			clock.now = clock.now.Add(tc.ttl - time.Nanosecond)
			directory.EnrichThread(context.Background(), &page)
			if len(fake.forms) != 1 {
				t.Fatal("metadata refreshed before its TTL or Retry-After")
			}
			clock.now = clock.now.Add(time.Nanosecond)
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected TTL boundary (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSlackDirectoryMessageIdentity preserves distinct per-message bot identities for the same sender ID.
func TestSlackDirectoryMessageIdentity(t *testing.T) {
	cases := []struct {
		name   string
		custom bool
	}{{name: "custom bot names and icons", custom: true}, {name: "unresolved human uses directory"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Directory profile","image_72":"https://avatars.slack-edge.com/profile.png"}}}`}}}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			page := directoryPersonPage("U_ONE")
			page.Messages = []opsalerts.ThreadMessage{{Author: page.Root.Author}}
			if tc.custom {
				page.Root.Author.Name = "Root app"
				page.Root.Author.AvatarURL = "https://avatars.slack-edge.com/root.png"
				page.Root.Author.IsBot = true
				page.Messages[0].Author.Name = "Reply app"
				page.Messages[0].Author.AvatarURL = "https://avatars.slack-edge.com/reply.png"
				page.Messages[0].Author.IsBot = true
			}
			wantRoot, wantReply := page.Root.Author, page.Messages[0].Author
			if !tc.custom {
				wantRoot.Name = "Directory profile"
				wantRoot.AvatarURL = "https://avatars.slack-edge.com/profile.png"
				wantReply = wantRoot
			}
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(wantRoot, page.Root.Author); diff != "" {
				t.Errorf("unexpected root identity (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantReply, page.Messages[0].Author); diff != "" {
				t.Errorf("unexpected reply identity (-want +got):\n%s", diff)
			}
		})
	}
}

// directoryHTTPFake synchronizes the existing SDK fake and optionally holds one in-flight response.
type directoryHTTPFake struct {
	mu      sync.Mutex
	fake    *slackHTTPFake
	entered chan struct{}
	release <-chan struct{}
	blocked bool
}

// Do holds no test mutex while waiting for cancellation or an explicitly released response.
func (f *directoryHTTPFake) Do(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	response, err := f.fake.Do(request)
	block := f.entered != nil && !f.blocked
	f.blocked = true
	f.mu.Unlock()
	if block {
		close(f.entered)
		select {
		case <-f.release:
		case <-request.Context().Done():
			if response != nil {
				err = errors.Join(err, response.Body.Close())
			}
			return nil, errors.Join(err, request.Context().Err())
		}
	}
	return response, err
}

// count reads the fake's request log without racing a metadata holder.
func (f *directoryHTTPFake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.fake.forms) }

// directoryEnrichResult transports a detached result from a named test goroutine.
type directoryEnrichResult struct{ page opsalerts.AlertThread }

// directoryWaitContext signals that acquisition has reached its cancellable wait.
type directoryWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

// observe records the first Done lookup without relying on scheduler timing.
func (c *directoryWaitContext) observe() { close(c.observed) }

// Done lets tests cancel an actual waiter while the network holder remains blocked.
func (c *directoryWaitContext) Done() <-chan struct{} { c.once.Do(c.observe); return c.Context.Done() }

// enrichDirectoryAsync exercises a real in-flight SDK request without closures or sleeps.
func enrichDirectoryAsync(ctx context.Context, directory *slackDirectory, page opsalerts.AlertThread, done chan<- directoryEnrichResult) {
	directory.EnrichThread(ctx, &page)
	done <- directoryEnrichResult{page: page}
}

// TestSlackDirectoryCancellation checks both cancelled gate waiters and cancelled first loads without cache poisoning.
func TestSlackDirectoryCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelHolder bool
	}{{name: "gate waiter"}, {name: "first load", cancelHolder: true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}, {body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}}}
			release := make(chan struct{})
			entered := make(chan struct{})
			transport := &directoryHTTPFake{fake: fake, entered: entered, release: release}
			directory := &slackDirectory{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			page := directoryPersonPage("U_ONE")
			if tc.cancelHolder {
				page.Root.Text = "Also <@U_TWO>"
			}
			done := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(ctx, directory, page, done)
			<-entered
			if tc.cancelHolder {
				cancel()
				<-done
				if len(directory.cache.people) != 0 {
					t.Fatal("cancelled or unattempted profile was cached")
				}
				page = directoryPersonPage("U_ONE")
				directory.EnrichThread(context.Background(), &page)
				if page.Root.Author.Name != "Ada" || transport.count() != 2 {
					t.Fatal("cancelled first load poisoned a subsequent request")
				}
				return
			}
			waiterCtx, cancelWaiter := context.WithCancel(context.Background())
			defer cancelWaiter()
			observed := make(chan struct{})
			waiting := &directoryWaitContext{Context: waiterCtx, observed: observed}
			waiter := directoryPersonPage("U_TWO")
			waiterDone := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(waiting, directory, waiter, waiterDone)
			<-observed
			cancelWaiter()
			result := <-waiterDone
			if transport.count() != 1 || len(result.page.Warnings) == 0 {
				t.Fatal("cancelled gate waiter performed I/O or lost fallback warning")
			}
			close(release)
			<-done
			if _, exists := directory.cache.people["U_TWO"]; exists {
				t.Fatal("cancelled waiter poisoned another identity")
			}
			page = directoryPersonPage("U_ONE")
			directory.EnrichThread(context.Background(), &page)
			if page.Root.Author.Name != "Ada" || transport.count() != 1 {
				t.Fatal("gate waiter interfered with the holder's successful cache")
			}
		})
	}
}
