package parro

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	oldRest, oldIDP := restBase, idpBase
	restBase, idpBase = srv.URL+"/rest", srv.URL
	t.Cleanup(func() { restBase, idpBase = oldRest, oldIDP })
}

// protects=the client cannot change anything in Parro; fails_when=a non-GET request leaves the transport;
// no_claim=login and token calls, which use a separate HTTP client on purpose.
func TestTransportRefusesNonGet(t *testing.T) {
	hit := false
	fakeAPI(t, func(http.ResponseWriter, *http.Request) { hit = true })
	req, _ := http.NewRequest(http.MethodPost, restBase+"/chatroom", strings.NewReader("x"))
	if _, err := NewClient(Tokens{}).http.Do(req); err == nil {
		t.Fatal("POST was not refused")
	}
	if hit {
		t.Fatal("POST reached the server")
	}
}

// protects=a crash cannot lose the session; fails_when=a rotated token is used before it is persisted,
// or the request is not retried after refresh; no_claim=real Parro token behaviour, the server is a fake.
func TestRefreshPersistsBeforeUse(t *testing.T) {
	var persistedBeforeRetry bool
	var persisted string
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/idp/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"rotated"}`))
		case r.Header.Get("Authorization") == "Bearer old":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			persistedBeforeRetry = persisted == "rotated"
			_, _ = w.Write([]byte(`{"items":[{"title":"x"}]}`))
		}
	})
	c := NewClient(Tokens{AccessToken: "old", RefreshToken: "r"})
	c.OnTokens = func(tok Tokens) error { persisted = tok.RefreshToken; return nil }
	rooms, err := c.GetChatRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || !persistedBeforeRetry {
		t.Fatalf("rooms=%d persistedBeforeRetry=%v", len(rooms), persistedBeforeRetry)
	}
}

// protects=a failed save never leaves an unsaved token in use; fails_when=the client continues after OnTokens fails;
// no_claim=database behaviour, OnTokens is a stub.
func TestRefreshStopsWhenPersistFails(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/idp/oauth2/token" {
			_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"rotated"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	c := NewClient(Tokens{AccessToken: "old", RefreshToken: "r"})
	c.OnTokens = func(Tokens) error { return errors.New("disk full") }
	if _, err := c.GetChatRooms(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if c.tokens.AccessToken != "old" {
		t.Fatalf("unsaved token in use: %s", c.tokens.AccessToken)
	}
}

// protects=a revoked login shows as a login problem; fails_when=a rejected refresh is not ErrBadCredentials;
// no_claim=which status codes the real IdP uses beyond 400 and 401.
func TestRejectedRefreshIsBadCredentials(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/idp/oauth2/token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := NewClient(Tokens{AccessToken: "old", RefreshToken: "r"}).GetChatRooms(context.Background())
	if !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("got %v", err)
	}
}

// protects=own messages are recognised through both identity records; fails_when=a guardian ID is missed;
// no_claim=none beyond protects.
func TestOwnIDs(t *testing.T) {
	var a Account
	a.Identity.Links = []Link{{ID: 1, Rel: "self"}}
	a.Identity.Guardians = []Identity{{Links: []Link{{ID: 2, Rel: "self"}}}}
	own, err := a.OwnIDs()
	if err != nil || !own["1"] || !own["2"] || len(own) != 2 {
		t.Fatalf("own=%v err=%v", own, err)
	}
}

// protects=list endpoints work with the status the live API sends; fails_when=206 counts as an error;
// no_claim=paging past the first page, which arrives in build step 2.
func TestPartialContentIsSuccess(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(`{"items":[{"title":"x"}]}`))
	})
	rooms, err := NewClient(Tokens{AccessToken: "a", RefreshToken: "r"}).GetChatRooms(context.Background())
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms=%d err=%v", len(rooms), err)
	}
}

// protects=catch-up after downtime reads older pages and stops at known messages; fails_when=it stops after one page or
// keeps walking past the stop; no_claim=the live server, whose Range handling is only copied into this fake.
func TestPagingFollowsRangeAndStop(t *testing.T) {
	var ranges []string
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		items := make([]string, pageSize)
		for i := range items {
			items[i] = `{"text":"x"}`
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
	})
	pages := 0
	got, err := NewClient(Tokens{AccessToken: "a", RefreshToken: "r"}).GetChatMessages(context.Background(), 1, func([]ChatMessage) bool {
		pages++
		return pages == 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2*pageSize || len(ranges) != 2 || ranges[1] != "items=100-199" {
		t.Fatalf("items=%d ranges=%v", len(got), ranges)
	}
}

// protects=a list ending exactly on a page boundary is not an error; fails_when=416 fails the poll;
// no_claim=none beyond protects.
func TestRangePastEndEndsList(t *testing.T) {
	calls := 0
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		items := make([]string, pageSize)
		for i := range items {
			items[i] = `{"text":"x"}`
		}
		_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
	})
	got, err := NewClient(Tokens{AccessToken: "a", RefreshToken: "r"}).GetChatMessages(context.Background(), 1, nil)
	if err != nil || len(got) != pageSize {
		t.Fatalf("items=%d err=%v", len(got), err)
	}
}

// protects=attachment downloads stay on Parro's own hosts over https; fails_when=http or another host is fetched;
// no_claim=that this list covers every host Parro will ever use, an unknown host becomes a visible failed attachment.
func TestDownloadRefusesOtherHosts(t *testing.T) {
	c := NewClient(Tokens{})
	for _, target := range []string{"http://x.cloudfront.net/a", "https://evil.test/a", "https://cloudfront.net.evil.test/a", "file:///etc/passwd"} {
		if _, err := c.Download(context.Background(), target); err == nil {
			t.Fatalf("%s was not refused", target)
		}
	}
	if err := downloadAllowed("https://abc.cloudfront.net/a.jpg"); err != nil {
		t.Fatal(err)
	}
}

// protects=a too large file fails instead of arriving broken; fails_when=the data is cut off silently;
// no_claim=none beyond protects.
func TestDownloadRefusesTruncation(t *testing.T) {
	allowInsecureHosts = true
	oldMax := maxDownloadBytes
	maxDownloadBytes = 4
	t.Cleanup(func() { allowInsecureHosts, maxDownloadBytes = false, oldMax })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("12345")) }))
	defer srv.Close()
	if _, err := NewClient(Tokens{}).Download(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error")
	}
}

// protects=attachment URLs never reach the logs through errors; fails_when=a network error text contains the URL;
// no_claim=other log lines, which do not receive URLs.
func TestDownloadErrorHasNoURL(t *testing.T) {
	allowInsecureHosts = true
	t.Cleanup(func() { allowInsecureHosts = false })
	srv := httptest.NewServer(http.NotFoundHandler())
	target := srv.URL + "/secret-name.jpg"
	srv.Close()
	_, err := NewClient(Tokens{}).Download(context.Background(), target)
	if err == nil || strings.Contains(err.Error(), "secret-name") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("err=%v", err)
	}
}

// protects=the poller can honour a rate limit; fails_when=Retry-After is lost; no_claim=real Parro rate limits, unknown.
func TestHTTPErrorCarriesRetryAfter(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := NewClient(Tokens{AccessToken: "a", RefreshToken: "r"}).GetChatRooms(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.RetryAfter != 2*time.Minute {
		t.Fatalf("err=%v", err)
	}
}

// protects=a changed API says so instead of "unreachable"; fails_when=an HTML page is a generic error;
// no_claim=every way the API could change.
func TestUnreadableAnswerIsAPIChange(t *testing.T) {
	fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	_, err := NewClient(Tokens{AccessToken: "a", RefreshToken: "r"}).GetChatRooms(context.Background())
	if !errors.Is(err, ErrAPIChanged) {
		t.Fatalf("err=%v", err)
	}
}
