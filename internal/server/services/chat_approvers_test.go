package services

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

// ---- allowlist parsing ----

func TestParseApproversAndAllows(t *testing.T) {
	l := ParseApprovers(`{"bot_token":"x","approver_user_ids":"11, 22;33","approver_role_ids":"900 901"}`)
	if got := strings.Join(l.Users, ","); got != "11,22,33" {
		t.Fatalf("users=%q", got)
	}
	cases := []struct {
		a    ChatActor
		want bool
	}{
		{ChatActor{Kind: "discord", UserID: "22"}, true},
		{ChatActor{Kind: "discord", UserID: "5", RoleIDs: []string{"1", "901"}}, true},
		{ChatActor{Kind: "discord", UserID: "5", RoleIDs: []string{"1"}}, false},
		{ChatActor{Kind: "telegram", UserID: "5", RoleIDs: []string{"901"}}, false}, // roles are Discord only
		{ChatActor{Kind: "telegram", UserID: "33"}, true},
		{ChatActor{Kind: "discord"}, false},
	}
	for i, c := range cases {
		if got := l.Allows(c.a); got != c.want {
			t.Errorf("case %d: Allows(%+v)=%v want %v", i, c.a, got, c.want)
		}
	}
	for _, cfg := range []string{`{}`, `not json`, `{"approver_user_ids":""}`, `{"approver_user_ids":" , "}`} {
		e := ParseApprovers(cfg)
		if !e.Empty() || e.Allows(ChatActor{Kind: "discord", UserID: "1"}) {
			t.Errorf("%q must deny everyone", cfg)
		}
	}
}

func guardFixture(t *testing.T, cfg string, active bool) (*queries.NotificationQueries, func(*int) DecisionFunc) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := queries.NewNotificationQueries(db)
	if err := q.Create(&queries.NotificationChannel{ID: "ch1", ChannelType: "discord_bot", Name: "n", Config: cfg, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if !active {
		_ = q.Update("ch1", false)
	}
	return q, func(calls *int) DecisionFunc {
		return GuardChannelDecision(q, "ch1", cfg, func(string, bool, int, ChatActor) error { *calls++; return nil })
	}
}

func TestGuardChannelDecisionEnforcesAllowlist(t *testing.T) {
	cfg := `{"bot_token":"t","channel_id":"1","approver_user_ids":"11","approver_role_ids":"900"}`
	q, mk := guardFixture(t, cfg, true)
	var calls int
	g := mk(&calls)
	if err := g("a1", true, 0, ChatActor{Kind: "discord", UserID: "11"}); err != nil || calls != 1 {
		t.Fatalf("listed user: %v calls=%d", err, calls)
	}
	if err := g("a1", false, 0, ChatActor{Kind: "discord", UserID: "7", RoleIDs: []string{"900"}}); err != nil || calls != 2 {
		t.Fatalf("listed role: %v calls=%d", err, calls)
	}
	if err := g("a1", true, 0, ChatActor{Kind: "discord", UserID: "7"}); !errors.Is(err, ErrNotApprover) || calls != 2 {
		t.Fatalf("stranger: %v calls=%d", err, calls)
	}
	// Editing the approver list takes effect at once, without invalidating the listener.
	if err := q.UpdateFull("ch1", "n", `{"bot_token":"t","channel_id":"1","approver_user_ids":"7"}`); err != nil {
		t.Fatal(err)
	}
	if err := g("a1", true, 0, ChatActor{Kind: "discord", UserID: "7"}); err != nil || calls != 3 {
		t.Fatalf("newly listed: %v calls=%d", err, calls)
	}
	if err := g("a1", true, 0, ChatActor{Kind: "discord", UserID: "11"}); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("removed user must lose access immediately: %v", err)
	}
	// Any other config edit still disables the running listener.
	if err := q.UpdateFull("ch1", "n", `{"bot_token":"other","channel_id":"1","approver_user_ids":"7"}`); err != nil {
		t.Fatal(err)
	}
	if err := g("a1", true, 0, ChatActor{Kind: "discord", UserID: "7"}); !errors.Is(err, ErrChannelNotAuthoritative) {
		t.Fatalf("edited channel: %v", err)
	}
}

func TestGuardChannelDecisionEmptyListDeniesEveryone(t *testing.T) {
	_, mk := guardFixture(t, `{"bot_token":"t","channel_id":"1"}`, true)
	var calls int
	g := mk(&calls)
	for _, a := range []ChatActor{{Kind: "discord", UserID: "1"}, {Kind: "discord", UserID: "2", RoleIDs: []string{"3"}}, {Kind: "telegram", UserID: "4"}} {
		if err := g("a1", true, 0, a); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	if calls != 0 {
		t.Fatal("an empty list must never reach the decision")
	}
}

func TestGuardChannelDecisionRefusesDisabledChannel(t *testing.T) {
	_, mk := guardFixture(t, `{"bot_token":"t","channel_id":"1","approver_user_ids":"11"}`, false)
	var calls int
	if err := mk(&calls)("a1", true, 0, ChatActor{Kind: "discord", UserID: "11"}); !errors.Is(err, ErrChannelNotAuthoritative) || calls != 0 {
		t.Fatalf("disabled: %v calls=%d", err, calls)
	}
}

// ---- Discord (fake REST + fake gateway dispatch) ----

type recordedReq struct {
	method, path string
	body         map[string]any
}

type fakeHTTP struct {
	mu   sync.Mutex
	reqs []recordedReq
	hook func(recordedReq)
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
	}
	rec := recordedReq{r.Method, r.URL.Path, body}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(rec)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"msg-1","result":{"message_id":77}}`)), Header: http.Header{}}, nil
}

func (f *fakeHTTP) find(method, suffix string) *recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reqs {
		if f.reqs[i].method == method && strings.HasSuffix(f.reqs[i].path, suffix) {
			return &f.reqs[i]
		}
	}
	return nil
}

func (f *fakeHTTP) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

type decisionCall struct {
	id      string
	approve bool
	minutes int
	actor   ChatActor
}

func newFakeGateway(t *testing.T, decide func(decisionCall) error) (*DiscordGateway, *fakeHTTP, *[]decisionCall) {
	t.Helper()
	fh := &fakeHTTP{}
	var calls []decisionCall
	var mu sync.Mutex
	g := NewDiscordGateway("tok", "chan1", nil, nil).WithDecider(func(id string, approve bool, minutes int, actor ChatActor) error {
		mu.Lock()
		calls = append(calls, decisionCall{id, approve, minutes, actor})
		mu.Unlock()
		return decide(decisionCall{id, approve, minutes, actor})
	})
	g.httpc = &http.Client{Transport: fh}
	g.apiBase = "http://discord.test/api/v10"
	return g, fh, &calls
}

func buttonPress(customID, user string, roles ...string) interactionData {
	var in interactionData
	raw := map[string]any{
		"id": "int1", "token": "tok1", "application_id": "app1", "type": 3, "channel_id": "chan1",
		"data":    map[string]any{"custom_id": customID, "component_type": 2},
		"member":  map[string]any{"user": map[string]any{"id": user, "username": "pat"}, "roles": roles},
		"message": map[string]any{"id": "msg9"},
	}
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &in)
	return in
}

func TestHeldRequestMessageCarriesApproveButtons(t *testing.T) {
	g, fh, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", ClientName: "c", ServiceName: "s", Method: "POST", Path: "/x"})
	post := fh.find("POST", "/channels/chan1/messages")
	if post == nil {
		t.Fatalf("no message posted: %v", fh.paths())
	}
	rows, _ := post.body["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("components: %v", post.body["components"])
	}
	btns := rows[0].(map[string]any)["components"].([]any)
	var labels, ids []string
	for _, b := range btns {
		m := b.(map[string]any)
		labels = append(labels, m["label"].(string))
		ids = append(ids, m["custom_id"].(string))
	}
	if strings.Join(labels, "|") != "Approve|Approve for 1 h|Reject" || strings.Join(ids, "|") != "dwa:a:ap1|dwa:a1h:ap1|dwa:r:ap1" {
		t.Fatalf("buttons: %v %v", labels, ids)
	}
	if _, ok := g.pendingApprovals.Load("msg-1"); !ok {
		t.Fatal("message not registered for reactions")
	}
}

func TestDiscordButtonPressByApproverDecidesAcksAndEditsMessage(t *testing.T) {
	g, fh, calls := newFakeGateway(t, func(decisionCall) error { return nil })
	g.handleInteraction(buttonPress("dwa:a1h:ap1", "42", "r1"))
	if len(*calls) != 1 || (*calls)[0].id != "ap1" || !(*calls)[0].approve || (*calls)[0].minutes != 60 ||
		(*calls)[0].actor.UserID != "42" || (*calls)[0].actor.Kind != "discord" || strings.Join((*calls)[0].actor.RoleIDs, ",") != "r1" {
		t.Fatalf("decision: %+v", *calls)
	}
	paths := fh.paths()
	if len(paths) < 2 || paths[0] != "POST /api/v10/interactions/int1/tok1/callback" {
		t.Fatalf("the press must be acknowledged first: %v", paths)
	}
	if ack := fh.find("POST", "/interactions/int1/tok1/callback"); ack.body["type"] != float64(6) {
		t.Fatalf("ack body: %v", ack.body)
	}
	edit := fh.find("PATCH", "/channels/chan1/messages/msg9")
	if edit == nil {
		t.Fatalf("message not edited: %v", paths)
	}
	content, _ := edit.body["content"].(string)
	if !strings.Contains(content, "Approved for 1 h by <@42>") || !strings.Contains(content, "<t:") {
		t.Fatalf("result line: %q", content)
	}
	if comps, ok := edit.body["components"].([]any); !ok || len(comps) != 0 {
		t.Fatalf("buttons must be removed: %v", edit.body["components"])
	}
	if fh.find("POST", "/webhooks/app1/tok1") != nil {
		t.Fatal("no private reply on success")
	}
}

func TestDiscordButtonPressByNonApproverGetsEphemeralReplyOnly(t *testing.T) {
	g, fh, calls := newFakeGateway(t, func(decisionCall) error { return ErrNotApprover })
	for _, id := range []string{"dwa:a:ap1", "dwa:a1h:ap1", "dwa:r:ap1"} {
		g.handleInteraction(buttonPress(id, "666"))
	}
	if len(*calls) != 3 {
		t.Fatalf("calls: %d", len(*calls))
	}
	if fh.find("PATCH", "/channels/chan1/messages/msg9") != nil {
		t.Fatal("a refused press must not touch the message")
	}
	fu := fh.find("POST", "/webhooks/app1/tok1")
	if fu == nil || fu.body["flags"] != float64(64) || !strings.Contains(strings.ToLower(fu.body["content"].(string)), "not an approver") {
		t.Fatalf("ephemeral reply: %+v", fu)
	}
}

func TestDiscordButtonPressOnDecidedApprovalShowsExistingDecider(t *testing.T) {
	g, fh, _ := newFakeGateway(t, func(decisionCall) error {
		return &AlreadyDecidedError{Decision: queries.ApprovalDecision{Status: "approved", DecidedBy: "admin:alice", DecidedVia: "web"}}
	})
	g.handleInteraction(buttonPress("dwa:r:ap1", "42"))
	fu := fh.find("POST", "/webhooks/app1/tok1")
	if fu == nil || fu.body["flags"] != float64(64) || !strings.Contains(fu.body["content"].(string), "Already decided (approved) by admin:alice") {
		t.Fatalf("followup: %+v", fu)
	}
	edit := fh.find("PATCH", "/channels/chan1/messages/msg9")
	if edit == nil || !strings.Contains(edit.body["content"].(string), "by admin:alice") {
		t.Fatalf("message should show the existing decision: %+v", edit)
	}
}

func TestDiscordInteractionIgnoresForeignComponentsAndOtherChannels(t *testing.T) {
	g, fh, calls := newFakeGateway(t, func(decisionCall) error { return nil })
	g.handleInteraction(buttonPress("someone-elses-button", "42"))
	if len(fh.paths()) != 0 || len(*calls) != 0 {
		t.Fatalf("foreign custom_id must be ignored: %v", fh.paths())
	}
	in := buttonPress("dwa:a:ap1", "42")
	in.ChannelID = "other-channel"
	g.handleInteraction(in)
	if len(*calls) != 0 {
		t.Fatal("a button from another channel must not decide")
	}
}

func TestDiscordReactionsAndCommandsHonourAllowlist(t *testing.T) {
	g, fh, calls := newFakeGateway(t, func(c decisionCall) error {
		if c.actor.UserID != "42" {
			return ErrNotApprover
		}
		return nil
	})
	g.RegisterApprovalMessage("m1", "ap1")
	var stranger reactionAddData
	stranger.UserID, stranger.ChannelID, stranger.MessageID = "7", "chan1", "m1"
	stranger.Emoji.Name = "✅"
	g.handleReaction(stranger)
	if fh.find("PATCH", "/channels/chan1/messages/m1") != nil {
		t.Fatal("a refused reaction must not edit the message")
	}
	if p := fh.find("POST", "/channels/chan1/messages"); p == nil || !strings.Contains(p.body["content"].(string), "not an approver") {
		t.Fatalf("reply: %v", fh.paths())
	}
	if _, ok := g.pendingApprovals.Load("m1"); !ok {
		t.Fatal("a refused reaction must leave the approval pending")
	}
	listed := stranger
	listed.UserID = "42"
	g.handleReaction(listed)
	if fh.find("PATCH", "/channels/chan1/messages/m1") == nil {
		t.Fatalf("listed user reaction should finish the message: %v", fh.paths())
	}
	if last := (*calls)[len(*calls)-1]; last.id != "ap1" || !last.approve || last.actor.UserID != "42" {
		t.Fatalf("last call: %+v", last)
	}

	// !approve / !reject
	*calls = nil
	fh2 := &fakeHTTP{}
	g.httpc = &http.Client{Transport: fh2}
	var msg messageCreateData
	msg.Content, msg.ChannelID, msg.ID = "!approve ap2", "chan1", "cmd1"
	msg.Author.ID, msg.Author.Username = "7", "eve"
	g.handleCommand(msg)
	if p := fh2.find("POST", "/channels/chan1/messages"); p == nil || !strings.Contains(p.body["content"].(string), "Not an approver") {
		t.Fatalf("command reply: %v", fh2.paths())
	}
	msg.Content, msg.Author.ID = "!reject ap2", "42"
	g.handleCommand(msg)
	if len(*calls) != 2 || (*calls)[1].approve || (*calls)[1].actor.UserID != "42" {
		t.Fatalf("calls: %+v", *calls)
	}
}

func TestDiscordMarkDecidedEditsMessageForDecisionsMadeElsewhere(t *testing.T) {
	g, fh, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"})
	g.MarkDecided("ap1", "**Approved by admin:alice**")
	edit := fh.find("PATCH", "/channels/chan1/messages/msg-1")
	if edit == nil || edit.body["content"] != "**Approved by admin:alice**" {
		t.Fatalf("edit: %+v", edit)
	}
	if comps, _ := edit.body["components"].([]any); len(comps) != 0 {
		t.Fatal("buttons must be removed")
	}
	g.MarkDecided("ap1", "again") // second call: nothing left to edit
	n := 0
	for _, p := range fh.paths() {
		if strings.HasPrefix(p, "PATCH") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("PATCH count=%d", n)
	}
}

func TestNotifierDecisionMadeUpdatesChatMessagesBestEffort(t *testing.T) {
	g, fh, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"})
	n := NewNotifier(nil)
	n.Gateways.Store("chan1", g)
	done := make(chan struct{})
	fh.hook = func(r recordedReq) {
		if r.method == "PATCH" {
			close(done)
		}
	}
	n.DecisionMade("ap1", "admin:alice", ViaWeb, "approved")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("web decision did not update the Discord message")
	}
	edit := fh.find("PATCH", "/channels/chan1/messages/msg-1")
	if !strings.Contains(edit.body["content"].(string), "Approved by admin:alice") {
		t.Fatalf("content: %v", edit.body["content"])
	}
}

// ---- Telegram ----

func newFakeTelegram(t *testing.T, decide func(decisionCall) error) (*TelegramPoller, *fakeHTTP, *[]decisionCall) {
	t.Helper()
	fh := &fakeHTTP{}
	var calls []decisionCall
	p := NewTelegramPoller("1:x", "2", nil, nil).WithDecider(func(id string, approve bool, minutes int, actor ChatActor) error {
		calls = append(calls, decisionCall{id, approve, minutes, actor})
		return decide(decisionCall{id, approve, minutes, actor})
	})
	p.client = &http.Client{Transport: fh}
	return p, fh, &calls
}

func tgCallback(data string, userID int64) *tgCallbackQuery {
	cb := &tgCallbackQuery{ID: "cb1", Data: data}
	cb.From.ID, cb.From.Username = userID, "pat"
	cb.Message.MessageID, cb.Message.Chat.ID = 5, 2
	return cb
}

func TestTelegramCallbackByApproverDecidesAndEdits(t *testing.T) {
	p, fh, calls := newFakeTelegram(t, func(decisionCall) error { return nil })
	p.handleCallback(tgCallback("approve1h:ap1", 4242))
	if len(*calls) != 1 || (*calls)[0].id != "ap1" || !(*calls)[0].approve || (*calls)[0].minutes != 60 ||
		(*calls)[0].actor.Kind != "telegram" || (*calls)[0].actor.UserID != "4242" {
		t.Fatalf("decision: %+v", *calls)
	}
	edit := fh.find("POST", "/editMessageText")
	if edit == nil || !strings.Contains(edit.body["text"].(string), "Approved for 1 h") || !strings.Contains(edit.body["text"].(string), "@pat") {
		t.Fatalf("edit: %+v", edit)
	}
	ans := fh.find("POST", "/answerCallbackQuery")
	if ans == nil || ans.body["text"] != "" {
		t.Fatalf("answer: %+v", ans)
	}
	*calls = nil
	p.handleCallback(tgCallback("reject:ap2", 4242))
	if len(*calls) != 1 || (*calls)[0].approve {
		t.Fatalf("reject: %+v", *calls)
	}
}

func TestTelegramCallbackByNonApproverIsRefused(t *testing.T) {
	p, fh, _ := newFakeTelegram(t, func(decisionCall) error { return ErrNotApprover })
	for _, d := range []string{"approve:ap1", "approve1h:ap1", "reject:ap1"} {
		fh.mu.Lock()
		fh.reqs = nil
		fh.mu.Unlock()
		p.handleCallback(tgCallback(d, 666))
		if fh.find("POST", "/editMessageText") != nil {
			t.Fatalf("%s: refused press must not edit the message", d)
		}
		ans := fh.find("POST", "/answerCallbackQuery")
		if ans == nil || ans.body["text"] != "Not an approver" {
			t.Fatalf("%s: answer %+v", d, ans)
		}
	}
}

func TestTelegramNotificationHasButtonsAndIsUpdatedOnOtherDecisions(t *testing.T) {
	fh := &fakeHTTP{}
	n := NewNotifier(nil)
	n.client = &http.Client{Transport: fh}
	cfg := `{"bot_token":"9:tok","chat_id":"2","approver_user_ids":"1"}`
	if err := n.sendTelegram(cfg, ApprovalNotification{ApprovalID: "ap1", ClientName: "c", ServiceName: "s", Method: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	send := fh.find("POST", "/sendMessage")
	kb := send.body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	var data []string
	for _, b := range kb {
		data = append(data, b.(map[string]any)["callback_data"].(string))
	}
	if strings.Join(data, "|") != "approve:ap1|approve1h:ap1|reject:ap1" {
		t.Fatalf("keyboard: %v", data)
	}
	done := make(chan struct{})
	fh.hook = func(r recordedReq) {
		if strings.HasSuffix(r.path, "/editMessageText") {
			close(done)
		}
	}
	n.DecisionMade("ap1", "approver:ci", ViaTUI, "rejected")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no edit for a decision made elsewhere")
	}
	edit := fh.find("POST", "/editMessageText")
	if edit.body["message_id"] != float64(77) || !strings.Contains(edit.body["text"].(string), "Rejected by approver:ci") {
		t.Fatalf("edit: %+v", edit.body)
	}
}

func TestDiscordBotRESTFallbackHasNoDeadButtonsOrButtonsFooter(t *testing.T) {
	fh := &fakeHTTP{}
	n := NewNotifier(nil)
	n.client = &http.Client{Transport: fh}
	if err := n.sendDiscordBot(`{"bot_token":"t","channel_id":"chan9"}`, ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	post := fh.find("POST", "/channels/chan9/messages")
	if post == nil {
		t.Fatal("nothing posted")
	}
	if post.body["components"] != nil {
		t.Fatalf("no listener runs, so no buttons: %v", post.body["components"])
	}
	blob, _ := json.Marshal(post.body)
	if strings.Contains(strings.ToLower(string(blob)), "buttons") {
		t.Fatalf("footer mentions buttons that do not exist: %s", blob)
	}
}

// ---- fx1116 chat hardening ----

func TestDiscordInteractionFromOtherChannelIsSilent(t *testing.T) {
	g, fh, calls := newFakeGateway(t, func(decisionCall) error { return nil })
	in := buttonPress("dwa:a:ap1", "42")
	in.ChannelID = "other-channel"
	g.handleInteraction(in)
	if len(fh.paths()) != 0 || len(*calls) != 0 {
		t.Fatalf("mismatch must send nothing (no ack, no ephemeral reply): %v %v", fh.paths(), *calls)
	}
}

func TestDiscordRESTClientHasTimeout(t *testing.T) {
	g := NewDiscordGateway("t", "c", nil, nil)
	if g.httpc.Timeout != discordRESTTimeout || discordRESTTimeout != 10*time.Second {
		t.Fatalf("timeout = %v", g.httpc.Timeout)
	}
	if cap(g.interactionSem) != maxConcurrentInteractions {
		t.Fatal("interaction semaphore missing")
	}
}

type seqRT struct {
	mu    sync.Mutex
	codes []int
	n     int
}

func (s *seqRT) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	code := 200
	if s.n < len(s.codes) {
		code = s.codes[s.n]
	}
	s.n++
	s.mu.Unlock()
	body := `{}`
	if code == 429 {
		body = `{"retry_after":0.01}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestDiscordDoRetriesOneRateLimit(t *testing.T) {
	g := NewDiscordGateway("t", "c", nil, nil)
	rt := &seqRT{codes: []int{429, 429, 200}}
	g.httpc = &http.Client{Transport: rt}
	g.discordDo("PATCH", "http://d.test/x", strings.NewReader(`{}`), "application/json")
	if rt.n != 2 {
		t.Fatalf("want one retry (2 calls), got %d", rt.n)
	}
	rt2 := &seqRT{codes: []int{429, 200}}
	g.httpc = &http.Client{Transport: rt2}
	g.discordDo("PATCH", "http://d.test/x", strings.NewReader(`{}`), "application/json")
	if rt2.n != 2 {
		t.Fatalf("429 then success: %d calls", rt2.n)
	}
}

// A decision by Discord must still update the other chat surfaces: only the
// message the deciding interaction edited is skipped (it dropped its own ref).
func TestDecisionMadeViaChatStillUpdatesOtherGateways(t *testing.T) {
	g1, fh1, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g2, fh2, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g1.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"})
	g2.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"})
	n := NewNotifier(nil)
	t.Cleanup(n.Stop)
	n.Gateways.Store("c1", g1)
	n.Gateways.Store("c2", g2)
	g1.finishMessage("msg-1", "ap1", "Approved by <@1>") // g1 handled its own press
	n.DecisionMade("ap1", "discord:1", ViaDiscord, "approved")
	deadline := time.Now().Add(3 * time.Second)
	for fh2.find("PATCH", "/channels/chan1/messages/msg-1") == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fh2.find("PATCH", "/channels/chan1/messages/msg-1") == nil {
		t.Fatal("the other gateway's message was skipped because via==discord")
	}
	n1 := 0
	for _, p := range fh1.paths() {
		if strings.HasPrefix(p, "PATCH") {
			n1++
		}
	}
	if n1 != 1 {
		t.Fatalf("deciding gateway's message edited %d times, want 1 (its own edit only)", n1)
	}
}

func TestDecisionMadeQueueIsBounded(t *testing.T) {
	n := NewNotifier(nil)
	t.Cleanup(n.Stop)
	before := runtime.NumGoroutine()
	start := time.Now()
	for i := 0; i < 5000; i++ {
		n.DecisionMade("ap"+strconv.Itoa(i), "admin:a", ViaWeb, "approved")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("DecisionMade blocked the decider")
	}
	if grown := runtime.NumGoroutine() - before; grown > decisionWorkers+3 {
		t.Fatalf("goroutines grew by %d: one per decision?", grown)
	}
}

func TestTTLMapExpiresAndIsConcurrencySafe(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &ttlMap[string]{ttl: time.Hour, now: func() time.Time { return now }}
	m.Store("a", "1")
	now = now.Add(30 * time.Minute)
	m.Store("b", "2")
	now = now.Add(45 * time.Minute) // a is 75 min old, b 45
	if _, ok := m.Load("a"); ok {
		t.Fatal("a should have expired")
	}
	if v, ok := m.Load("b"); !ok || v != "2" {
		t.Fatal("b should live")
	}
	now = now.Add(2 * time.Hour)
	m.Store("c", "3") // triggers the sweep
	if m.Len() != 1 {
		t.Fatalf("sweep left %d entries", m.Len())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := strconv.Itoa(j % 7)
				m.Store(k, "x")
				m.Load(k)
				m.LoadAndDelete(k)
			}
		}(i)
	}
	wg.Wait()
}

// A decision that landed before the message ref was stored still updates the
// message right after it is sent.
func TestDiscordDecisionBeforeStoreEditsImmediately(t *testing.T) {
	g, fh, _ := newFakeGateway(t, func(decisionCall) error { return nil })
	g.WithDecisionLookup(func(string) (queries.ApprovalDecision, error) {
		return queries.ApprovalDecision{Status: "approved", DecidedBy: "admin:alice", DecidedVia: "web"}, nil
	})
	g.SendApprovalMessage(ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"})
	edit := fh.find("PATCH", "/channels/chan1/messages/msg-1")
	if edit == nil || !strings.Contains(edit.body["content"].(string), "Approved by admin:alice") {
		t.Fatalf("not edited: %v", fh.paths())
	}
}

func TestTelegramDecisionBeforeStoreEditsImmediately(t *testing.T) {
	fh := &fakeHTTP{}
	n := NewNotifier(nil).WithDecisionLookup(func(string) (queries.ApprovalDecision, error) {
		return queries.ApprovalDecision{Status: "rejected", DecidedBy: "approver:ci"}, nil
	})
	t.Cleanup(n.Stop)
	n.client = &http.Client{Transport: fh}
	t.Setenv("DUCKWAY_TELEGRAM_BASE_URL", "http://tg.test")
	if err := n.sendTelegram(`{"bot_token":"1:x","chat_id":"2"}`, ApprovalNotification{ApprovalID: "ap1", Method: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	edit := fh.find("POST", "/editMessageText")
	if edit == nil || !strings.Contains(edit.body["text"].(string), "Rejected by approver:ci") {
		t.Fatalf("not edited: %v", fh.paths())
	}
	if _, ok := n.tgMessages.Load("ap1"); ok {
		if refs, _ := n.tgMessages.Load("ap1"); len(refs) != 0 {
			t.Fatal("ref should be dropped after the immediate edit")
		}
	}
}

func TestTelegramAlreadyDecidedNamesDecider(t *testing.T) {
	p := NewTelegramPoller("1:x", "2", nil, nil).WithDecider(func(string, bool, int, ChatActor) error {
		return &AlreadyDecidedError{Decision: queries.ApprovalDecision{Status: "approved", DecidedBy: "admin:alice"}}
	})
	var answers []string
	p.client = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "answerCallbackQuery") {
			answers = append(answers, string(b))
		}
		return okJSON(`{}`), nil
	})}
	p.handleCallback(tgCB("cb1", "approve:a1", 2))
	if len(answers) != 1 || !strings.Contains(answers[0], "Already decided (approved) by admin:alice") {
		t.Fatalf("answers: %v", answers)
	}
}

func tgCB(id, data string, chat int64) *tgCallbackQuery {
	cb := &tgCallbackQuery{ID: id, Data: data}
	cb.Message.MessageID, cb.Message.Chat.ID = 5, chat
	return cb
}

// A button pressed in any other chat is ignored: no answer, no decision.
func TestTelegramCallbackFromOtherChatIsIgnored(t *testing.T) {
	p, fh, calls := newFakeTelegram(t, func(decisionCall) error { return nil })
	cb := tgCallback("approve:ap1", 4242)
	cb.Message.Chat.ID = 999 // the poller is configured for chat "2"
	p.handleCallback(cb)
	if len(*calls) != 0 || len(fh.paths()) != 0 {
		t.Fatalf("decisions=%v requests=%v", *calls, fh.paths())
	}
}
