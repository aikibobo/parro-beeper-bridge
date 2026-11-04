package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// ---- fake Matrix side: records what the bridge sends ----

type sentEvent struct {
	room    id.RoomID
	sender  id.UserID
	evtType event.Type
	content *event.Content
	ts      time.Time
	eventID id.EventID
}

type fakeMatrix struct {
	lock   sync.Mutex
	events []sentEvent
	rooms  map[id.RoomID]string
	nextID int
}

func newFakeMatrix() *fakeMatrix { return &fakeMatrix{rooms: map[id.RoomID]string{}} }

func (m *fakeMatrix) record(e sentEvent) id.EventID {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.nextID++
	e.eventID = id.EventID(fmt.Sprintf("$e%d", m.nextID))
	m.events = append(m.events, e)
	return e.eventID
}

func (m *fakeMatrix) snapshot() []sentEvent {
	m.lock.Lock()
	defer m.lock.Unlock()
	return append([]sentEvent(nil), m.events...)
}

func (m *fakeMatrix) roomByName(name string) id.RoomID {
	m.lock.Lock()
	defer m.lock.Unlock()
	for r, n := range m.rooms {
		if n == name {
			return r
		}
	}
	return ""
}

func (m *fakeMatrix) Init(*bridgev2.Bridge)       {}
func (m *fakeMatrix) Start(context.Context) error { return nil }
func (m *fakeMatrix) PreStop()                    {}
func (m *fakeMatrix) Stop()                       {}
func (m *fakeMatrix) GetCapabilities() *bridgev2.MatrixCapabilities {
	return &bridgev2.MatrixCapabilities{}
}
func (m *fakeMatrix) ParseGhostMXID(u id.UserID) (networkid.UserID, bool) {
	s := string(u)
	if !strings.HasPrefix(s, "@ghost_") {
		return "", false
	}
	return networkid.UserID(strings.TrimSuffix(strings.TrimPrefix(s, "@ghost_"), ":test")), true
}
func (m *fakeMatrix) FormatGhostMXID(u networkid.UserID) id.UserID {
	return id.UserID("@ghost_" + string(u) + ":test")
}
func (m *fakeMatrix) GhostIntent(u networkid.UserID) bridgev2.MatrixAPI {
	return &fakeIntent{m: m, mxid: m.FormatGhostMXID(u)}
}
func (m *fakeMatrix) NewUserIntent(context.Context, id.UserID, string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", fmt.Errorf("no double puppeting in tests")
}
func (m *fakeMatrix) BotIntent() bridgev2.MatrixAPI                               { return &fakeIntent{m: m, mxid: "@bot:test"} }
func (m *fakeMatrix) SendBridgeStatus(context.Context, *status.BridgeState) error { return nil }
func (m *fakeMatrix) SendMessageStatus(context.Context, *bridgev2.MessageStatus, *bridgev2.MessageStatusEventInfo) {
}
func (m *fakeMatrix) GenerateContentURI(context.Context, networkid.MediaID) (id.ContentURIString, error) {
	return "mxc://test/generated", nil
}
func (m *fakeMatrix) ParseContentURI(context.Context, id.ContentURIString) (networkid.MediaID, error) {
	return nil, fmt.Errorf("not supported")
}
func (m *fakeMatrix) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return &event.PowerLevelsEventContent{}, nil
}
func (m *fakeMatrix) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	return map[id.UserID]*event.MemberEventContent{}, nil
}
func (m *fakeMatrix) GetMemberInfo(context.Context, id.RoomID, id.UserID) (*event.MemberEventContent, error) {
	return nil, nil
}
func (m *fakeMatrix) BatchSend(context.Context, id.RoomID, *mautrix.ReqBeeperBatchSend, []*bridgev2.MatrixSendExtra) (*mautrix.RespBeeperBatchSend, error) {
	return nil, fmt.Errorf("batch send disabled in tests")
}
func (m *fakeMatrix) GenerateDeterministicRoomID(networkid.PortalKey) id.RoomID { return "" }
func (m *fakeMatrix) GenerateDeterministicEventID(id.RoomID, networkid.PortalKey, networkid.MessageID, networkid.PartID) id.EventID {
	return ""
}
func (m *fakeMatrix) GenerateReactionEventID(id.RoomID, *database.Message, networkid.UserID, networkid.EmojiID) id.EventID {
	return ""
}
func (m *fakeMatrix) ServerName() string { return "test" }

type fakeIntent struct {
	m    *fakeMatrix
	mxid id.UserID
}

func (f *fakeIntent) GetMXID() id.UserID   { return f.mxid }
func (f *fakeIntent) IsDoublePuppet() bool { return false }
func (f *fakeIntent) SendMessage(_ context.Context, room id.RoomID, t event.Type, c *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	e := sentEvent{room: room, sender: f.mxid, evtType: t, content: c}
	if extra != nil {
		e.ts = extra.Timestamp
	}
	return &mautrix.RespSendEvent{EventID: f.m.record(e)}, nil
}
func (f *fakeIntent) SendState(_ context.Context, room id.RoomID, t event.Type, _ string, c *event.Content, ts time.Time) (*mautrix.RespSendEvent, error) {
	return &mautrix.RespSendEvent{EventID: f.m.record(sentEvent{room: room, sender: f.mxid, evtType: t, content: c, ts: ts})}, nil
}
func (f *fakeIntent) MarkRead(context.Context, id.RoomID, id.EventID, time.Time) error { return nil }
func (f *fakeIntent) MarkUnread(context.Context, id.RoomID, bool) error                { return nil }
func (f *fakeIntent) MarkTyping(context.Context, id.RoomID, bridgev2.TypingType, time.Duration) error {
	return nil
}
func (f *fakeIntent) DownloadMedia(context.Context, id.ContentURIString, *event.EncryptedFileInfo) ([]byte, error) {
	return nil, fmt.Errorf("not supported")
}
func (f *fakeIntent) DownloadMediaToFile(context.Context, id.ContentURIString, *event.EncryptedFileInfo, bool, func(*os.File) error) error {
	return fmt.Errorf("not supported")
}
func (f *fakeIntent) UploadMedia(_ context.Context, _ id.RoomID, data []byte, name, _ string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	return id.ContentURIString(fmt.Sprintf("mxc://test/%s-%d", name, len(data))), nil, nil
}
func (f *fakeIntent) UploadMediaStream(context.Context, id.RoomID, int64, bool, bridgev2.FileStreamCallback) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	return "", nil, fmt.Errorf("not supported")
}
func (f *fakeIntent) SetDisplayName(context.Context, string) error            { return nil }
func (f *fakeIntent) SetAvatarURL(context.Context, id.ContentURIString) error { return nil }
func (f *fakeIntent) SetExtraProfileMeta(context.Context, any) error          { return nil }
func (f *fakeIntent) SetProfile(context.Context, any) error                   { return nil }
func (f *fakeIntent) CreateRoom(_ context.Context, req *mautrix.ReqCreateRoom) (id.RoomID, error) {
	f.m.lock.Lock()
	defer f.m.lock.Unlock()
	room := id.RoomID(fmt.Sprintf("!room%d:test", len(f.m.rooms)+1))
	name := req.Name
	for _, state := range req.InitialState {
		if state.Type == event.StateRoomName {
			if c, ok := state.Content.Parsed.(*event.RoomNameEventContent); ok {
				name = c.Name
			}
		}
	}
	f.m.rooms[room] = name
	return room, nil
}
func (f *fakeIntent) DeleteRoom(context.Context, id.RoomID, bool) error { return nil }
func (f *fakeIntent) EnsureJoined(context.Context, id.RoomID, ...bridgev2.EnsureJoinedParams) error {
	return nil
}
func (f *fakeIntent) EnsureInvited(context.Context, id.RoomID, id.UserID) error     { return nil }
func (f *fakeIntent) TagRoom(context.Context, id.RoomID, event.RoomTag, bool) error { return nil }
func (f *fakeIntent) MuteRoom(context.Context, id.RoomID, time.Time) error          { return nil }
func (f *fakeIntent) GetEvent(context.Context, id.RoomID, id.EventID) (*event.Event, error) {
	return nil, fmt.Errorf("not supported")
}

// ---- fake Parro: the API shapes seen live, with Range paging and rotating tokens ----

type fakeMessage struct {
	id         int64
	sender     int64
	text       string
	status     string
	created    time.Time
	deleted    bool
	attachment string
}

type fakeAnnouncement struct {
	id       int64
	owner    int64
	title    string
	contents string
	sort     time.Time
}

type fakeParro struct {
	lock          sync.Mutex
	baseURL       string
	access        string
	refresh       string
	rotations     int
	refreshBroken bool
	groups        []map[string]any
	rooms         []map[string]any
	messages      map[int64][]fakeMessage      // oldest first
	announcements map[int64][]fakeAnnouncement // oldest first
}

func link(id int64) []map[string]any { return []map[string]any{{"id": id, "rel": "self"}} }

func identity(id int64) map[string]any {
	return map[string]any{"links": link(id), "firstname": "Person", "surname": strconv.FormatInt(id, 10), "role": "GUARDIAN"}
}

func (p *fakeParro) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.lock.Lock()
	defer p.lock.Unlock()
	path := r.URL.Path
	switch {
	case path == "/idp/idp/oauth2/token":
		_ = r.ParseForm()
		if p.refreshBroken || r.Form.Get("refresh_token") != p.refresh {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.rotations++
		p.access = fmt.Sprintf("access-%d", p.rotations)
		p.refresh = fmt.Sprintf("refresh-%d", p.rotations)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": p.access, "refresh_token": p.refresh})
		return
	case strings.HasPrefix(path, "/media/"):
		_, _ = w.Write([]byte("image-bytes"))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+p.access {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case path == "/rest/account/me":
		me := identity(1)
		me["guardians"] = []any{identity(2)}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": me})
	case path == "/rest/group":
		p.list(w, r, p.groups)
	case path == "/rest/chatroom":
		p.list(w, r, p.rooms)
	case path == "/rest/event":
		group, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
		var items []map[string]any
		anns := p.announcements[group]
		for i := len(anns) - 1; i >= 0; i-- {
			a := anns[i]
			items = append(items, map[string]any{
				"links": link(a.id), "title": a.title, "contents": a.contents, "owner": identity(a.owner),
				"sortDate": a.sort.Format(time.RFC3339Nano), "dtype": "event.RAnnouncementEvent",
			})
		}
		p.list(w, r, items)
	case strings.HasPrefix(path, "/rest/chatroom/") && strings.HasSuffix(path, "/chatmessage"):
		room, _ := strconv.ParseInt(strings.Split(path, "/")[3], 10, 64)
		var items []map[string]any
		msgs := p.messages[room]
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			item := map[string]any{"links": link(m.id), "identity": identity(m.sender), "createdAt": m.created.Format(time.RFC3339Nano), "deleted": m.deleted}
			if m.status != "" {
				item["dtype"], item["status"] = "chat.RChatStatusMessage", m.status
			} else {
				item["dtype"], item["text"] = "chat.RChatTextMessage", m.text
			}
			if m.attachment != "" {
				item["attachment"] = map[string]any{"attachmentType": "IMAGE", "entries": []any{
					map[string]any{"type": "THUMBNAIL", "url": p.baseURL + "/media/thumb.jpg", "contentType": "image/jpeg"},
					map[string]any{"type": "SOURCE", "url": p.baseURL + "/media/" + m.attachment, "contentType": "image/jpeg"},
				}}
			}
			items = append(items, item)
		}
		p.list(w, r, items)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// list answers like Parro: 206 with at most the requested Range, 416 past the end.
func (p *fakeParro) list(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	start, end := 0, 99
	if rng := r.Header.Get("Range"); rng != "" {
		_, _ = fmt.Sscanf(rng, "items=%d-%d", &start, &end)
	}
	if start > 0 && start >= len(items) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	end = min(end+1, len(items))
	page := items[start:end]
	if page == nil {
		page = []map[string]any{}
	}
	w.WriteHeader(http.StatusPartialContent)
	_ = json.NewEncoder(w).Encode(map[string]any{"items": page})
}

func (p *fakeParro) addMessages(room int64, msgs ...fakeMessage) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.messages[room] = append(p.messages[room], msgs...)
	sort.SliceStable(p.messages[room], func(i, j int) bool { return p.messages[room][i].created.Before(p.messages[room][j].created) })
}
