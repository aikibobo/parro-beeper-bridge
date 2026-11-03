package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

func decode[T any](t *testing.T, raw string) []T {
	t.Helper()
	var out []T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// protects=status messages are skipped and order is chronological; fails_when=ENTER becomes a message or newest
// comes first; no_claim=none beyond protects, the JSON is synthetic in the shape seen live on 2026-10-30.
func TestItemsFromMessages(t *testing.T) {
	items := itemsFromMessages(decode[parro.ChatMessage](t, `[
		{"links":[{"id":3,"rel":"self"}],"dtype":"chat.RChatTextMessage","text":"nieuw","createdAt":"2026-10-30T09:00:00.000+01:00"},
		{"links":[{"id":2,"rel":"self"}],"dtype":"chat.RChatStatusMessage","status":"ENTER","createdAt":"2026-10-29T09:00:00.000+01:00"},
		{"links":[{"id":1,"rel":"self"}],"dtype":"chat.RChatTextMessage","attachment":{"attachmentType":"PDF","entries":[]},"createdAt":"2026-10-28T09:00:00.000+01:00"}
	]`))
	if len(items) != 2 || items[0].id != "m:1" || items[1].id != "m:3" {
		t.Fatalf("got %+v", items)
	}
	if len(items[0].attachments) != 1 {
		t.Fatalf("chat attachment lost: %+v", items[0])
	}
}

// protects=drafts never reach Beeper; fails_when=a draft is kept; no_claim=whether guardians ever receive drafts.
func TestItemsFromAnnouncementsSkipsDrafts(t *testing.T) {
	items := itemsFromAnnouncements(decode[parro.Announcement](t, `[
		{"links":[{"id":2,"rel":"self"}],"title":"t","draft":true,"sortDate":"2026-10-30T09:00:00.000+01:00"},
		{"links":[{"id":1,"rel":"self"}],"title":"t","contents":"c","sortDate":"2026-10-29T09:00:00.000+01:00"}
	]`))
	if len(items) != 1 || items[0].id != "a:1" {
		t.Fatalf("got %+v", items)
	}
}

// protects=no duplicates, deletes and real edits per poll; fails_when=a known message is resent, history outside the
// window is sent, or an unchanged message counts as edited; no_claim=the bridge database, lookup is a stub.
func TestPlanSync(t *testing.T) {
	floor := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old, recent := floor.Add(-time.Hour), floor.Add(time.Hour)
	unchanged := item{id: "a:1", timestamp: recent, title: "t"}
	edited := item{id: "a:2", timestamp: recent, title: "nieuw"}
	removed := item{id: "m:3", timestamp: recent, deleted: true}
	fresh := item{id: "m:4", timestamp: recent, text: "hallo"}
	tooOld := item{id: "m:5", timestamp: old, text: "oud"}
	deletedUnseen := item{id: "m:6", timestamp: recent, deleted: true}
	known := map[networkid.MessageID]*knownMessage{
		"a:1": {contentHash: unchanged.contentHash()},
		"a:2": {contentHash: "other"},
		"m:3": {},
	}
	got := planSync([]item{unchanged, edited, removed, fresh, tooOld, deletedUnseen}, func(id networkid.MessageID) *knownMessage { return known[id] }, floor)
	want := map[networkid.MessageID]actionKind{"a:2": actionEdit, "m:3": actionRemove, "m:4": actionSend}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, a := range got {
		if kind, ok := want[a.item.id]; !ok || kind != a.kind {
			t.Fatalf("unexpected %+v", a)
		}
	}
}

type fakeUploader struct{ err error }

func (f fakeUploader) UploadMedia(context.Context, id.RoomID, []byte, string, string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	return "mxc://test/file", nil, f.err
}

func okDownload(context.Context, string) ([]byte, error) { return []byte("data"), nil }

// protects=attachments arrive with the original file and the right type; fails_when=the thumbnail is used or a PDF
// becomes text; no_claim=rendering in Beeper, the upload is a fake.
func TestConvertAttachmentUsesOriginal(t *testing.T) {
	it := item{text: "kijk", attachments: []parro.Attachment{{AttachmentType: "PDF", Entries: []parro.AttachmentEntry{
		{Type: "THUMBNAIL", URL: "https://cdn/thumb.jpg", ContentType: "image/jpeg"},
		{Type: "SOURCE", URL: "https://cdn/1-abc.pdf", ContentType: "application/pdf", Filename: "KMSPoster.pdf"},
	}}}}
	parts := convertItem(context.Background(), fakeUploader{}, okDownload, "!r", it).Parts
	if len(parts) != 2 {
		t.Fatalf("parts=%d", len(parts))
	}
	c := parts[1].Content
	if c.MsgType != event.MsgFile || c.FileName != "KMSPoster.pdf" || c.Info.MimeType != "application/pdf" || c.URL == "" {
		t.Fatalf("content=%+v", c)
	}
}

// protects=a video without SOURCE still arrives; fails_when=VIDEO is ignored; no_claim=none beyond protects.
func TestOriginalFallsBackToVideo(t *testing.T) {
	a := parro.Attachment{Entries: []parro.AttachmentEntry{{Type: "THUMBNAIL"}, {Type: "VIDEO", URL: "v"}}}
	if e := a.Original(); e == nil || e.URL != "v" {
		t.Fatalf("got %+v", e)
	}
}

// protects=a failed attachment does not lose the message; fails_when=the text disappears or nothing says the file is
// missing; no_claim=none beyond protects.
func TestFailedAttachmentBecomesNotice(t *testing.T) {
	fail := func(context.Context, string) ([]byte, error) { return nil, errors.New("gone") }
	it := item{text: "kijk", attachments: []parro.Attachment{{AttachmentType: "IMAGE", Entries: []parro.AttachmentEntry{{Type: "SOURCE", URL: "https://cdn/a.jpg"}}}}}
	parts := convertItem(context.Background(), fakeUploader{}, fail, "!r", it).Parts
	if len(parts) != 2 || parts[0].Content.Body != "kijk" || !strings.Contains(parts[1].Content.Body, "a.jpg") {
		t.Fatalf("parts=%+v", parts)
	}
}

// protects=an attachment-only message is not an empty bubble; fails_when=an empty text part is sent;
// no_claim=none beyond protects.
func TestAttachmentOnlyHasNoTextPart(t *testing.T) {
	it := item{attachments: []parro.Attachment{{AttachmentType: "IMAGE", Entries: []parro.AttachmentEntry{{Type: "SOURCE", URL: "https://cdn/a.jpg"}}}}}
	parts := convertItem(context.Background(), fakeUploader{}, okDownload, "!r", it).Parts
	if len(parts) != 1 || parts[0].Content.MsgType != event.MsgImage {
		t.Fatalf("parts=%+v", parts)
	}
}

// protects=announcement titles show bold and markup in a title is harmless; fails_when=HTML is injected or the title
// is lost; no_claim=rendering in Beeper.
func TestTextContentEscapesTitle(t *testing.T) {
	c := textContent(item{title: "A<b>", text: "regel 1\nregel 2"})
	if !strings.Contains(c.FormattedBody, "<strong>A&lt;b&gt;</strong>") || !strings.Contains(c.FormattedBody, "regel 1<br>regel 2") {
		t.Fatalf("html=%s", c.FormattedBody)
	}
	if c.Body != "A<b>\n\nregel 1\nregel 2" {
		t.Fatalf("body=%q", c.Body)
	}
}
