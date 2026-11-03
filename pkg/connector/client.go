package connector

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

const (
	errParroBadCredentials status.BridgeStateErrorCode = "parro-bad-credentials"
	errParroConnect        status.BridgeStateErrorCode = "parro-connect-error"
	errParroAPIChanged     status.BridgeStateErrorCode = "parro-api-changed"

	pollInterval = 15 * time.Minute
	pollJitter   = time.Minute
	wakeCheck    = time.Minute
	failedLimit  = 3
	maxBackoff   = 2 * time.Hour
	maxRetryWait = 6 * time.Hour
	historyDays  = 30

	// resetFile, when present in the working directory at start, recreates all chats with fresh history.
	resetFile = "reset-portals"
	// heartbeatFile is touched after every successful poll; scripts/watchdog.sh alarms when it goes stale.
	heartbeatFile = "last-poll-ok"
	// needsLoginFile tells the watchdog that Parro rejected the login, so its alert can say what to do.
	needsLoginFile = "needs-login"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		errParroBadCredentials: "Parro heeft de login geweigerd, log opnieuw in",
		errParroConnect:        "Parro is niet bereikbaar",
		errParroAPIChanged:     "Parro is veranderd, deze bridge heeft een update nodig",
	})
}

// ErrReadOnly is what a message typed in Beeper gets: it is never sent to Parro.
var ErrReadOnly = bridgev2.WrapErrorInStatus(errors.New("Parro bridge is read-only, stuur dit via de Parro-app.")).
	WithStatus(event.MessageStatusFail).WithIsCertain(true).WithSendNotice(true).WithErrorAsMessage().WithErrorReason(event.MessageStatusUnsupported)

var roomCaps = &event.RoomFeatures{ID: "parro.capabilities.read_only.1"}

// source is one Parro conversation: a group or a chat room.
type source struct {
	portalID networkid.PortalID
	name     string
	fetch    func(ctx context.Context, stop func([]item) bool) ([]item, error)
}

// Client is one logged-in Parro account.
type Client struct {
	Main      *Connector
	UserLogin *bridgev2.UserLogin
	API       *parro.Client

	lock      sync.Mutex
	ownIDs    map[string]bool
	people    map[string]parro.Identity
	sources   map[networkid.PortalID]source
	resynced  map[networkid.PortalID]bool
	stop      context.CancelFunc
	failCount int
}

var (
	_ bridgev2.NetworkAPI            = (*Client)(nil)
	_ bridgev2.NetworkAPIWithUserID  = (*Client)(nil)
	_ bridgev2.BackfillingNetworkAPI = (*Client)(nil)
)

// Connect starts polling.
func (c *Client) Connect(ctx context.Context) {
	if !c.API.HasTokens() {
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: errParroBadCredentials})
		return
	}
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	c.lock.Lock()
	if c.stop != nil {
		c.stop()
	}
	runCtx, cancel := context.WithCancel(c.UserLogin.Log.WithContext(c.Main.Bridge.BackgroundCtx))
	c.stop = cancel
	c.lock.Unlock()
	go c.pollLoop(runCtx)
}

// pollLoop checks the clock every minute instead of sleeping 15 minutes, so a Mac waking from sleep catches up right away.
func (c *Client) pollLoop(ctx context.Context) {
	c.resetPortalsIfRequested(ctx)
	next := time.Now()
	ticker := time.NewTicker(wakeCheck)
	defer ticker.Stop()
	for {
		if !time.Now().Before(next) {
			wait, keepGoing := c.pollOnce(ctx)
			if !keepGoing {
				return
			}
			next = time.Now().Add(wait + rand.N(pollJitter))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollDelay backs off after failures, and never polls sooner than Parro asked for.
func pollDelay(failedCycles int, retryAfter time.Duration) time.Duration {
	wait := pollInterval
	for range min(failedCycles, 4) {
		wait *= 2
	}
	return max(min(wait, maxBackoff), min(retryAfter, maxRetryWait))
}

// pollOnce returns how long to wait, and false when polling must stop until the user logs in again.
func (c *Client) pollOnce(ctx context.Context) (time.Duration, bool) {
	err := c.sync(ctx)
	switch {
	case err == nil:
		c.failCount = 0
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		if err := writeHeartbeat(heartbeatFile); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to write heartbeat")
		}
		_ = os.Remove(needsLoginFile)
		return pollInterval, true
	case errors.Is(err, parro.ErrBadCredentials):
		zerolog.Ctx(ctx).Err(err).Msg("Parro rejected the login")
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: errParroBadCredentials})
		if err := os.WriteFile(needsLoginFile, nil, 0o600); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to write login marker")
		}
		return 0, false
	case ctx.Err() != nil:
		return 0, false
	}
	c.failCount++
	zerolog.Ctx(ctx).Err(err).Int("failed_cycles", c.failCount).Msg("Parro poll failed")
	if errors.Is(err, parro.ErrAPIChanged) {
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: errParroAPIChanged})
	} else if c.failCount >= failedLimit {
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: errParroConnect})
	}
	var retryAfter time.Duration
	var httpErr *parro.HTTPError
	if errors.As(err, &httpErr) {
		retryAfter = httpErr.RetryAfter
	}
	return pollDelay(c.failCount, retryAfter), true
}

func writeHeartbeat(path string) error {
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

func (c *Client) resetPortalsIfRequested(ctx context.Context) {
	if _, err := os.Stat(resetFile); err != nil {
		return
	}
	log := zerolog.Ctx(ctx)
	portals, err := c.Main.Bridge.GetAllPortals(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list portals for reset")
		return
	}
	for _, portal := range portals {
		mxid := portal.MXID
		if err := portal.Delete(ctx); err != nil {
			log.Err(err).Msg("Failed to delete portal")
			continue
		}
		if mxid != "" {
			if err := c.Main.Bridge.Bot.DeleteRoom(ctx, mxid, false); err != nil {
				log.Err(err).Msg("Failed to delete room")
			}
		}
	}
	log.Info().Int("portals", len(portals)).Msg("Reset portals")
	if err := os.Remove(resetFile); err != nil {
		log.Err(err).Msg("Failed to remove reset file")
	}
}

func (c *Client) sync(ctx context.Context) error {
	account, err := c.API.GetAccount(ctx)
	if err != nil {
		return err
	}
	own, err := account.OwnIDs()
	if err != nil {
		return err
	}
	c.lock.Lock()
	c.ownIDs = own
	c.lock.Unlock()
	sources, err := c.discover(ctx)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if err := c.syncSource(ctx, src); err != nil {
			return fmt.Errorf("sync %s: %w", src.portalID, err)
		}
	}
	return nil
}

// discover finds the conversations on every poll, so a new school year or chat just appears.
func (c *Client) discover(ctx context.Context) ([]source, error) {
	groups, err := c.API.GetGroups(ctx)
	if err != nil {
		return nil, err
	}
	rooms, err := c.API.GetChatRooms(ctx)
	if err != nil {
		return nil, err
	}
	var sources []source
	for _, g := range currentGroups(groups) {
		id, err := g.ID()
		if err != nil {
			return nil, err
		}
		sources = append(sources, source{portalID: makeGroupPortalID(id), name: g.Name, fetch: c.groupFetcher(id)})
	}
	for _, r := range activeRooms(rooms) {
		id, err := r.ID()
		if err != nil {
			return nil, err
		}
		sources = append(sources, source{portalID: makeRoomPortalID(id), name: r.Title, fetch: c.roomFetcher(id)})
	}
	c.lock.Lock()
	c.sources = make(map[networkid.PortalID]source, len(sources))
	for _, s := range sources {
		c.sources[s.portalID] = s
	}
	c.lock.Unlock()
	return sources, nil
}

// currentGroups keeps the groups of the newest school year.
func currentGroups(groups []parro.Group) []parro.Group {
	newest := 0
	for _, g := range groups {
		newest = max(newest, g.SchoolYear)
	}
	var out []parro.Group
	for _, g := range groups {
		if g.SchoolYear == newest {
			out = append(out, g)
		}
	}
	return out
}

func activeRooms(rooms []parro.ChatRoom) []parro.ChatRoom {
	var out []parro.ChatRoom
	for _, r := range rooms {
		if r.Active && !r.Archived {
			out = append(out, r)
		}
	}
	return out
}

func (c *Client) groupFetcher(groupID int64) func(context.Context, func([]item) bool) ([]item, error) {
	return func(ctx context.Context, stop func([]item) bool) ([]item, error) {
		found, err := c.API.GetAnnouncements(ctx, groupID, func(page []parro.Announcement) bool {
			return stop != nil && stop(itemsFromAnnouncements(page))
		})
		if err != nil {
			return nil, err
		}
		return itemsFromAnnouncements(found), nil
	}
}

func (c *Client) roomFetcher(roomID int64) func(context.Context, func([]item) bool) ([]item, error) {
	return func(ctx context.Context, stop func([]item) bool) ([]item, error) {
		found, err := c.API.GetChatMessages(ctx, roomID, func(page []parro.ChatMessage) bool {
			return stop != nil && stop(itemsFromMessages(page))
		})
		if err != nil {
			return nil, err
		}
		return itemsFromMessages(found), nil
	}
}

func makeGroupPortalID(id int64) networkid.PortalID {
	return networkid.PortalID("group:" + strconv.FormatInt(id, 10))
}

func makeRoomPortalID(id int64) networkid.PortalID {
	return networkid.PortalID("room:" + strconv.FormatInt(id, 10))
}

func (c *Client) portalKey(id networkid.PortalID) networkid.PortalKey {
	return networkid.PortalKey{ID: id, Receiver: c.UserLogin.ID}
}

func historyFloor() time.Time { return time.Now().AddDate(0, 0, -historyDays) }

func olderThan(floor time.Time) func([]item) bool {
	return func(page []item) bool { return len(page) > 0 && page[0].timestamp.Before(floor) }
}

func (c *Client) syncSource(ctx context.Context, src source) error {
	key := c.portalKey(src.portalID)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, key)
	if err != nil {
		return err
	}
	if portal == nil || portal.MXID == "" {
		return c.createPortal(ctx, src)
	}
	c.lock.Lock()
	needsResync := !c.resynced[src.portalID]
	c.lock.Unlock()
	if needsResync {
		c.queueInfoResync(src)
	}
	floor := historyFloor()
	first, err := c.Main.Bridge.DB.Message.GetFirstPortalMessage(ctx, key)
	if err != nil {
		return err
	}
	if first != nil && first.Timestamp.Before(floor) {
		floor = first.Timestamp
	}
	lookup := c.lookupFunc(ctx)
	items, err := src.fetch(ctx, func(page []item) bool {
		for _, it := range page {
			if lookup(it.id) != nil {
				return true
			}
		}
		return olderThan(floor)(page)
	})
	if err != nil {
		return err
	}
	c.rememberPeople(items)
	for _, act := range planSync(items, lookup, floor) {
		c.queueAction(key, act)
	}
	return nil
}

func (c *Client) lookupFunc(ctx context.Context) func(networkid.MessageID) *knownMessage {
	return func(id networkid.MessageID) *knownMessage {
		msg, err := c.Main.Bridge.DB.Message.GetFirstPartByID(ctx, c.UserLogin.ID, id)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to look up message")
			return &knownMessage{}
		}
		if msg == nil {
			return nil
		}
		known := &knownMessage{}
		if meta, ok := msg.Metadata.(*MessageMetadata); ok {
			known.contentHash = meta.ContentHash
		}
		return known
	}
}

func (c *Client) createPortal(ctx context.Context, src source) error {
	floor := historyFloor()
	items, err := src.fetch(ctx, olderThan(floor))
	if err != nil {
		return err
	}
	c.rememberPeople(items)
	var latest time.Time
	for _, it := range items {
		if !it.timestamp.Before(floor) && it.timestamp.After(latest) {
			latest = it.timestamp
		}
	}
	c.lock.Lock()
	if c.resynced == nil {
		c.resynced = map[networkid.PortalID]bool{}
	}
	c.resynced[src.portalID] = true
	c.lock.Unlock()
	c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
		EventMeta:       simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: c.portalKey(src.portalID), CreatePortal: true},
		ChatInfo:        c.chatInfo(src.name),
		LatestMessageTS: latest,
	})
	return nil
}

// queueInfoResync refreshes the name and avatar of an existing chat once per run, without history.
func (c *Client) queueInfoResync(src source) {
	c.lock.Lock()
	if c.resynced == nil {
		c.resynced = map[networkid.PortalID]bool{}
	}
	c.resynced[src.portalID] = true
	c.lock.Unlock()
	c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: c.portalKey(src.portalID)},
		ChatInfo:  c.chatInfo(src.name),
		CheckNeedsBackfillFunc: func(context.Context, *database.Message) (bool, error) {
			return false, nil
		},
	})
}

func (c *Client) queueAction(key networkid.PortalKey, act action) {
	it := act.item
	meta := simplevent.EventMeta{PortalKey: key, Sender: c.senderFor(it.sender), Timestamp: it.timestamp}
	switch act.kind {
	case actionSend:
		meta.Type = bridgev2.RemoteEventMessage
		c.UserLogin.QueueRemoteEvent(&simplevent.Message[item]{
			EventMeta: meta,
			ID:        it.id,
			Data:      it,
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data item) (*bridgev2.ConvertedMessage, error) {
				return convertItem(ctx, intent, c.API.Download, portal.MXID, data), nil
			},
		})
	case actionEdit:
		meta.Type = bridgev2.RemoteEventEdit
		c.UserLogin.QueueRemoteEvent(&simplevent.Message[item]{
			EventMeta:     meta,
			Data:          it,
			TargetMessage: it.id,
			ConvertEditFunc: func(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, existing []*database.Message, data item) (*bridgev2.ConvertedEdit, error) {
				return convertEdit(data, existing), nil
			},
		})
	case actionRemove:
		meta.Type = bridgev2.RemoteEventMessageRemove
		c.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{EventMeta: meta, TargetMessage: it.id})
	}
}

// FetchMessages delivers the initial history of a new chat. Later messages come from the poller.
func (c *Client) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	if !params.Forward || params.AnchorMessage != nil {
		return &bridgev2.FetchMessagesResponse{Forward: params.Forward}, nil
	}
	c.lock.Lock()
	src, ok := c.sources[params.Portal.ID]
	c.lock.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown parro source %s", params.Portal.ID)
	}
	floor := historyFloor()
	items, err := src.fetch(ctx, olderThan(floor))
	if err != nil {
		return nil, err
	}
	c.rememberPeople(items)
	resp := &bridgev2.FetchMessagesResponse{Forward: true, MarkRead: true, AggressiveDeduplication: true}
	for _, it := range items {
		if it.deleted || it.timestamp.Before(floor) {
			continue
		}
		resp.Messages = append(resp.Messages, &bridgev2.BackfillMessage{
			ConvertedMessage: convertItem(ctx, c.Main.Bridge.Bot, c.API.Download, params.Portal.MXID, it),
			Sender:           c.senderFor(it.sender),
			ID:               it.id,
			Timestamp:        it.timestamp,
		})
	}
	return resp, nil
}

func (c *Client) rememberPeople(items []item) {
	c.lock.Lock()
	defer c.lock.Unlock()
	for _, it := range items {
		if id, err := it.sender.ID(); err == nil {
			c.people[id] = it.sender
		}
	}
}

func (c *Client) senderFor(identity parro.Identity) bridgev2.EventSender {
	id, err := identity.ID()
	if err != nil {
		return bridgev2.EventSender{}
	}
	c.lock.Lock()
	own := c.ownIDs[id]
	c.lock.Unlock()
	if own {
		return bridgev2.EventSender{IsFromMe: true, SenderLogin: c.UserLogin.ID, Sender: networkid.UserID(id)}
	}
	return bridgev2.EventSender{Sender: networkid.UserID(id)}
}

func (c *Client) chatInfo(name string) *bridgev2.ChatInfo {
	return &bridgev2.ChatInfo{
		Name:        ptr.Ptr(name),
		Avatar:      logoAvatar(),
		Type:        ptr.Ptr(database.RoomTypeGroupDM),
		CanBackfill: true,
		Members: &bridgev2.ChatMemberList{
			MemberMap: bridgev2.ChatMemberMap{
				networkid.UserID(c.UserLogin.ID): {EventSender: bridgev2.EventSender{
					IsFromMe: true, SenderLogin: c.UserLogin.ID, Sender: networkid.UserID(c.UserLogin.ID),
				}},
			},
		},
		ExcludeChangesFromTimeline: true,
	}
}

// Disconnect stops polling.
func (c *Client) Disconnect() {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
}

// IsLoggedIn reports whether tokens are stored.
func (c *Client) IsLoggedIn() bool { return c.API.HasTokens() }

// LogoutRemote stops polling. There is nothing to revoke: dropping the login row removes the tokens.
func (c *Client) LogoutRemote(context.Context) { c.Disconnect() }

// GetUserID returns the Parro identity of this login.
func (c *Client) GetUserID() networkid.UserID { return networkid.UserID(c.UserLogin.ID) }

// IsThisUser reports whether a Parro identity is this login.
func (c *Client) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.ownIDs[string(userID)] || string(userID) == string(c.UserLogin.ID)
}

// GetChatInfo returns the conversation info from the last poll.
func (c *Client) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	c.lock.Lock()
	src, ok := c.sources[portal.ID]
	c.lock.Unlock()
	if !ok {
		return &bridgev2.ChatInfo{CanBackfill: true}, nil
	}
	return c.chatInfo(src.name), nil
}

// GetUserInfo returns the name and photo seen on this person's messages.
func (c *Client) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	c.lock.Lock()
	person, ok := c.people[string(ghost.ID)]
	c.lock.Unlock()
	if !ok {
		return &bridgev2.UserInfo{}, nil
	}
	info := &bridgev2.UserInfo{Name: ptr.Ptr(person.Name())}
	if strings.HasPrefix(person.AvatarURL, "http") {
		avatarURL := person.AvatarURL
		info.Avatar = &bridgev2.Avatar{
			ID:  avatarID(avatarURL),
			Get: func(ctx context.Context) ([]byte, error) { return c.API.Download(ctx, avatarURL) },
		}
	}
	return info, nil
}

// avatarID drops the query, so a presigned URL that changes on every poll does not re-upload the same photo.
func avatarID(avatarURL string) networkid.AvatarID {
	if u, err := url.Parse(avatarURL); err == nil {
		u.RawQuery, u.Fragment = "", ""
		return networkid.AvatarID(u.String())
	}
	return networkid.AvatarID(avatarURL)
}

// GetCapabilities reports that nothing can be sent.
func (c *Client) GetCapabilities(context.Context, *bridgev2.Portal) *event.RoomFeatures {
	return roomCaps
}

// HandleMatrixMessage refuses every message: this bridge only reads.
func (c *Client) HandleMatrixMessage(context.Context, *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, ErrReadOnly
}
