# Design

One Go program on [mautrix bridgev2](https://github.com/mautrix/go/tree/main/bridgev2), connected to Beeper over the appservice websocket that `bbctl` sets up. One user, one launchd agent, one SQLite file.

## Layout

| Path | What |
|---|---|
| `cmd/parro-bridge` | Entry point, refuses to start without encryption |
| `pkg/parro` | API client and login. GET only towards the API, enforced by the HTTP transport |
| `pkg/connector` | bridgev2 connector: login, discovery, polling, message conversion |
| `scripts/watchdog.sh` | Runs separately every 5 minutes and alerts when the bridge stops working |
| `install.sh` | Builds, tests, then swaps the running agents |

## Parro API

The API is undocumented. [gwillem/parro `doc/api.md`](https://github.com/gwillem/parro/blob/main/doc/api.md) describes most of it. Additions seen in practice:

1. List endpoints answer `206 Partial Content`, also when the list fits one page. Paging uses `Range: items=0-99`, newest first.
2. Announcements use `dtype=event.RAnnouncementEvent` with `attachments[]`. Chat messages use `attachment` (singular).
3. An attachment has `attachmentType` (`PDF`, `IMAGE`, `VIDEO`) and `entries[]` with `type` (`SOURCE`, `THUMBNAIL`, `VIDEO`), `url`, `contentType` and `size`.
4. Chat messages are `chat.RChatTextMessage` or `chat.RChatStatusMessage`. Status messages are skipped.
5. Attachments and profile photos live on media hosts (`*.cloudfront.net`) and, as of October 2026, load without any login for whoever has the URL. We reported this to Topicus on 2026-10-31; they replied they are moving to presigned URLs for all media. The bridge sends no token to these hosts and is ready for signed URLs: profile photos are recognised without the signature, so they are not uploaded again on every poll.
6. Refresh tokens rotate. Each one is saved before its access token is used, so a crash cannot lose the login.

## Polling

Every 15 minutes with up to a minute of jitter, checked against the clock each minute so a waking Mac catches up right away. One quiet cycle costs `/account/me`, `/group`, `/chatroom` and one list request per conversation. After failures the wait doubles up to 2 hours, and `Retry-After` is respected.

Conversations are discovered on every cycle. Only groups of the newest school year and active, non-archived rooms get a chat.

## Messages

The Parro self-link ID is the message ID, which is what prevents duplicates across restarts. New chats get 30 days of history through a bridgev2 forward backfill, marked as read. After that, the poller sends normal messages.

Announcement edits are detected by a hash of title and text, because `lastModifiedAt` is not reliable for that. Edits and deletes are noticed in the newest 100 items of a conversation.

A failed attachment becomes a text line, and the rest of the message still comes through.

## Known limits

1. `TestWholeChain` runs the real bridgev2 core with this connector against a fake Parro and a fake Matrix server. Real Parro, Beeper, encryption and the service manager are not part of it.
2. macOS only for installer and watchdog.
3. Refresh token lifetime and Parro's rate limits are unknown.
