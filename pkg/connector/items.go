package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

const textMessageType = "chat.RChatTextMessage"

// MessageMetadata detects real edits. Parro's lastModifiedAt also moves without a content change.
type MessageMetadata struct {
	ContentHash string `json:"content_hash,omitempty"`
}

// item is one announcement or chat message, in the shape the bridge needs.
type item struct {
	id          networkid.MessageID
	sender      parro.Identity
	timestamp   time.Time
	deleted     bool
	title       string
	text        string
	attachments []parro.Attachment
}

func (i item) contentHash() string {
	sum := sha256.Sum256([]byte(i.title + "\x00" + i.text))
	return hex.EncodeToString(sum[:])
}

func (i item) hasText() bool { return i.title != "" || i.text != "" }

// itemsFromMessages keeps text messages, oldest first. Status messages like "ENTER" are skipped.
func itemsFromMessages(messages []parro.ChatMessage) []item {
	items := make([]item, 0, len(messages))
	for _, m := range messages {
		id, err := m.ID()
		if err != nil || m.DType != textMessageType {
			continue
		}
		it := item{id: makeMessageID("m", id), sender: m.Identity, timestamp: m.CreatedAt.Time, deleted: m.Deleted, text: m.Text}
		if m.Attachment != nil {
			it.attachments = []parro.Attachment{*m.Attachment}
		}
		items = append(items, it)
	}
	slices.Reverse(items)
	return items
}

// itemsFromAnnouncements keeps published announcements, oldest first.
func itemsFromAnnouncements(announcements []parro.Announcement) []item {
	items := make([]item, 0, len(announcements))
	for _, a := range announcements {
		id, err := a.ID()
		if err != nil || a.Draft {
			continue
		}
		items = append(items, item{
			id: makeMessageID("a", id), sender: a.Owner, timestamp: a.SortDate.Time, deleted: a.Deleted,
			title: a.Title, text: a.Contents, attachments: a.Attachments,
		})
	}
	slices.Reverse(items)
	return items
}

func makeMessageID(prefix string, id int64) networkid.MessageID {
	return networkid.MessageID(prefix + ":" + strconv.FormatInt(id, 10))
}

type actionKind int

const (
	actionSend actionKind = iota
	actionEdit
	actionRemove
)

type action struct {
	kind actionKind
	item item
}

// knownMessage is what the bridge database holds for a message.
type knownMessage struct {
	contentHash string
}

// planSync decides what one poll does, from oldest to newest. Unknown messages older than floor
// are history outside the window and stay out.
func planSync(items []item, lookup func(networkid.MessageID) *knownMessage, floor time.Time) []action {
	var actions []action
	for _, it := range items {
		known := lookup(it.id)
		switch {
		case known == nil:
			if !it.deleted && !it.timestamp.Before(floor) {
				actions = append(actions, action{actionSend, it})
			}
		case it.deleted:
			actions = append(actions, action{actionRemove, it})
		case known.contentHash != "" && known.contentHash != it.contentHash():
			actions = append(actions, action{actionEdit, it})
		}
	}
	return actions
}

// uploader is the part of the Matrix API that attachments need.
type uploader interface {
	UploadMedia(ctx context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error)
}

type downloader func(ctx context.Context, url string) ([]byte, error)

func textContent(it item) *event.MessageEventContent {
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: it.text}
	if it.title != "" {
		content.Body = strings.TrimRight(it.title+"\n\n"+it.text, "\n")
		content.Format = event.FormatHTML
		content.FormattedBody = "<strong>" + html.EscapeString(it.title) + "</strong>"
		if it.text != "" {
			content.FormattedBody += "<br><br>" + strings.ReplaceAll(html.EscapeString(it.text), "\n", "<br>")
		}
	}
	return content
}

// convertItem builds the message parts. A failed attachment becomes a text line, the rest still gets through.
func convertItem(ctx context.Context, up uploader, get downloader, room id.RoomID, it item) *bridgev2.ConvertedMessage {
	var parts []*bridgev2.ConvertedMessagePart
	meta := &MessageMetadata{ContentHash: it.contentHash()}
	if it.hasText() {
		parts = append(parts, &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: textContent(it), DBMetadata: meta})
	}
	for n, attachment := range it.attachments {
		content := convertAttachment(ctx, up, get, room, attachment)
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID: networkid.PartID(fmt.Sprintf("attachment-%d", n)), Type: event.EventMessage, Content: content, DBMetadata: meta,
		})
	}
	if len(parts) == 0 {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Leeg Parro-bericht"}, DBMetadata: meta,
		})
	}
	return &bridgev2.ConvertedMessage{Parts: parts}
}

func convertAttachment(ctx context.Context, up uploader, get downloader, room id.RoomID, a parro.Attachment) *event.MessageEventContent {
	entry := a.Original()
	if entry == nil {
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Bijlage niet beschikbaar in Parro"}
	}
	name := attachmentName(entry)
	failed := func(err error) *event.MessageEventContent {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Attachment not bridged")
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Bijlage niet opgehaald: " + name}
	}
	data, err := get(ctx, entry.URL)
	if err != nil {
		return failed(err)
	}
	mxc, file, err := up.UploadMedia(ctx, room, data, name, entry.ContentType)
	if err != nil {
		return failed(err)
	}
	content := &event.MessageEventContent{
		MsgType:  attachmentMsgType(a.AttachmentType),
		Body:     name,
		FileName: name,
		Info:     &event.FileInfo{MimeType: entry.ContentType, Size: len(data), Width: entry.Width, Height: entry.Height},
	}
	if file != nil {
		content.File = file
	} else {
		content.URL = mxc
	}
	return content
}

func attachmentMsgType(attachmentType string) event.MessageType {
	switch attachmentType {
	case "IMAGE":
		return event.MsgImage
	case "VIDEO":
		return event.MsgVideo
	default:
		return event.MsgFile
	}
}

func attachmentName(entry *parro.AttachmentEntry) string {
	if entry.Filename != "" {
		return entry.Filename
	}
	if u, err := url.Parse(entry.URL); err == nil && path.Base(u.Path) != "." && path.Base(u.Path) != "/" {
		return path.Base(u.Path)
	}
	return "bijlage"
}

func convertEdit(it item, existing []*database.Message) *bridgev2.ConvertedEdit {
	if len(existing) == 0 {
		return &bridgev2.ConvertedEdit{}
	}
	existing[0].Metadata = &MessageMetadata{ContentHash: it.contentHash()}
	return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{{
		Part: existing[0], Type: event.EventMessage, Content: textContent(it),
	}}}
}
