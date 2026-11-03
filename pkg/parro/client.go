package parro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var restBase = "https://rest-v2.parro.com/rest/v2"

const pageSize = 100

var (
	maxDownloadBytes int64 = 100 << 20
	// downloadHostSuffixes are the hosts the Parro API was seen to serve attachments and avatars from.
	downloadHostSuffixes = []string{".cloudfront.net", ".parnassys.net", ".parro.com"}
	allowInsecureHosts   = false
)

// ErrAPIChanged means Parro answered with something this client cannot read, so the bridge needs an update.
var ErrAPIChanged = errors.New("the parro api answered in an unexpected format")

// HTTPError is an unexpected status. RetryAfter is what Parro asked for, or zero.
type HTTPError struct {
	Path       string
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.Path, e.Status) }

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// withoutURL drops the URL from a net/http error, because attachment URLs must not end up in logs.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func downloadAllowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("download: malformed url")
	}
	if allowInsecureHosts {
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("download: refused a non-https url")
	}
	for _, suffix := range downloadHostSuffixes {
		if strings.HasSuffix(u.Hostname(), suffix) {
			return nil
		}
	}
	return errors.New("download: refused an unknown host")
}

// readOnlyTransport refuses everything but GET, so this client cannot change anything in Parro.
type readOnlyTransport struct{ next http.RoundTripper }

func (t readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("parro client is read-only, refused %s", req.Method)
	}
	return t.next.RoundTrip(req)
}

// Client talks to the REST API. Rotated tokens go through OnTokens before they are used.
type Client struct {
	OnTokens func(Tokens) error

	http     *http.Client
	download *http.Client
	lock     sync.Mutex
	tokens   Tokens
}

// NewClient returns a client for a stored token set.
func NewClient(tokens Tokens) *Client {
	return &Client{
		tokens: tokens,
		http:   &http.Client{Timeout: 30 * time.Second, Transport: readOnlyTransport{http.DefaultTransport}},
		download: &http.Client{
			Timeout:   5 * time.Minute,
			Transport: readOnlyTransport{http.DefaultTransport},
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return downloadAllowed(req.URL.String())
			},
		},
	}
}

// HasTokens reports whether a refresh token is stored.
func (c *Client) HasTokens() bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.tokens.RefreshToken != ""
}

func (c *Client) refresh(ctx context.Context, failed string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.tokens.AccessToken != failed {
		return nil // another caller already refreshed
	}
	tokens, err := Refresh(ctx, c.tokens.RefreshToken)
	if err != nil {
		return err
	}
	if c.OnTokens != nil {
		if err := c.OnTokens(*tokens); err != nil {
			return fmt.Errorf("persist rotated tokens: %w", err)
		}
	}
	c.tokens = *tokens
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	_, err := c.fetch(ctx, path, "", out)
	return err
}

// fetch runs one GET. It reports ended=true when a Range request is past the end of the list.
func (c *Client) fetch(ctx context.Context, path, itemRange string, out any) (ended bool, err error) {
	for attempt := 0; ; attempt++ {
		c.lock.Lock()
		access := c.tokens.AccessToken
		c.lock.Unlock()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, restBase+path, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Accept", "application/json")
		if itemRange != "" {
			req.Header.Set("Range", itemRange)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return false, fmt.Errorf("GET %s: %w", path, withoutURL(err))
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			if err := c.refresh(ctx, access); err != nil {
				return false, err
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return true, nil
		}
		// Parro answers list endpoints with 206 Partial Content, also when the list fits one page.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			return false, &HTTPError{Path: path, Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return false, fmt.Errorf("GET %s: %w: %v", path, ErrAPIChanged, err)
		}
		return false, nil
	}
}

// fetchPaged walks a newest-first list until stop says enough, or the list ends.
func fetchPaged[T any](ctx context.Context, c *Client, path string, stop func(page []T) bool) ([]T, error) {
	var all []T
	for {
		var page list[T]
		ended, err := c.fetch(ctx, path, fmt.Sprintf("items=%d-%d", len(all), len(all)+pageSize-1), &page)
		if err != nil {
			return nil, err
		}
		if ended || len(page.Items) == 0 {
			return all, nil
		}
		all = append(all, page.Items...)
		if len(page.Items) < pageSize || (stop != nil && stop(page.Items)) {
			return all, nil
		}
	}
}

// Link is a resource link. The self link carries the resource ID.
type Link struct {
	ID  int64  `json:"id"`
	Rel string `json:"rel"`
}

func selfID(links []Link) (int64, error) {
	for _, l := range links {
		if l.Rel == "self" {
			return l.ID, nil
		}
	}
	return 0, errors.New("resource without self link")
}

// Identity is a person in Parro.
type Identity struct {
	Links     []Link `json:"links"`
	Firstname string `json:"firstname"`
	Surname   string `json:"surname"`
	Role      string `json:"role"`
	AvatarURL string `json:"cachedAvatarUrl"`
}

// ID returns the identity's self-link ID as a string.
func (i Identity) ID() (string, error) {
	id, err := selfID(i.Links)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// Name returns the display name.
func (i Identity) Name() string {
	if i.Surname == "" {
		return i.Firstname
	}
	return i.Firstname + " " + i.Surname
}

// Account is the logged-in user.
type Account struct {
	Identity struct {
		Identity
		Guardians []Identity `json:"guardians"`
	} `json:"identity"`
}

// OwnIDs returns every identity ID that means "me": the account identity and its guardian records.
func (a *Account) OwnIDs() (map[string]bool, error) {
	own := map[string]bool{}
	id, err := a.Identity.ID()
	if err != nil {
		return nil, err
	}
	own[id] = true
	for _, g := range a.Identity.Guardians {
		if id, err := g.ID(); err == nil {
			own[id] = true
		}
	}
	return own, nil
}

// ChatRoom is a chat in the Chat tab.
type ChatRoom struct {
	Links    []Link `json:"links"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	Active   bool   `json:"active"`
	Archived bool   `json:"archived"`
}

// ID returns the room's self-link ID.
func (r ChatRoom) ID() (int64, error) { return selfID(r.Links) }

// Time is a Parro timestamp.
type Time struct{ time.Time }

// UnmarshalJSON parses Parro's ISO 8601 timestamps.
func (t *Time) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// AttachmentEntry is one rendition of an attachment.
type AttachmentEntry struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Filename    string `json:"filename"`
}

// Attachment is a file on an announcement or chat message.
type Attachment struct {
	AttachmentType string            `json:"attachmentType"`
	Entries        []AttachmentEntry `json:"entries"`
}

// Original returns the full-size entry: SOURCE, or VIDEO when there is no SOURCE.
func (a Attachment) Original() *AttachmentEntry {
	var video *AttachmentEntry
	for i := range a.Entries {
		switch a.Entries[i].Type {
		case "SOURCE":
			return &a.Entries[i]
		case "VIDEO":
			video = &a.Entries[i]
		}
	}
	return video
}

// ChatMessage is one message in a room. Status messages have a Status and no Text.
type ChatMessage struct {
	Links      []Link      `json:"links"`
	DType      string      `json:"dtype"`
	Identity   Identity    `json:"identity"`
	Text       string      `json:"text"`
	Status     string      `json:"status"`
	CreatedAt  Time        `json:"createdAt"`
	Deleted    bool        `json:"deleted"`
	Attachment *Attachment `json:"attachment"`
}

// ID returns the message's self-link ID.
func (m ChatMessage) ID() (int64, error) { return selfID(m.Links) }

// Group is a school or class in the Home tab.
type Group struct {
	Links      []Link `json:"links"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	SchoolYear int    `json:"schooljaar"`
}

// ID returns the group's self-link ID.
func (g Group) ID() (int64, error) { return selfID(g.Links) }

// Announcement is a one-way post in a group.
type Announcement struct {
	Links       []Link       `json:"links"`
	Title       string       `json:"title"`
	Contents    string       `json:"contents"`
	SortDate    Time         `json:"sortDate"`
	Owner       Identity     `json:"owner"`
	Deleted     bool         `json:"deleted"`
	Draft       bool         `json:"draft"`
	Attachments []Attachment `json:"attachments"`
}

// ID returns the announcement's self-link ID.
func (a Announcement) ID() (int64, error) { return selfID(a.Links) }

type list[T any] struct {
	Items []T `json:"items"`
}

// GetAccount fetches the logged-in account.
func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.get(ctx, "/account/me", &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetChatRooms fetches the chat rooms.
func (c *Client) GetChatRooms(ctx context.Context) ([]ChatRoom, error) {
	var l list[ChatRoom]
	if err := c.get(ctx, "/chatroom", &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// GetGroups fetches the school and class groups.
func (c *Client) GetGroups(ctx context.Context) ([]Group, error) {
	var l list[Group]
	if err := c.get(ctx, "/group?dtype=identity.RHomeGroup", &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// GetChatMessages fetches a room's messages, newest first, until stop says enough.
func (c *Client) GetChatMessages(ctx context.Context, roomID int64, stop func([]ChatMessage) bool) ([]ChatMessage, error) {
	return fetchPaged(ctx, c, fmt.Sprintf("/chatroom/%d/chatmessage", roomID), stop)
}

// GetAnnouncements fetches a group's announcements, newest first, until stop says enough.
func (c *Client) GetAnnouncements(ctx context.Context, groupID int64, stop func([]Announcement) bool) ([]Announcement, error) {
	return fetchPaged(ctx, c, fmt.Sprintf("/event?dtype=event.RAnnouncementEvent&group=%d", groupID), stop)
}

// Download fetches an attachment or avatar. The Parro token never goes to these media hosts.
func (c *Client) Download(ctx context.Context, target string) ([]byte, error) {
	if err := downloadAllowed(target); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("download: malformed url")
	}
	resp, err := c.download.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download: %w", withoutURL(err))
	}
	if int64(len(data)) > maxDownloadBytes {
		return nil, fmt.Errorf("download: larger than %d bytes", maxDownloadBytes)
	}
	return data, nil
}
