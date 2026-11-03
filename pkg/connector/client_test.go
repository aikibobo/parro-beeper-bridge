package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/event"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

// protects=a message typed in Beeper fails visibly with the read-only text; fails_when=it returns success, another
// error or a retriable status; no_claim=how Beeper renders the status, checked by hand in AC 6.
func TestSendIsRefused(t *testing.T) {
	_, err := (&Client{}).HandleMatrixMessage(context.Background(), nil)
	var ms bridgev2.MessageStatus
	if !errors.As(err, &ms) {
		t.Fatalf("got %v", err)
	}
	if ms.Error() != "Parro bridge is read-only, stuur dit via de Parro-app." || !ms.ErrorAsMessage || !ms.IsCertain {
		t.Fatalf("status=%+v", ms)
	}
	if ms.Status != event.MessageStatusFail {
		t.Fatalf("retriable status would make Beeper keep trying: %s", ms.Status)
	}
}

// protects=last year's groups do not become chats; fails_when=a group of an older school year is kept;
// no_claim=what Parro returns around the school-year change, which has not been seen live.
func TestCurrentGroupsKeepsNewestYear(t *testing.T) {
	got := currentGroups([]parro.Group{{Name: "old", SchoolYear: 2025}, {Name: "school", SchoolYear: 2026}, {Name: "class", SchoolYear: 2026}})
	if len(got) != 2 || got[0].Name != "school" || got[1].Name != "class" {
		t.Fatalf("got %+v", got)
	}
}

// protects=archived and inactive chats stay out; fails_when=one of them is kept; no_claim=none beyond protects.
func TestActiveRooms(t *testing.T) {
	got := activeRooms([]parro.ChatRoom{{Title: "a", Active: true}, {Title: "b", Active: true, Archived: true}, {Title: "c"}})
	if len(got) != 1 || got[0].Title != "a" {
		t.Fatalf("got %+v", got)
	}
}

// protects=the watchdog sees a fresh timestamp after a good poll; fails_when=the file keeps its old mtime;
// no_claim=that pollOnce calls it, which the live watchdog test covers.
func TestWriteHeartbeatRefreshesMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), heartbeatFile)
	if err := writeHeartbeat(path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := writeHeartbeat(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("mtime=%s", info.ModTime())
	}
}

// protects=polling backs off and respects Parro; fails_when=a failure polls at the normal rate, the wait grows without
// bound, or Retry-After is ignored; no_claim=real Parro rate limits, which are unknown.
func TestPollDelay(t *testing.T) {
	if got := pollDelay(0, 0); got != pollInterval {
		t.Fatalf("healthy=%s", got)
	}
	if got := pollDelay(1, 0); got != 2*pollInterval {
		t.Fatalf("one failure=%s", got)
	}
	if got := pollDelay(50, 0); got != maxBackoff {
		t.Fatalf("many failures=%s", got)
	}
	if got := pollDelay(1, 3*time.Hour); got != 3*time.Hour {
		t.Fatalf("retry after=%s", got)
	}
	if got := pollDelay(1, 100*time.Hour); got != maxRetryWait {
		t.Fatalf("huge retry after=%s", got)
	}
}

// protects=the bridge never runs with encryption off; fails_when=any of the three settings may be false;
// no_claim=that the running bridge encrypts, which the live check in AC 10 covers.
func TestCheckEncryption(t *testing.T) {
	if err := CheckEncryption(bridgeconfig.EncryptionConfig{Allow: true, Default: true, Require: true}); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []bridgeconfig.EncryptionConfig{
		{Default: true, Require: true}, {Allow: true, Require: true}, {Allow: true, Default: true},
	} {
		if CheckEncryption(cfg) == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}

// protects=wrong passwords cannot lock the real account; fails_when=a second try passes within the cooldown;
// no_claim=Parro's own lockout rules, which are unknown.
func TestLoginCooldown(t *testing.T) {
	c := &Connector{}
	if err := c.checkLoginCooldown(); err != nil {
		t.Fatal(err)
	}
	c.noteLoginRefused()
	if err := c.checkLoginCooldown(); err == nil {
		t.Fatal("cooldown was not enforced")
	}
}

// protects=photos are not uploaded again when Parro signs avatar URLs per request; fails_when=the signature is part of
// the avatar ID; no_claim=the exact URL format Parro will use, which is not live yet.
func TestAvatarIDIgnoresSignature(t *testing.T) {
	a := avatarID("https://x.cloudfront.net/p/1.jpg?Expires=1&Signature=a")
	b := avatarID("https://x.cloudfront.net/p/1.jpg?Expires=2&Signature=b")
	if a != b || a != "https://x.cloudfront.net/p/1.jpg" {
		t.Fatalf("a=%s b=%s", a, b)
	}
}
