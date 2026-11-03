package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

const loginFlowPassword = "password"

// loginCooldown protects the real Parro account against lockout by repeated wrong passwords.
const loginCooldown = time.Minute

var (
	errWrongPassword = bridgev2.RespError{
		ErrCode:    "PARRO.BAD_CREDENTIALS",
		Err:        "Parro accepteert dit e-mailadres en wachtwoord niet",
		StatusCode: http.StatusForbidden,
	}
	errLoginCooldown = bridgev2.RespError{
		ErrCode:    "PARRO.LOGIN_COOLDOWN",
		Err:        "Wacht een minuut voordat je opnieuw probeert in te loggen",
		StatusCode: http.StatusTooManyRequests,
	}
	errLoginPageChanged = bridgev2.RespError{
		ErrCode:    "PARRO.LOGIN_PAGE_CHANGED",
		Err:        "De inlogpagina van Parro is veranderd. Je wachtwoord is waarschijnlijk goed, deze bridge heeft een update nodig.",
		StatusCode: http.StatusBadGateway,
	}
)

// GetLoginFlows offers the only flow: the ParnasSys email and password.
func (c *Connector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "E-mail en wachtwoord",
		Description: "Log in met je Parro/ParnasSys-account. Alleen de tokens worden bewaard, niet je wachtwoord.",
		ID:          loginFlowPassword,
	}}
}

// CreateLogin starts a login.
func (c *Connector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != loginFlowPassword {
		return nil, bridgev2.ErrInvalidLoginFlowID
	}
	return &passwordLogin{main: c, user: user}, nil
}

type passwordLogin struct {
	main *Connector
	user *bridgev2.User

	// Kept only between the password step and the account step.
	email    string
	password string
}

var _ bridgev2.LoginProcessUserInput = (*passwordLogin)(nil)

func (l *passwordLogin) Start(context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "parro.login.password",
		Instructions: "Log in met je Parro-account.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypeEmail, ID: "email", Name: "E-mailadres"},
			{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Wachtwoord"},
		}},
	}, nil
}

func (l *passwordLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if l.email == "" {
		l.email, l.password = input["email"], input["password"]
	}
	if err := l.main.checkLoginCooldown(); err != nil {
		l.Cancel()
		return nil, err
	}
	tokens, err := parro.Login(ctx, l.email, l.password, input["account"])
	var choice *parro.AccountChoiceError
	switch {
	case errors.As(err, &choice):
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       "parro.login.account",
			Instructions: "Deze login heeft meerdere accounts. Kies er een.",
			UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{
				{Type: bridgev2.LoginInputFieldTypeSelect, ID: "account", Name: "Account", Options: choice.Names},
			}},
		}, nil
	case errors.Is(err, parro.ErrBadCredentials):
		l.Cancel()
		l.main.noteLoginRefused()
		return nil, errWrongPassword
	case errors.Is(err, parro.ErrLoginPageChanged):
		l.Cancel()
		return nil, errLoginPageChanged
	case err != nil:
		l.Cancel()
		return nil, fmt.Errorf("parro login: %w", err)
	}
	l.Cancel()
	account, err := parro.NewClient(*tokens).GetAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch parro account: %w", err)
	}
	id, err := account.Identity.ID()
	if err != nil {
		return nil, err
	}
	name := account.Identity.Name()
	ul, err := l.user.NewLogin(ctx, &database.UserLogin{
		ID:            networkid.UserLoginID(id),
		RemoteName:    name,
		RemoteProfile: status.RemoteProfile{Name: name},
		Metadata:      &UserLoginMetadata{Tokens: *tokens},
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	if err != nil {
		return nil, fmt.Errorf("save login: %w", err)
	}
	go ul.Client.Connect(ul.Log.WithContext(l.main.Bridge.BackgroundCtx))
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         "parro.login.complete",
		Instructions:   "Ingelogd als " + name,
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}

// Cancel forgets the password.
func (l *passwordLogin) Cancel() { l.email, l.password = "", "" }

func (c *Connector) checkLoginCooldown() error {
	c.loginLock.Lock()
	defer c.loginLock.Unlock()
	if time.Since(c.lastLoginNo) < loginCooldown {
		return errLoginCooldown
	}
	return nil
}

func (c *Connector) noteLoginRefused() {
	c.loginLock.Lock()
	defer c.loginLock.Unlock()
	c.lastLoginNo = time.Now()
}
