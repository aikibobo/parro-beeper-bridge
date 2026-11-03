// Package parro is a read-only client for the reverse-engineered Parro REST API.
package parro

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	clientID    = "W52dbSBQuFp-LF4Xch1r"
	redirectURI = "parro://oauth2"
	userAgent   = "Mozilla/5.0 (Macintosh) parro-beeper-bridge"
	maxHops     = 20
)

// ErrBadCredentials means Parro rejected the password or refresh token, so the user has to log in again.
var ErrBadCredentials = errors.New("parro rejected the credentials")

// ErrLoginPageChanged means the login flow ended somewhere this client does not recognise.
// It is not a wrong password, so nobody should keep retrying their password because of it.
var ErrLoginPageChanged = errors.New("the parro login page is not what this bridge expects")

// AccountChoiceError means one login has several identities and the user has to pick one.
type AccountChoiceError struct{ Names []string }

func (e *AccountChoiceError) Error() string {
	return fmt.Sprintf("parro login has %d accounts, choose one", len(e.Names))
}

// Tokens is the OAuth2 token set. Refreshing rotates RefreshToken and invalidates the old one.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

var (
	idpBase = "https://inloggen.parnassys.net"

	formActionRe    = regexp.MustCompile(`(?i)<form[^>]*action="([^"]*)"`)
	inputTagRe      = regexp.MustCompile(`(?i)<input[^>]*>`)
	inputTypeRe     = regexp.MustCompile(`(?i)type="([^"]*)"`)
	inputNameRe     = regexp.MustCompile(`(?i)name="([^"]*)"`)
	inputValueRe    = regexp.MustCompile(`(?i)value="([^"]*)"`)
	passwordInputRe = regexp.MustCompile(`(?i)<input[^>]*type="password"`)
	accountItemRe   = regexp.MustCompile(`(?s)<li[^>]*id="([^"]*)"[^>]*>(.*?)</li>`)
	accountNameRe   = regexp.MustCompile(`(?s)account-list__name"[^>]*>(.*?)</span>\s*</span>`)
	htmlTagRe       = regexp.MustCompile(`<[^>]+>`)
	accountRe       = regexp.MustCompile(`"u":"([^"]*accountKeuze-accounts-account-\d+)","c":"([^"]+)"`)
	ajaxBaseRe      = regexp.MustCompile(`Wicket\.Ajax\.baseUrl="([^"]*)"`)
	ajaxRedirRe     = regexp.MustCompile(`<redirect><!\[CDATA\[(.*?)\]\]></redirect>`)
)

// Login runs the headless OAuth2 PKCE flow against the ParnasSys login page. When the login has
// several identities and account is empty, it returns an AccountChoiceError with their names.
func Login(ctx context.Context, email, password, account string) (*Tokens, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	state, _, err := newPKCE()
	if err != nil {
		return nil, err
	}
	authorize := idpBase + "/idp/oauth2/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	page, err := send(ctx, hc, http.MethodGet, authorize, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("open authorize page: %w", err)
	}
	for hops := 0; page.redirect() && hops < maxHops; hops++ {
		if page, err = send(ctx, hc, http.MethodGet, page.absLocation(), nil, nil); err != nil {
			return nil, fmt.Errorf("follow to login page: %w", err)
		}
	}
	action, form, err := parseLoginForm(page.url, page.body)
	if err != nil {
		return nil, err
	}
	userField, passField := loginFieldNames(page.body)
	form.Set(userField, email)
	form.Set(passField, password)
	form.Set("aanmelden", "x") // Wicket requires the submit button name

	page, err = send(ctx, hc, http.MethodPost, action, strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return nil, fmt.Errorf("submit login form: %w", err)
	}
	code, err := followToCode(ctx, hc, page, account)
	if err != nil {
		return nil, err
	}
	return postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// Refresh exchanges a refresh token. The caller must persist the result before using it.
func Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	tokens, err := postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
	var rejected *tokenRejectedError
	if errors.As(err, &rejected) {
		return nil, ErrBadCredentials
	}
	return tokens, err
}

func followToCode(ctx context.Context, hc *http.Client, page *response, account string) (string, error) {
	var err error
	for hops := 0; hops < maxHops; hops++ {
		if !page.redirect() {
			if !strings.Contains(page.body, "accountKeuze") {
				return "", diagnoseLoginPage(page.body)
			}
			location, err := chooseAccount(ctx, hc, page, account)
			if err != nil {
				return "", err
			}
			if code, ok, err := codeFromRedirect(location); ok || err != nil {
				return code, err
			}
			if page, err = send(ctx, hc, http.MethodGet, resolve(page.url, location), nil, nil); err != nil {
				return "", fmt.Errorf("follow account choice: %w", err)
			}
			continue
		}
		if code, ok, err := codeFromRedirect(page.location); ok || err != nil {
			return code, err
		}
		if page, err = send(ctx, hc, http.MethodGet, page.absLocation(), nil, nil); err != nil {
			return "", fmt.Errorf("follow login redirect: %w", err)
		}
	}
	return "", errors.New("parro login: no authorization code after too many redirects")
}

// diagnoseLoginPage separates a refused password, where the form comes back, from any other page.
func diagnoseLoginPage(body string) error {
	if passwordInputRe.MatchString(body) {
		return ErrBadCredentials
	}
	return ErrLoginPageChanged
}

type accountOption struct{ name, url, focusID string }

// parseAccounts reads the "Account kiezen" page. Names are empty when the markup is not recognised.
func parseAccounts(body string) []accountOption {
	items := map[string]string{}
	for _, m := range accountItemRe.FindAllStringSubmatch(body, -1) {
		items[m[1]] = m[2]
	}
	var accounts []accountOption
	for _, m := range accountRe.FindAllStringSubmatch(body, -1) {
		option := accountOption{url: m[1], focusID: m[2]}
		if name := accountNameRe.FindStringSubmatch(items[m[2]]); name != nil {
			text := html.UnescapeString(htmlTagRe.ReplaceAllString(name[1], " "))
			option.name = strings.Join(strings.Fields(text), " ")
		}
		accounts = append(accounts, option)
	}
	return accounts
}

func pickAccount(accounts []accountOption, wanted string) (*accountOption, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("%w: account chooser without accounts", ErrLoginPageChanged)
	}
	if len(accounts) == 1 {
		return &accounts[0], nil
	}
	names := make([]string, 0, len(accounts))
	for i := range accounts {
		if wanted != "" && accounts[i].name == wanted {
			return &accounts[i], nil
		}
		names = append(names, accounts[i].name)
	}
	return nil, &AccountChoiceError{Names: names}
}

func chooseAccount(ctx context.Context, hc *http.Client, page *response, wanted string) (string, error) {
	chosen, err := pickAccount(parseAccounts(page.body), wanted)
	if err != nil {
		return "", err
	}
	base := ajaxBaseRe.FindStringSubmatch(page.body)
	ajaxBase := ""
	if base != nil {
		ajaxBase = base[1]
	} else if u, err := url.Parse(page.url); err == nil {
		ajaxBase = u.Path
	}
	resp, err := send(ctx, hc, http.MethodGet, resolve(page.url, chosen.url), nil, map[string]string{
		"Wicket-Ajax":             "true",
		"Wicket-Ajax-BaseURL":     ajaxBase,
		"Wicket-FocusedElementId": chosen.focusID,
		"X-Requested-With":        "XMLHttpRequest",
	})
	if err != nil {
		return "", fmt.Errorf("choose account: %w", err)
	}
	if loc := resp.header.Get("Ajax-Location"); loc != "" {
		return loc, nil
	}
	if r := ajaxRedirRe.FindStringSubmatch(resp.body); r != nil {
		return r[1], nil
	}
	return "", fmt.Errorf("parro login: account choice gave no redirect (HTTP %d)", resp.status)
}

// codeFromRedirect reports the code in a parro:// redirect, and ok=false for any other location.
func codeFromRedirect(location string) (code string, ok bool, err error) {
	if !strings.HasPrefix(location, "parro://") {
		return "", false, nil
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", false, errors.New("parro login: malformed redirect")
	}
	if code = u.Query().Get("code"); code != "" {
		return code, true, nil
	}
	return "", false, fmt.Errorf("%w: %s", ErrBadCredentials, u.Query().Get("error"))
}

func parseLoginForm(pageURL, body string) (string, url.Values, error) {
	m := formActionRe.FindStringSubmatch(body)
	if m == nil {
		return "", nil, errors.New("parro login: form not found, the login page may have changed")
	}
	form := url.Values{}
	for _, tag := range inputTagRe.FindAllString(body, -1) {
		typ, name := inputTypeRe.FindStringSubmatch(tag), inputNameRe.FindStringSubmatch(tag)
		if typ == nil || name == nil || !strings.EqualFold(typ[1], "hidden") {
			continue
		}
		value := ""
		if v := inputValueRe.FindStringSubmatch(tag); v != nil {
			value = html.UnescapeString(v[1])
		}
		form.Set(name[1], value)
	}
	return resolve(pageURL, html.UnescapeString(m[1])), form, nil
}

// loginFieldNames reads the input names from the page, because ParnasSys has renamed them before.
func loginFieldNames(body string) (user, pass string) {
	user, pass = "emailadres", "wachtwoord"
	for _, tag := range inputTagRe.FindAllString(body, -1) {
		name := inputNameRe.FindStringSubmatch(tag)
		if name == nil {
			continue
		}
		typ := "text"
		if t := inputTypeRe.FindStringSubmatch(tag); t != nil {
			typ = strings.ToLower(t[1])
		}
		switch {
		case typ == "password":
			pass = name[1]
		case (typ == "text" || typ == "email") && !strings.Contains(strings.ToLower(name[1]), "password"):
			user = name[1]
		}
	}
	return user, pass
}

type tokenRejectedError struct{ status int }

func (e *tokenRejectedError) Error() string {
	return fmt.Sprintf("token request rejected: HTTP %d", e.status)
}

func postToken(ctx context.Context, form url.Values) (*Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, idpBase+"/idp/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return nil, &tokenRejectedError{resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request: HTTP %d", resp.StatusCode)
	}
	var tokens Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return nil, errors.New("token response without access or refresh token")
	}
	return &tokens, nil
}

func newPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

type response struct {
	status   int
	url      string
	location string
	header   http.Header
	body     string
}

func (r *response) redirect() bool { return r.status >= 300 && r.status < 400 }

func (r *response) absLocation() string { return resolve(r.url, r.location) }

func send(ctx context.Context, hc *http.Client, method, target string, body io.Reader, headers map[string]string) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &response{resp.StatusCode, target, resp.Header.Get("Location"), resp.Header, string(data)}, nil
}
