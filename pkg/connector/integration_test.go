package connector

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle waits until no new Matrix events arrive for a moment, so "nothing happened" can be asserted.
func settle(m *fakeMatrix) int {
	last := -1
	for {
		n := len(m.snapshot())
		if n == last {
			return n
		}
		last = n
		time.Sleep(400 * time.Millisecond)
	}
}

func bodiesIn(m *fakeMatrix, room id.RoomID) []string {
	var out []string
	for _, e := range m.snapshot() {
		if e.room != room || e.evtType != event.EventMessage {
			continue
		}
		if c, ok := e.content.Parsed.(*event.MessageEventContent); ok && c.RelatesTo == nil {
			out = append(out, c.Body)
		}
	}
	return out
}

func countBody(bodies []string, body string) int {
	n := 0
	for _, b := range bodies {
		if b == body {
			n++
		}
	}
	return n
}

// TestWholeChain runs the real bridgev2 core and this connector against a fake Parro and a fake Matrix server.
//
// protects=the user-visible promises of the spec end to end: discovery of exactly the right chats, a 30 day history,
// no duplicates after a restart, complete catch-up over several pages, token rotation saved to the database, edits,
// deletes, attachments, new chats, and a clear stop on a revoked login;
// fails_when=any of those breaks in the glue between poller, bridgev2 queue, backfill and database;
// no_claim=the real Parro and Beeper servers, encryption, launchd and timing of the 15 minute loop, which this test
// replaces with direct calls to pollOnce. Parro shapes are copied from what was seen live on 2026-10-30.
func TestWholeChain(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	day := 24 * time.Hour

	fp := &fakeParro{
		access: "access-0", refresh: "refresh-0",
		groups: []map[string]any{
			{"links": link(10), "name": "School", "schooljaar": 2026, "type": "SCHOOLWIDE"},
			{"links": link(11), "name": "Klas", "schooljaar": 2026, "type": "NORMAL"},
			{"links": link(9), "name": "Vorig jaar", "schooljaar": 2025, "type": "NORMAL"},
		},
		rooms: []map[string]any{
			{"links": link(20), "title": "Gezin", "type": "FAMILY", "active": true},
			{"links": link(21), "title": "Ouders", "type": "GROUP", "active": true},
			{"links": link(22), "title": "Archief", "type": "GROUP", "active": true, "archived": true},
		},
		messages: map[int64][]fakeMessage{},
		announcements: map[int64][]fakeAnnouncement{
			10: {{id: 100, owner: 7, title: "Oud nieuws", contents: "x", sort: now.Add(-40 * day)}, {id: 101, owner: 7, title: "Studiedag", contents: "Vrij", sort: now.Add(-2 * day)}},
			11: {{id: 110, owner: 8, title: "Gym", contents: "Neem schoenen mee", sort: now.Add(-1 * day)}},
		},
	}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	fp.baseURL = srv.URL
	defer parro.SetEndpointsForTesting(srv.URL+"/rest", srv.URL+"/idp")()

	fp.addMessages(20,
		fakeMessage{id: 200, sender: 5, text: "te oud", created: now.Add(-40 * day)},
		fakeMessage{id: 201, sender: 5, text: "hallo", created: now.Add(-3 * day)},
		fakeMessage{id: 202, sender: 2, text: "van mij", created: now.Add(-3*day + time.Hour)},
		fakeMessage{id: 203, sender: 5, status: "ENTER", created: now.Add(-3*day + 2*time.Hour)},
		fakeMessage{id: 204, sender: 5, text: "foto", attachment: "a.jpg", created: now.Add(-2 * day)},
	)
	for i := range 120 {
		fp.addMessages(21, fakeMessage{id: int64(1000 + i), sender: int64(30 + i%5), text: fmt.Sprintf("ouder %d", i), created: now.Add(-10*day + time.Duration(i)*time.Minute)})
	}

	db, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "bridge.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &bridgeconfig.BridgeConfig{
		CommandPrefix: "!parro",
		SplitPortals:  true,
		Backfill:      bridgeconfig.BackfillConfig{Enabled: true, MaxInitialMessages: 50, MaxCatchupMessages: 500},
	}
	fm := newFakeMatrix()
	br := bridgev2.NewBridge("test", db, zerolog.Nop(), cfg, fm, &Connector{}, commands.NewProcessor)
	if err := br.StartConnectors(ctx); err != nil {
		t.Fatal(err)
	}
	defer br.Stop()
	user, err := br.GetUserByMXID(ctx, "@aik:test")
	if err != nil {
		t.Fatal(err)
	}
	ul, err := user.NewLogin(ctx, &database.UserLogin{ID: "1", Metadata: &UserLoginMetadata{Tokens: parro.Tokens{AccessToken: "access-0", RefreshToken: "refresh-0"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := ul.Client.(*Client)
	poll := func() bool {
		_, ok := client.pollOnce(br.Log.WithContext(br.BackgroundCtx))
		return ok
	}

	// First poll: exactly the four current chats, with their history inside the window.
	if !poll() {
		t.Fatal("first poll stopped")
	}
	waitFor(t, "four chats", func() bool { return len(fm.rooms) == 4 })
	for _, name := range []string{"Vorig jaar", "Archief"} {
		if fm.roomByName(name) != "" {
			t.Fatalf("chat %q should not exist", name)
		}
	}
	gezin, ouders, school := fm.roomByName("Gezin"), fm.roomByName("Ouders"), fm.roomByName("School")
	waitFor(t, "history", func() bool {
		return len(bodiesIn(fm, ouders)) == 120 && slices.Contains(bodiesIn(fm, gezin), "a.jpg") && len(bodiesIn(fm, school)) == 1
	})
	g := bodiesIn(fm, gezin)
	if !slices.Equal(g, []string{"hallo", "van mij", "foto", "a.jpg"}) {
		t.Fatalf("gezin history = %q", g)
	}
	if b := bodiesIn(fm, school); b[0] != "Studiedag\n\nVrij" {
		t.Fatalf("school history = %q", b)
	}
	for _, e := range fm.snapshot() {
		if c, ok := e.content.Parsed.(*event.MessageEventContent); ok && c.Body == "van mij" && e.sender != fm.FormatGhostMXID("2") {
			t.Fatalf("own message sent as %s", e.sender)
		}
	}

	// A restart with nothing new sends nothing.
	before := settle(fm)
	if !poll() {
		t.Fatal("second poll stopped")
	}
	if after := settle(fm); after != before {
		t.Fatalf("idle poll sent %d events", after-before)
	}

	// Downtime: 130 new messages over two pages, while the access token expired. All arrive once, in order,
	// with their original time, and the rotated token is in the database.
	var catchUp []fakeMessage
	for i := range 130 {
		catchUp = append(catchUp, fakeMessage{id: int64(5000 + i), sender: 31, text: fmt.Sprintf("nieuw %d", i), created: now.Add(-time.Hour + time.Duration(i)*time.Second)})
	}
	fp.addMessages(21, catchUp...)
	fp.lock.Lock()
	fp.access = "expired"
	fp.lock.Unlock()
	if !poll() {
		t.Fatal("catch-up poll stopped")
	}
	waitFor(t, "catch-up", func() bool { return len(bodiesIn(fm, ouders)) == 250 })
	got := bodiesIn(fm, ouders)[120:]
	for i, body := range got {
		if body != fmt.Sprintf("nieuw %d", i) {
			t.Fatalf("catch-up message %d = %q", i, body)
		}
	}
	for _, e := range fm.snapshot() {
		if c, ok := e.content.Parsed.(*event.MessageEventContent); ok && c.Body == "nieuw 0" && e.ts.Sub(catchUp[0].created).Abs() > time.Millisecond {
			t.Fatalf("timestamp %s, want %s", e.ts, catchUp[0].created)
		}
	}
	stored, err := br.DB.UserLogin.GetByID(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	fp.lock.Lock()
	wantRefresh := fp.refresh
	fp.lock.Unlock()
	if stored.Metadata.(*UserLoginMetadata).Tokens.RefreshToken != wantRefresh {
		t.Fatal("rotated refresh token was not saved")
	}

	// An edited announcement becomes an edit, a deleted chat message a redaction, a new chat a new room.
	fp.lock.Lock()
	fp.announcements[10][1].contents = "Vrij, ook de BSO"
	fp.messages[20][1].deleted = true
	fp.rooms = append(fp.rooms, map[string]any{"links": link(23), "title": "Nieuw", "type": "GROUP", "active": true})
	fp.messages[23] = []fakeMessage{{id: 2300, sender: 40, text: "welkom", created: now.Add(-time.Minute)}}
	fp.lock.Unlock()
	if !poll() {
		t.Fatal("change poll stopped")
	}
	waitFor(t, "edit, redaction and new chat", func() bool {
		var edit, redaction bool
		for _, e := range fm.snapshot() {
			if c, ok := e.content.Parsed.(*event.MessageEventContent); ok && c.RelatesTo != nil && c.RelatesTo.Type == event.RelReplace && c.NewContent != nil && c.NewContent.Body == "Studiedag\n\nVrij, ook de BSO" {
				edit = true
			}
			if e.evtType == event.EventRedaction && e.room == gezin {
				redaction = true
			}
		}
		nieuw := fm.roomByName("Nieuw")
		return edit && redaction && nieuw != "" && countBody(bodiesIn(fm, nieuw), "welkom") == 1
	})

	// A revoked login stops polling and leaves the marker the watchdog reads.
	fp.lock.Lock()
	fp.access, fp.refreshBroken = "revoked", true
	fp.lock.Unlock()
	if poll() {
		t.Fatal("poll kept going after the login was revoked")
	}
	if _, err := os.Stat(needsLoginFile); err != nil {
		t.Fatalf("no login marker: %v", err)
	}
}
