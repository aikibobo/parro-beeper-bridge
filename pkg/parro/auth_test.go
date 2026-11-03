package parro

import (
	"errors"
	"testing"
)

const loginPage = `<form id="f" method="post" action="./wicket/page?1-1.IFormSubmitListener&amp;x=2">
<input type="hidden" name="f_hf_0" value="a&amp;b"><input type="text" name="mail"><input type="password" name="geheim"></form>`

// protects=the login form is submitted to the right URL with its hidden fields; fails_when=a relative action
// or an HTML entity is passed on unresolved; no_claim=the live page, this is a synthetic copy of its shape.
func TestParseLoginForm(t *testing.T) {
	action, form, err := parseLoginForm("https://idp.test/idp/?auth=1", loginPage)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://idp.test/idp/wicket/page?1-1.IFormSubmitListener&x=2"; action != want {
		t.Fatalf("action=%s", action)
	}
	if form.Get("f_hf_0") != "a&b" {
		t.Fatalf("hidden=%q", form.Get("f_hf_0"))
	}
}

// protects=renamed form fields keep working; fails_when=hardcoded names are used despite other names on the page;
// no_claim=future page redesigns beyond input names.
func TestLoginFieldNames(t *testing.T) {
	user, pass := loginFieldNames(loginPage)
	if user != "mail" || pass != "geheim" {
		t.Fatalf("user=%s pass=%s", user, pass)
	}
}

// protects=the auth code is read from the parro:// redirect; fails_when=an error redirect or a normal URL counts
// as a code; no_claim=none beyond protects.
func TestCodeFromRedirect(t *testing.T) {
	if code, ok, err := codeFromRedirect("parro://oauth2:443/?code=abc&state=s"); !ok || err != nil || code != "abc" {
		t.Fatalf("code=%q ok=%v err=%v", code, ok, err)
	}
	if _, ok, err := codeFromRedirect("https://idp.test/next"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, _, err := codeFromRedirect("parro://oauth2/?error=denied"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err=%v", err)
	}
}

const chooserPage = `<ul><li id="id1"><span class="account-list__name"><span>Jan &amp; Co</span></span></li>
<li id="id2"><span class="account-list__name"><span>Els</span></span></li></ul>
<script>x({"u":"./page?1-accountKeuze-accounts-account-1","c":"id1"});y({"u":"./page?1-accountKeuze-accounts-account-2","c":"id2"});</script>`

// protects=a family with several identities gets to choose; fails_when=the first is silently used or names are mangled;
// no_claim=the live chooser markup, this copies the shape parsed by anneschuth/parro-cli.
func TestPickAccount(t *testing.T) {
	accounts := parseAccounts(chooserPage)
	var choice *AccountChoiceError
	if _, err := pickAccount(accounts, ""); !errors.As(err, &choice) || len(choice.Names) != 2 || choice.Names[0] != "Jan & Co" {
		t.Fatalf("err=%v choice=%+v", err, choice)
	}
	got, err := pickAccount(accounts, "Els")
	if err != nil || got.focusID != "id2" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if got, err := pickAccount(accounts[:1], ""); err != nil || got.focusID != "id1" {
		t.Fatalf("single account must need no choice: %+v %v", got, err)
	}
}

// protects=a changed login page is not reported as a wrong password; fails_when=an unknown page says bad credentials,
// which would make people retry into a lockout; no_claim=the exact page ParnasSys shows on a refusal.
func TestDiagnoseLoginPage(t *testing.T) {
	if err := diagnoseLoginPage(loginPage); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("form shown again: %v", err)
	}
	if err := diagnoseLoginPage("<html>Onderhoud</html>"); !errors.Is(err, ErrLoginPageChanged) {
		t.Fatalf("other page: %v", err)
	}
}

func FuzzLoginParsers(f *testing.F) {
	f.Add(loginPage)
	f.Add(chooserPage)
	f.Add(`<form action="`)
	f.Fuzz(func(_ *testing.T, body string) {
		_, _, _ = parseLoginForm("https://idp.test/idp/", body)
		_, _ = loginFieldNames(body)
		_, _ = pickAccount(parseAccounts(body), "x")
		_ = diagnoseLoginPage(body)
	})
}
