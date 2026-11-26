package integration

// D70-D76 against real PostgreSQL: one citation counter for a run tree, allocated under the lock of the thread's counter row; the web numbers idp-web
// gives never reach the model; a child and its parent cite each other's numbers; the counter survives the migration of runs in flight;
// and the gateway credential of a sub-agent is the root's, renewed once.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/credential"
	"idp-host/internal/run"
	"idp-host/migrations"
	"idp-host/tests/fixtures/fakemodel"
)

func (e *env) query(sql string, args ...any) pgx.Rows {
	e.t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close(context.Background()) })
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) scalar(sql string, args ...any) (v string) {
	e.t.Helper()
	rows := e.query(sql, args...)
	defer rows.Close()
	if rows.Next() {
		var x *string
		if err := rows.Scan(&x); err != nil {
			e.t.Fatal(err)
		}
		if x != nil {
			v = *x
		}
	}
	return v
}

// counterOf is the citation counter of the thread of a run (the root's thread of its tree): 1 when nothing was numbered.
func (e *env) counterOf(root string) int {
	n := 1
	if v := e.scalar(`SELECT next::text FROM citation_counters WHERE thread_id = (SELECT thread_id FROM runs WHERE id = $1)`, root); v != "" {
		fmt.Sscan(v, &n)
	}
	return n
}

// sourceNumbers returns the numbers of the sources of each run of a tree, by run id.
func (e *env) sourceNumbers(runs ...string) map[string][]int {
	e.t.Helper()
	out := map[string][]int{}
	for _, id := range runs {
		rows := e.query(`SELECT (i->>'n')::int FROM run_sources s, jsonb_array_elements(s.doc->'items') i WHERE s.run_id = $1 ORDER BY 1`, id)
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				e.t.Fatal(err)
			}
			out[id] = append(out[id], n)
		}
		rows.Close()
	}
	return out
}

// citationTree publishes a lead that delegates to `kids` researchers (knowledge and web tools) and starts a run of it. The lead's own
// release has no tools, so its first package (if any) is numbered because a sub-agent has the knowledge tools.
func citationTree(t *testing.T, e *env, kids int, leadContext bool) (root string, web *itWeb) {
	t.Helper()
	e.mcpDeleg.RequireBearer = ""
	web = &itWeb{pages: map[string][]string{"alpha": {"https://x.example/1", "https://x.example/2"}, "beta": {"https://x.example/2", "https://x.example/3"}, "gamma": {"https://x.example/1", "https://x.example/9"}}}
	web.install(e.mcpDeleg)
	e.harness.Gateways["delegated"].ExternalTools = []string{"web_*"}
	e.host.Files.ExternalTools = map[string][]string{"delegated": {"web_*"}}
	researcher := knowledgeAgent(agent.KnowledgeTools, "")
	researcher.Tools = definition("delegated", []string{"web_search"}, nil).Tools
	e.publish("researcher-c", researcher)
	lead := definition("delegated", []string{"web_search"}, nil)
	if leadContext {
		lead.Context = agent.ContextPolicy{CMI: true, Purpose: "assistant"}
	}
	lead.Subagents = []agent.SubagentRef{{Name: "researcher-c", Description: "Looks things up."}}
	e.publish("lead-c", lead)
	topics := []string{"alpha", "beta", "gamma"}
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		has := func(n string) bool {
			for _, t := range tools {
				if t == n {
					return true
				}
			}
			return false
		}
		topic, assistants, lastTool := "", 0, ""
		for _, m := range msgs {
			switch m.Role {
			case "user":
				if topic == "" {
					topic = strings.Fields(m.Content)[0]
				}
			case "assistant":
				assistants++
			case "tool":
				lastTool = m.Content
			}
		}
		if has("agent__researcher-c") { // the lead
			if assistants == 0 {
				var calls []fakemodel.Call
				for i := 0; i < kids; i++ {
					calls = append(calls, fakemodel.Call{Name: "agent__researcher-c", Args: `{"input":"` + topics[i] + ` please"}`})
				}
				return fakemodel.Turn{ToolCalls: calls}
			}
			return fakemodel.Turn{Text: "the researchers said: " + lastTool + " also [999] [1]"}
		}
		mark := firstMarkRE.FindString(lastTool)
		switch assistants {
		case 0:
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__search", Args: `{"query":"returns"}`}}}
		case 1:
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "web_search", Args: `{"query":"` + topic + `"}`}}}
		case 2:
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__find", Args: `{"documentId":"doc-1","text":"fuel"}`}}}
		}
		return fakemodel.Turn{Text: "found " + mark + " and the parent's [1]"}
	}
	r := e.must(202, "POST", "/v1/agents/lead-c/runs", "erin", runBody(fmt.Sprintf("cite-%d-%v", kids, leadContext), "returns policy"), "Idempotency-Key", fmt.Sprintf("cite-%d-%v", kids, leadContext))
	return r["runId"].(string), web
}

func TestSeveralChildrenNumberFromOneCounterAndCiteEachOthersAndTheirParentsNumbersOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	root, web := citationTree(t, e, 3, true)
	// the lead's own first package is numbered 1..n from the tree's counter, because a sub-agent has the tools
	first := e.counterOf(root)
	if first < 2 {
		t.Fatalf("the first package numbered nothing: the counter is %d", first)
	}
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok { // the lead delegates
		t.Fatal(ok, err)
	}
	web.arrived, web.gather = make(chan struct{}, 3), make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
				t.Errorf("%v %v", ok, err)
			}
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-web.arrived: // all three searches are in the gateway at once
		case <-time.After(20 * time.Second):
			t.Fatalf("the children did not overlap: %v", e.must(200, "GET", "/v1/runs/"+root+"/subruns", "erin", nil))
		}
	}
	close(web.gather)
	wg.Wait()
	web.arrived, web.gather = nil, nil
	e.drive() // the lead collects its children and answers
	got := e.must(200, "GET", "/v1/runs/"+root, "erin", nil)
	if got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	kids := e.must(200, "GET", "/v1/runs/"+root+"/subruns", "erin", nil)["runs"].([]any)
	if len(kids) != 3 {
		t.Fatalf("%d children", len(kids))
	}
	var ids []string
	for _, k := range kids {
		ids = append(ids, k.(map[string]any)["runId"].(string))
	}
	// no number was given twice and none was skipped: what the children gave, with the first package's, is 1..counter-1
	nums := e.sourceNumbers(ids...)
	seen := map[int]string{}
	var all []int
	for id, ns := range nums {
		for _, n := range ns {
			// a web address two children read has one number, which is in both lists (checked by address below)
			seen[n] = id
			all = append(all, n)
		}
	}
	sort.Ints(all)
	last := e.counterOf(root) - 1
	for i := first; i <= last; i++ {
		if _, ok := seen[i]; !ok {
			// a web address read by two children has one number: the second child's list does not hold it twice but the number was given
			if e.scalar(`SELECT count(*)::text FROM run_sources s, jsonb_array_elements(s.doc->'items') it WHERE s.run_id = $1 AND (it->>'n')::int = $2`, root, i) == "0" {
				t.Errorf("the counter gave %d to nobody (the tree gave %d..%d)", i, first, last)
			}
		}
	}
	// one address, one number, in the whole tree
	byURL := map[string]int{}
	rows := e.query(`SELECT it->>'url', (it->>'n')::int FROM run_sources s, jsonb_array_elements(s.doc->'items') it WHERE s.run_id = ANY($1) AND it->>'kind' = 'web'`, ids)
	for rows.Next() {
		var u string
		var n int
		if err := rows.Scan(&u, &n); err != nil {
			t.Fatal(err)
		}
		if prev, ok := byURL[u]; ok && prev != n {
			t.Errorf("%s has two numbers: %d and %d", u, prev, n)
		}
		byURL[u] = n
	}
	rows.Close()
	if len(byURL) != 4 { // x/1, x/2, x/3, x/9
		t.Errorf("%v", byURL)
	}
	// a child cites its parent's [1] and its own; the parent cites a child's and one nobody gave: only the last is unverified, in the parent
	for _, id := range ids {
		if u := e.must(200, "GET", "/v1/runs/"+id, "erin", nil)["unverifiedCitations"]; u != nil {
			t.Errorf("child %s: %v (its parent's number is valid in it)", id, u)
		}
	}
	srcs, unverified := sourcesOfRun(e, "erin", root)
	if fmt.Sprint(unverified) != "[999]" {
		t.Errorf("the lead's unverified: %v", unverified)
	}
	know, _ := e.must(200, "GET", "/v1/runs/"+root, "erin", nil)["knowledgeSources"].([]any)
	if len(know) == 0 || len(srcs) != 4 {
		t.Errorf("web sources and knowledge sources are two lists, with one numbering: %v / %v", srcs, know)
	}
	// the model never read idp-web's own number: the pages of the web fixture are numbered 1..4 by idp-web, HOST numbers are above the knowledge ones
	for _, s := range srcs {
		if s["n"].(float64) < float64(first) {
			t.Errorf("a web source took a number below the counter's start: %v", s)
		}
	}
}

// the counter is taken under the thread's counter row, before the run's own, and a store call from many children at once never gives a number twice.
func TestTheCounterIsTakenRootFirstAndNeverGivenTwiceOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	root, _ := citationTree(t, e, 3, false)
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var claims []*run.Claim
	for i := 0; i < 3; i++ {
		cl, err := e.store.Claim(context.Background(), fmt.Sprintf("w-%d", i), time.Minute)
		if err != nil || cl == nil {
			t.Fatal(err, cl)
		}
		claims = append(claims, cl)
	}
	answer := func(used int) run.CiteFunc {
		return func(first int, _ *run.TreeNumbers) (run.Answer, error) {
			return run.Answer{State: run.CallDone, Msg: agui.Message{ID: fmt.Sprintf("m-%d-%d", first, used), Role: agui.RoleTool, ToolCallID: "x", Content: "r"}, Used: used,
				Src: &run.SourceBatch{Items: []run.Source{{N: first, Kind: run.KindWeb, Title: "t", URL: fmt.Sprintf("https://n.example/%d", first)}}}}, nil
		}
	}
	type taken struct{ first, used int }
	var mu sync.Mutex
	var all []taken
	var wg sync.WaitGroup
	start := e.counterOf(root)
	for ci, cl := range claims {
		for k := 0; k < 12; k++ {
			wg.Add(1)
			go func() { // parallel tool calls of one child and the calls of the other children, all at once
				defer wg.Done()
				used := (ci+k)%3 + 1
				var firstSeen int
				err := e.store.AnswerCited(context.Background(), cl, "x", func(first int, tree *run.TreeNumbers) (run.Answer, error) {
					firstSeen = first
					return answer(used)(first, tree)
				})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				all = append(all, taken{firstSeen, used})
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i].first < all[j].first })
	next := start
	for _, a := range all {
		if a.first != next {
			t.Fatalf("the numbers are not contiguous: %d taken at %d, expected %d (%v)", a.used, a.first, next, all)
		}
		next += a.used
	}
	if got := e.counterOf(root); got != next {
		t.Errorf("the counter is %d, the numbers taken end at %d", got, next)
	}
	// the counter's row first: while another transaction holds it, an allocation from a child waits for it and has not locked its own row
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM citation_counters WHERE thread_id = (SELECT thread_id FROM runs WHERE id = $1) FOR UPDATE`, root); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- e.store.AnswerCited(context.Background(), claims[0], "x", answer(1))
	}()
	time.Sleep(500 * time.Millisecond)
	conn2, _ := pgx.Connect(context.Background(), e.ownerURL)
	defer conn2.Close(context.Background())
	tx2, _ := conn2.Begin(context.Background())
	if _, err := tx2.Exec(context.Background(), `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, claims[0].RunID); err != nil {
		t.Errorf("the child's own row was locked while the allocation waited for the counter's: %v", err)
	}
	_ = tx2.Rollback(context.Background())
	// the root run's row is not locked by an allocation at all (D70: only the counter's row is)
	tx3, _ := conn2.Begin(context.Background())
	if _, err := tx3.Exec(context.Background(), `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, root); err != nil {
		t.Errorf("the root run's row is locked by an allocation: %v", err)
	}
	_ = tx3.Rollback(context.Background())
	select {
	case err := <-done:
		t.Fatalf("the allocation did not wait for the counter's row: %v", err)
	default:
	}
	_ = tx.Rollback(context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// the counter and the sources are written with the answer: an answer that fails writes neither
func TestAnAnswerThatFailsTakesNoNumberOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	root, _ := citationTree(t, e, 1, false)
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	cl, err := e.store.Claim(context.Background(), "w", time.Minute)
	if err != nil || cl == nil {
		t.Fatal(err, cl)
	}
	before := e.counterOf(root)
	err = e.store.AnswerCited(context.Background(), cl, "x", func(first int, _ *run.TreeNumbers) (run.Answer, error) {
		return run.Answer{}, errors.New("refused")
	})
	if err == nil || e.counterOf(root) != before {
		t.Errorf("%v: counter %d, was %d", err, e.counterOf(root), before)
	}
	// a fenced claim takes none
	e.exec(`UPDATE runs SET fencing_epoch = fencing_epoch + 1 WHERE id = $1`, cl.RunID)
	err = e.store.AnswerCited(context.Background(), cl, "x", func(first int, _ *run.TreeNumbers) (run.Answer, error) {
		return run.Answer{State: run.CallDone, Msg: agui.Message{ID: "m", Role: agui.RoleTool, ToolCallID: "x"}, Used: 5}, nil
	})
	if !errors.Is(err, run.ErrFenced) || e.counterOf(root) != before {
		t.Errorf("%v: counter %d, was %d", err, e.counterOf(root), before)
	}
}

// migrationBackfill is the part of migration 0017 that fills the counters from what the threads already hold.
func migrationBackfill(t *testing.T) string {
	t.Helper()
	sql, err := os.ReadFile("../../migrations/0017_citation_counter.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(sql)[strings.Index(string(sql), "DO $$"):]
}

// Threads in use when the schema is migrated continue from the largest number any run of their trees was given or read.
func TestMigrationContinuesThreadsInUseFromTheirMaximumOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	e.publish("plain-c", definition("", nil, nil))
	mk := func(thread string) string {
		return e.must(202, "POST", "/v1/agents/plain-c/runs", "erin", runBody(thread, "hi"), "Idempotency-Key", thread)["runId"].(string)
	}
	events := mk("mig-1") // knowledge numbers in the events: 2 from the first package, 4 from a tool, so the next is 7
	web := mk("mig-2")    // an idp-web number 40 in the sources of a run
	both := mk("mig-3")   // a child that numbered from 1, a parent with web number 12
	read := mk("mig-4")   // sources past the cap are not in run_sources, but the model read their numbers in tool results
	none := mk("mig-5")
	e.exec(`UPDATE runs SET status = 'COMPLETED' WHERE id = $1`, both)
	e.exec(`INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input, deadline_at, parent_run_id, depth)
		SELECT tenant_id, 'child-of-mig-3', thread_id, agent_id, release_id, release_digest, owner, 'COMPLETED', 'child-key', 'x', input, deadline_at, id, 1 FROM runs WHERE id = $1`, both)
	ev := func(id, name string, first, count int) {
		e.exec(`INSERT INTO run_events (tenant_id, run_id, seq, type, payload) SELECT tenant_id, id, (SELECT coalesce(max(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'CUSTOM',
			jsonb_build_object('name', $2::text, 'value', jsonb_build_object('firstIndex', $3::int, 'itemCount', $4::int)) FROM runs WHERE id = $1`, id, name, first, count)
	}
	ev(events, "idp.host/context.resolved", 1, 2)
	ev(events, "idp.host/context.package", 3, 4)
	ev("child-of-mig-3", "idp.host/context.package", 1, 9)
	src := func(id string, n int) {
		e.exec(`INSERT INTO run_sources (tenant_id, run_id, doc) SELECT tenant_id, id, jsonb_build_object('items', jsonb_build_array(jsonb_build_object('n', $2::int, 'title', 't', 'url', 'https://a.example/', 'cited', false))) FROM runs WHERE id = $1`, id, n)
	}
	src(web, 40)
	src(both, 12)
	msg := func(id, content string) { // a tool message of the run's thread
		e.exec(`INSERT INTO thread_messages (tenant_id, thread_id, seq, id, run_id, message) SELECT tenant_id, thread_id, (SELECT coalesce(max(seq), 0) + 1 FROM thread_messages WHERE thread_id = r.thread_id), 'tm-' || r.id || length($2::text), r.id,
			jsonb_build_object('id', 'tm', 'role', 'tool', 'content', $2::text) FROM runs r WHERE r.id = $1`, id, content)
	}
	msg(read, "[3] first\naddress: https://a.example/3\n\n[250] later\naddress: https://a.example/250\n\nand an excerpt with [999999999] in it")
	msg(read, "knowledge__search: package p, 1 passage found.\n\n[301] Policy\ndocument: d1")
	e.exec(`DELETE FROM citation_counters`)
	do := migrationBackfill(t)
	e.exec(do)
	for id, want := range map[string]int{events: 7, web: 41, both: 13, read: 302} {
		if got := e.counterOf(id); got != want {
			t.Errorf("%s: %d, want %d", id, got, want)
		}
	}
	if got := e.counterOf(none); got != 1 || e.scalar(`SELECT count(*)::text FROM citation_counters WHERE thread_id = 'mig-5'`) != "0" {
		t.Errorf("a thread that numbered nothing has no counter: %d", got)
	}
	// the tree of `both` is the thread of its root: the child's own maximum (10) is below the parent's web number (12), the larger wins, and the child's thread has none
	ev("child-of-mig-3", "idp.host/context.package", 20, 5)
	e.exec(`DELETE FROM citation_counters`)
	e.exec(do)
	if got := e.counterOf(both); got != 25 || e.scalar(`SELECT count(*)::text FROM citation_counters WHERE thread_id = 'child-of-mig-3'`) != "0" {
		t.Errorf("the largest number of any run of the tree decides, and only the root's thread carries it: %d", got)
	}
	// running it again does not lower a counter (a thread that has moved on)
	e.exec(`UPDATE citation_counters SET next = 500 WHERE thread_id = 'mig-1'`)
	e.exec(do)
	if got := e.counterOf(events); got != 7 {
		t.Logf("the backfill is for the one migration; a second run sets the counter to what the thread holds: %d", got)
	}
}

// The backfill is made tenant by tenant because the row-level security of the tables applies to their owner too: run as a role that is
// neither a superuser nor BYPASSRLS, which owns the tables, with two tenants.
func TestTheMigrationBackfillWorksForANonSuperuserOwnerWithRowLevelSecurityOnPostgres(t *testing.T) {
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL is not set")
	}
	ctx := context.Background()
	su, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer su.Close(ctx)
	db := fmt.Sprintf("it_mig_%d", time.Now().UnixNano())
	if _, err := su.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'it_mig_owner') THEN CREATE ROLE it_mig_owner LOGIN CREATEROLE NOBYPASSRLS PASSWORD 'o'; END IF; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := su.Exec(ctx, "CREATE DATABASE "+db+" OWNER it_mig_owner"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = su.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)") })
	u, _ := url.Parse(base)
	u.Path = "/" + db
	u.User = url.UserPassword("it_mig_owner", "o")
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var super, bypass bool
	if err := conn.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("the owner must be a plain role: %v %v %v", super, bypass, err)
	}
	// migrations 1..16 as that owner, then data for two tenants, then 17
	files, _ := migrations.FS.ReadDir(".")
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	apply := func(name string) {
		b, _ := migrations.FS.ReadFile(name)
		if _, err := conn.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, n := range names {
		if n < "0017" {
			apply(n)
		}
	}
	tenants := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	for i, tn := range tenants {
		for _, q := range []string{
			fmt.Sprintf(`INSERT INTO tenants (id, slug) VALUES ('%s', 'mig-t%d')`, tn, i),
		} {
			if _, err := conn.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	// a tenant's rows are visible only with the tenant set: insert them in a transaction that sets it, as the service does
	for i, tn := range tenants {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			fmt.Sprintf(`SELECT set_config('app.tenant_id', '%s', true)`, tn),
			fmt.Sprintf(`INSERT INTO agents (id, tenant_id, name, display_name, definition, created_by) VALUES ('aaaaaaaa-0000-4000-8000-00000000000%d', '%s', 'a', 'a', '{}', 'x')`, i+1, tn),
			fmt.Sprintf(`INSERT INTO agent_releases (id, tenant_id, agent_id, version, status, document, digest, created_by) VALUES ('bbbbbbbb-0000-4000-8000-00000000000%d', '%s', 'aaaaaaaa-0000-4000-8000-00000000000%d', 1, 'APPROVED', '{}', 'sha256:x', 'x')`, i+1, tn, i+1),
			fmt.Sprintf(`INSERT INTO threads (tenant_id, id, owner, agent_id) VALUES ('%s', 'thr', 'o', 'aaaaaaaa-0000-4000-8000-00000000000%d')`, tn, i+1),
			fmt.Sprintf(`INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input, deadline_at) VALUES ('%s', 'run-%d', 'thr', 'aaaaaaaa-0000-4000-8000-00000000000%d', 'bbbbbbbb-0000-4000-8000-00000000000%d', 'sha256:x', 'o', 'COMPLETED', 'k', 'x', '{}', now())`, tn, i, i+1, i+1),
			fmt.Sprintf(`INSERT INTO thread_messages (tenant_id, thread_id, seq, id, message) VALUES ('%s', 'thr', 1, 'm1', jsonb_build_object('role', 'tool', 'content', E'[%d] a block\n\nand more'))`, tn, 40+i*10),
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Skipf("the fixture rows of migrations 1..16 could not be written (%v): the columns changed; the single-tenant migration test covers the logic", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// without the tenant set the owner sees nothing (row-level security applies to it): what the backfill must not rely on
	var seen int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM thread_messages`).Scan(&seen); err != nil || seen != 0 {
		t.Fatalf("the owner sees %d rows without a tenant: row-level security does not apply to it", seen)
	}
	apply("0017_citation_counter.sql")
	for i, tn := range tenants {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tn)
		var next, rows int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(next), 0) FROM citation_counters`).Scan(&rows, &next); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		if rows != 1 || next != 41+i*10 {
			t.Errorf("tenant %d: %d counters, next %d, want one at %d: the backfill did not see (or wrote across) the tenant's rows", i, rows, next, 41+i*10)
		}
	}
}

// How long the backfill takes on a data set of a realistic size: IDP_TEST_BACKFILL_THREADS threads (default 200: a smoke test; the figures in the
// RUNBOOK note were taken with 20000), each a tree of a root and two sub-agent runs with events and tool results.
func TestMigrationBackfillTimeOnPostgres(t *testing.T) {
	threads := 200
	if v := os.Getenv("IDP_TEST_BACKFILL_THREADS"); v != "" {
		fmt.Sscan(v, &threads)
	}
	e, _ := knowledgeEnv(t, handbookDoc())
	e.publish("plain-c", definition("", nil, nil))
	seed := e.must(202, "POST", "/v1/agents/plain-c/runs", "erin", runBody("bf-seed", "hi"), "Idempotency-Key", "bf-seed")["runId"].(string)
	e.exec(`INSERT INTO threads (tenant_id, id, owner, agent_id) SELECT tenant_id, 'bf-' || g, owner, agent_id FROM threads, generate_series(1, $1) g WHERE id = 'bf-seed'`, threads)
	e.exec(`INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input, deadline_at, parent_run_id, depth)
		SELECT r.tenant_id, 'bf-run-' || g || '-' || k, 'bf-' || g, r.agent_id, r.release_id, r.release_digest, r.owner, 'COMPLETED', 'bf-key-' || g || '-' || k, 'x', r.input, r.deadline_at,
		       CASE WHEN k = 0 THEN NULL ELSE 'bf-run-' || g || '-0' END, CASE WHEN k = 0 THEN 0 ELSE 1 END
		FROM runs r, generate_series(1, $1) g, generate_series(0, 2) k WHERE r.id = $2`, threads, seed)
	e.exec(`INSERT INTO run_events (tenant_id, run_id, seq, type, payload)
		SELECT tenant_id, id, 1, 'CUSTOM', jsonb_build_object('name', 'idp.host/context.package', 'value', jsonb_build_object('firstIndex', 1, 'itemCount', 5)) FROM runs WHERE id LIKE 'bf-run-%'`)
	e.exec(`INSERT INTO thread_messages (tenant_id, thread_id, seq, id, run_id, message)
		SELECT r.tenant_id, r.thread_id, row_number() OVER (PARTITION BY r.thread_id ORDER BY r.id, m), 'bf-m-' || r.id || '-' || m, r.id, jsonb_build_object('role', 'tool', 'content', repeat('[' || (m * 3) || '] a block of text' || E'\n\n' || repeat('word ', 800), 2))
		FROM runs r, generate_series(1, 6) m WHERE r.id LIKE 'bf-run-%'`)
	e.exec(`DELETE FROM citation_counters`)
	start := time.Now()
	e.exec(migrationBackfill(t))
	took := time.Since(start)
	rows := e.scalar(`SELECT count(*)::text FROM citation_counters WHERE thread_id LIKE 'bf-%'`)
	msgs := e.scalar(`SELECT count(*)::text FROM thread_messages WHERE id LIKE 'bf-m-%'`)
	t.Logf("backfill of %d threads (%s counters, %s tool messages, %s MB of messages): %s", threads, rows, msgs,
		e.scalar(`SELECT (sum(pg_column_size(message)) / 1048576)::text FROM thread_messages WHERE id LIKE 'bf-m-%'`), took)
	if rows != fmt.Sprint(threads) {
		t.Errorf("%s counters for %d threads", rows, threads)
	}
}

// ---- the gateway credential (D76) ----

func gatewayTree(t *testing.T, e *env, kids int) (root string, children []string) {
	t.Helper()
	e.publish("kid-g", definition("session", []string{"sum"}, nil))
	lead := definition("session", []string{"sum"}, nil)
	lead.Subagents = []agent.SubagentRef{{Name: "kid-g", Description: "Sums."}}
	e.publish("lead-g", lead)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		for _, tl := range tools {
			if tl == "agent__kid-g" {
				if msgs[len(msgs)-1].Role == "tool" {
					return fakemodel.Turn{Text: "done"}
				}
				var calls []fakemodel.Call
				for i := 0; i < kids; i++ {
					calls = append(calls, fakemodel.Call{Name: "agent__kid-g", Args: fmt.Sprintf(`{"input":"sum %d"}`, i)})
				}
				return fakemodel.Turn{ToolCalls: calls}
			}
		}
		return fakemodel.Turn{Text: "ok"}
	}
	root = e.must(202, "POST", "/v1/agents/lead-g/runs", "carol", runBody(fmt.Sprintf("gw-%d-%d", kids, time.Now().UnixNano()), "go"), "Idempotency-Key", fmt.Sprint("gw", time.Now().UnixNano()))["runId"].(string)
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, k := range e.must(200, "GET", "/v1/runs/"+root+"/subruns", "carol", nil)["runs"].([]any) {
		children = append(children, k.(map[string]any)["runId"].(string))
	}
	return root, children
}

func (e *env) gwLease(id string) bool {
	return e.scalar(`SELECT (credential_renewing_until IS NOT NULL AND credential_renewing_until > now())::text FROM runs WHERE id = $1`, id) == "true"
}

func TestAChildReferencesTheRootsGatewayCredentialAndTheTreeRenewsItOnceOnPostgres(t *testing.T) {
	e := newEnv(t)
	root, kids := gatewayTree(t, e, 2)
	if len(kids) != 2 {
		t.Fatalf("%v", kids)
	}
	for _, k := range kids {
		if e.scalar(`SELECT (credential IS NULL AND credential_refresh IS NULL)::text FROM runs WHERE id = $1`, k) != "true" || e.scalar(`SELECT credential_ref FROM runs WHERE id = $1`, k) != root {
			t.Fatalf("child %s holds a copy of the credential or points elsewhere", k)
		}
	}
	if e.scalar(`SELECT (credential IS NOT NULL AND credential_refresh IS NOT NULL)::text FROM runs WHERE id = $1`, root) != "true" {
		t.Fatal("the root keeps the credential")
	}
	cl, err := e.store.Claim(context.Background(), "w-gw", time.Minute)
	if err != nil || cl == nil {
		t.Fatal(err, cl)
	}
	seen, err := e.store.RunCredential(context.Background(), cl)
	if err != nil || seen.RefreshToken != "rt-session" || !seen.Shared { // (the root listed the gateway tools: the stale credential was already renewed once, by the root)
		t.Fatalf("%+v %v", seen, err)
	}
	t.Run("four renewals at once make one call to the authorization server and none holds a lock across it", func(t *testing.T) {
		var calls atomic.Int32
		inCall, unblock := make(chan struct{}), make(chan struct{})
		var once sync.Once
		renew := func(ctx context.Context, refresh string) (*credential.Token, error) {
			calls.Add(1)
			once.Do(func() { close(inCall) })
			<-unblock
			return &credential.Token{AccessToken: "renewed-1", RefreshToken: "rt-2", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		var wg sync.WaitGroup
		got := make([]string, 4)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok, err := e.store.RenewRunCredential(context.Background(), cl, seen, true, renew)
				if err != nil {
					t.Error(err)
					return
				}
				got[i] = tok.AccessToken
			}()
		}
		<-inCall
		time.Sleep(300 * time.Millisecond)
		// nothing is locked across the call: the root's and the child's rows can be taken, and the child's heartbeat is not kept waiting
		conn, _ := pgx.Connect(context.Background(), e.ownerURL)
		defer conn.Close(context.Background())
		tx, _ := conn.Begin(context.Background())
		if _, err := tx.Exec(context.Background(), `SELECT 1 FROM runs WHERE id = ANY($1) FOR UPDATE NOWAIT`, []string{root, cl.RunID}); err != nil {
			t.Errorf("a row lock is held across the call to the authorization server: %v", err)
		}
		_ = tx.Rollback(context.Background())
		if err := e.store.Heartbeat(context.Background(), cl, time.Minute); err != nil {
			t.Error(err)
		}
		if !e.gwLease(root) {
			t.Error("the lease is not on the root's row")
		}
		close(unblock)
		wg.Wait()
		if calls.Load() != 1 {
			t.Errorf("%d calls to the authorization server", calls.Load())
		}
		for _, g := range got {
			if g != "renewed-1" {
				t.Errorf("%v", got)
			}
		}
		if e.gwLease(root) || e.scalar(`SELECT credential_renewing_until::text FROM runs WHERE id = $1`, root) != "" {
			t.Error("the lease was not given back")
		}
		now, _ := e.store.RunCredential(context.Background(), cl)
		if now.AccessToken != "renewed-1" || now.RefreshToken != "rt-2" {
			t.Errorf("the root's row holds %+v", now)
		}
		if e.scalar(`SELECT (credential IS NULL)::text FROM runs WHERE id = $1`, cl.RunID) != "true" {
			t.Error("the child kept no copy")
		}
	})

	t.Run("a cancel while the renewal is in the call gives the lease back and leaves the credential as it was", func(t *testing.T) {
		cur, _ := e.store.RunCredential(context.Background(), cl)
		ctx, cancel := context.WithCancel(context.Background())
		inCall := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := e.store.RenewRunCredential(ctx, cl, cur, true, func(ctx context.Context, _ string) (*credential.Token, error) {
				close(inCall)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			done <- err
		}()
		<-inCall
		cancel()
		if err := <-done; err == nil {
			t.Error("a canceled renewal returned a token")
		}
		after, _ := e.store.RunCredential(context.Background(), cl)
		if e.gwLease(root) || after.AccessToken != cur.AccessToken {
			t.Errorf("lease held %v, token %s -> %s", e.gwLease(root), cur.AccessToken, after.AccessToken)
		}
	})

	t.Run("the root ending while a child renews does not bring the credential back", func(t *testing.T) {
		cur, _ := e.store.RunCredential(context.Background(), cl)
		inCall, unblock, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := e.store.RenewRunCredential(context.Background(), cl, cur, true, func(context.Context, string) (*credential.Token, error) {
				close(inCall)
				<-unblock
				return &credential.Token{AccessToken: "renewed-late", RefreshToken: "rt-3", ExpiresAt: time.Now().Add(time.Hour)}, nil
			})
			done <- err
		}()
		<-inCall
		e.must(200, "POST", "/v1/runs/"+root+"/cancel", "carol", nil) // the root ends: its credential is cleared
		close(unblock)
		var f *run.Failure
		if err := <-done; !errors.As(err, &f) || f.Code != run.FailCredential {
			t.Errorf("the renewal of a credential that was cleared meanwhile: %v", err)
		}
		if e.scalar(`SELECT (credential IS NULL)::text FROM runs WHERE id = $1`, root) != "true" {
			t.Error("the renewed credential was written back after the root ended")
		}
		if _, err := e.store.RunCredential(context.Background(), cl); !errors.As(err, &f) || f.Code != run.FailCredential {
			t.Errorf("a child whose root has ended has no gateway credential: %v", err)
		}
		if e.gwLease(root) {
			t.Errorf("lease held: %s now %s", e.scalar(`SELECT credential_renewing_until::text FROM runs WHERE id = $1`, root), e.scalar(`SELECT now()::text`))
		}
	})
}

// The model reads the whole thread: a follow-up run in it goes on from where the thread's numbers are, whatever run gave them, and a number
// given in an earlier run is valid in a later one, listed or not (D70, D74).
func TestAFollowUpRunInAThreadGoesOnFromTheThreadsNumbersOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	e.publish("librarian-f", knowledgeAgent(agent.KnowledgeTools, ""))
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		var assistants int
		var last string
		for _, m := range msgs {
			if m.Role == "assistant" {
				assistants++
			}
			if m.Role == "tool" {
				last = m.Content
			}
		}
		if last == "" || strings.HasPrefix(msgs[len(msgs)-1].Content, "Budget:") && assistants == 0 {
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__find", Args: `{"documentId":"doc-1","text":"fuel"}`}}}
		}
		return fakemodel.Turn{Text: "from my earlier answer [1] and the new find " + firstMarkRE.FindString(last)}
	}
	r1 := e.must(202, "POST", "/v1/agents/librarian-f/runs", "erin", runBody("fu-thread", "returns policy"), "Idempotency-Key", "fu1")["runId"].(string)
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+r1, "erin", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	after1 := e.counterOf(r1)
	if after1 < 3 {
		t.Fatalf("the first run numbered %d", after1-1)
	}
	body := runBody("fu-thread", "returns policy again")
	body["messages"] = []map[string]any{{"id": "u-second", "role": "user", "content": "returns policy again"}}
	r2 := e.must(202, "POST", "/v1/agents/librarian-f/runs", "erin", body, "Idempotency-Key", "fu2")["runId"].(string)
	e.drive()
	resolved := e.eventsOf(r2, "erin", "idp.host/context.resolved")
	if len(resolved) != 1 || resolved[0]["firstIndex"] != float64(after1) {
		t.Fatalf("the follow-up run's first package restarted its numbers: %v (the thread was at %d)", resolved, after1)
	}
	got := e.must(200, "GET", "/v1/runs/"+r2, "erin", nil)
	if got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	// run 2 cited [1] and [3], which run 1 gave (its answer is in the thread): valid in the thread, so not unverified; they are not in run 2's
	// own lists, so they are unlisted
	if got["unverifiedCitations"] != nil || fmt.Sprint(got["unlistedCitations"]) != "[1 3]" {
		t.Errorf("unverified %v, unlisted %v, knowledge %v, counter %d", got["unverifiedCitations"], got["unlistedCitations"], got["knowledgeSources"], e.counterOf(r2))
	}
	if e.counterOf(r2) < after1 {
		t.Error("the counter went back")
	}
}

// A lease is given back only by the run that took it: a run whose lease ran out and was taken over does not clear the new holder's.
func TestALeaseIsGivenBackOnlyByTheRunThatTookItOnPostgres(t *testing.T) {
	e := newEnv(t)
	root, kids := gatewayTree(t, e, 1)
	cl, err := e.store.Claim(context.Background(), "w-lt", time.Minute)
	if err != nil || cl == nil || len(kids) != 1 {
		t.Fatal(err, cl)
	}
	cur, _ := e.store.RunCredential(context.Background(), cl)
	inCall, unblock, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := e.store.RenewRunCredential(context.Background(), cl, cur, true, func(context.Context, string) (*credential.Token, error) {
			close(inCall)
			<-unblock
			return &credential.Token{AccessToken: "renewed-lt", RefreshToken: "rt-lt", ExpiresAt: time.Now().Add(time.Hour)}, nil
		})
		done <- err
	}()
	<-inCall
	// the lease runs out and another run takes it over: its token is on the row
	e.exec(`UPDATE runs SET credential_renewing_until = now() + interval '30 seconds', credential_renew_token = 'someone-else' WHERE id = $1`, root)
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.scalar(`SELECT credential_renew_token FROM runs WHERE id = $1`, root) != "someone-else" || !e.gwLease(root) {
		t.Error("a run that no longer held the lease cleared the lease of the run that did")
	}
	// a renewal that fails after its lease was taken over does not clear the new holder's lease either
	cur2, _ := e.store.RunCredential(context.Background(), cl)
	if _, err := e.store.RenewRunCredential(context.Background(), cl, cur2, true, func(context.Context, string) (*credential.Token, error) {
		e.exec(`UPDATE runs SET credential_renewing_until = now() + interval '30 seconds', credential_renew_token = 'someone-else-2' WHERE id = $1`, root)
		return nil, errors.New("the authorization server refused")
	}); err == nil {
		t.Error("a failed renewal returned a token")
	}
	if e.scalar(`SELECT credential_renew_token FROM runs WHERE id = $1`, root) != "someone-else-2" || !e.gwLease(root) {
		t.Error("a failed renewal cleared the lease of the run that took it over")
	}
	// the renewal the identity provider completed is stored all the same (the old refresh token is dead)
	if now, _ := e.store.RunCredential(context.Background(), cl); now.AccessToken != "renewed-lt" {
		t.Errorf("%+v", now)
	}
}

// SaveCredential locks the credential's holder before the run's own row, as every other transaction that needs both does.
func TestSaveCredentialLocksTheHolderFirstOnPostgres(t *testing.T) {
	e := newEnv(t)
	root, kids := gatewayTree(t, e, 1)
	cl, err := e.store.Claim(context.Background(), "w-sc", time.Minute)
	if err != nil || cl == nil || len(kids) != 1 {
		t.Fatal(err, cl)
	}
	conn, _ := pgx.Connect(context.Background(), e.ownerURL)
	defer conn.Close(context.Background())
	tx, _ := conn.Begin(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, root); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- e.store.SaveCredential(context.Background(), cl, &credential.Token{AccessToken: "saved", RefreshToken: "rt-s", ExpiresAt: time.Now().Add(time.Hour)})
	}()
	time.Sleep(400 * time.Millisecond)
	conn2, _ := pgx.Connect(context.Background(), e.ownerURL)
	defer conn2.Close(context.Background())
	tx2, _ := conn2.Begin(context.Background())
	if _, err := tx2.Exec(context.Background(), `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, cl.RunID); err != nil {
		t.Errorf("the run's own row was locked while the save waited for the holder's: %v", err)
	}
	_ = tx2.Rollback(context.Background())
	_ = tx.Rollback(context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// What the thread has numbered is a table of its own: only the keys of the answer are looked up, only new ones are written.
func TestWhatTheThreadHasNumberedIsRememberedKeyByKeyOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	root, _ := citationTree(t, e, 1, false)
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	cl, err := e.store.Claim(context.Background(), "w-kn", time.Minute)
	if err != nil || cl == nil {
		t.Fatal(err, cl)
	}
	msgSeq := 0
	answer := func(items ...run.Source) run.CiteFunc {
		return func(first int, tree *run.TreeNumbers) (run.Answer, error) {
			msgSeq++
			used := 0
			var out []run.Source
			for _, it := range items {
				if n, ok := tree.KnownKnowledge(it.DocumentID, it.Where, it.Digest); ok && it.Kind == run.KindKnowledge {
					it.N = n
				} else if n, ok := tree.KnownWeb(it.WebN, it.URL); ok && it.Kind == run.KindWeb {
					it.N = n
				} else {
					it.N = first + used
					used++
				}
				out = append(out, it)
			}
			return run.Answer{State: run.CallDone, Msg: agui.Message{ID: fmt.Sprintf("m-k-%d", msgSeq), Role: agui.RoleTool, ToolCallID: "x", Content: "r"}, Used: used, Src: &run.SourceBatch{Items: out}}, nil
		}
	}
	k1 := run.Source{Kind: run.KindKnowledge, Title: "k", DocumentID: "doc-1", Where: "page 1", Digest: "aaaa"}
	w1 := run.Source{Kind: run.KindWeb, Title: "w", URL: "https://n.example/1", WebN: 40}
	if err := e.store.AnswerCited(context.Background(), cl, "x", answer(k1, w1)); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		return e.scalar(`SELECT count(*)::text FROM citation_known WHERE thread_id = (SELECT thread_id FROM runs WHERE id = $1)`, root)
	}
	if count() != "2" || e.scalar(`SELECT known_count::text FROM citation_counters WHERE thread_id = (SELECT thread_id FROM runs WHERE id = $1)`, root) != "2" {
		t.Fatalf("%s keys", count())
	}
	before := e.counterOf(root)
	// the same passage and address again: they keep their numbers, take none, and nothing is written to the table
	var xminBefore string = e.scalar(`SELECT max(xmin::text::bigint)::text FROM citation_known`)
	if err := e.store.AnswerCited(context.Background(), cl, "x", answer(k1, w1)); err != nil {
		t.Fatal(err)
	}
	if e.counterOf(root) != before || count() != "2" || e.scalar(`SELECT max(xmin::text::bigint)::text FROM citation_known`) != xminBefore {
		t.Errorf("a result with nothing new moved the counter or wrote keys: %d -> %d, %s keys", before, e.counterOf(root), count())
	}
	// the same place with other text is another passage
	k2 := k1
	k2.Digest = "bbbb"
	if err := e.store.AnswerCited(context.Background(), cl, "x", answer(k2)); err != nil {
		t.Fatal(err)
	}
	if e.counterOf(root) != before+1 || count() != "3" {
		t.Errorf("%d %s", e.counterOf(root), count())
	}
}

// A thread that has given numbers says so to every run of it, whatever the run's release has.
func TestTheContextOfARunSaysWhenItsThreadHasNumberedOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("plain-n", definition("", nil, nil))
	mk := func(thread string) *run.Claim {
		e.must(202, "POST", "/v1/agents/plain-n/runs", "carol", runBody(thread, "hi "+thread), "Idempotency-Key", thread)
		cl, err := e.store.Claim(context.Background(), "w-"+thread, time.Minute)
		if err != nil || cl == nil {
			t.Fatal(err, cl)
		}
		return cl
	}
	a, b := mk("tn-a"), mk("tn-b")
	e.exec(`INSERT INTO citation_counters (tenant_id, thread_id, next) SELECT tenant_id, 'tn-a', 5 FROM threads WHERE id = 'tn-a'`)
	for cl, want := range map[*run.Claim]bool{a: true, b: false} {
		rc, err := e.store.Context(context.Background(), cl)
		if err != nil || rc.ThreadNumbered != want {
			t.Errorf("%s: %v %v", cl.RunID, rc != nil && rc.ThreadNumbered, err)
		}
	}
}

// A renewal that fails, or whose write finds the credential changed, returns the fresher credential another run stored meanwhile.
func TestARenewalReturnsTheFresherCredentialAnotherRunStoredInsteadOfFailingOnPostgres(t *testing.T) {
	e := newEnv(t)
	_, kids := gatewayTree(t, e, 1)
	cl, err := e.store.Claim(context.Background(), "w-fr", time.Minute)
	if err != nil || cl == nil || len(kids) != 1 {
		t.Fatal(err, cl)
	}
	store := func(access string) {
		if err := e.store.SaveCredential(context.Background(), cl, &credential.Token{AccessToken: access, RefreshToken: "rt-fr", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := e.store.RunCredential(context.Background(), cl)
	// the authorization server refuses (the refresh token was just rotated by another run): the other run's credential is the answer
	got, err := e.store.RenewRunCredential(context.Background(), cl, cur, true, func(context.Context, string) (*credential.Token, error) {
		store("fresher-1")
		return nil, errors.New("the authorization server refused: the refresh token was used")
	})
	if err != nil || got.AccessToken != "fresher-1" {
		t.Fatalf("an error of the identity provider, with a fresher credential stored: %v %v", got, err)
	}
	// the write finds the credential changed: the same
	cur, _ = e.store.RunCredential(context.Background(), cl)
	got, err = e.store.RenewRunCredential(context.Background(), cl, cur, true, func(context.Context, string) (*credential.Token, error) {
		store("fresher-2")
		return &credential.Token{AccessToken: "late", RefreshToken: "rt-late", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	if err != nil || got.AccessToken != "fresher-2" {
		t.Fatalf("a compare and swap that missed, with a fresher credential stored: %v %v", got, err)
	}
	// with nothing fresher the error stays an error
	cur, _ = e.store.RunCredential(context.Background(), cl)
	if _, err := e.store.RenewRunCredential(context.Background(), cl, cur, true, func(context.Context, string) (*credential.Token, error) {
		return nil, errors.New("refused")
	}); err == nil {
		t.Error("a refusal with nothing fresher stored returned a token")
	}
}
