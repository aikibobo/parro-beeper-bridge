// Package connector implements the bridgev2 network connector for Parro.
package connector

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/aikibobo/parro-beeper-bridge/pkg/parro"
)

const (
	logoKV       database.Key = "parro_logo_mxc"
	botAvatarLag              = 10 * time.Second
)

// logoFile is fetched by install.sh. The logo is Parro's, so it stays out of this repository.
const logoFile = "parro-logo.png"

var logoPNG []byte

// Connector is the Parro network connector.
type Connector struct {
	Bridge *bridgev2.Bridge

	icon id.ContentURIString

	loginLock   sync.Mutex
	lastLoginNo time.Time
}

// CheckEncryption refuses a config where messages of other parents and children could reach a server in the clear.
func CheckEncryption(cfg bridgeconfig.EncryptionConfig) error {
	if !cfg.Allow || !cfg.Default || !cfg.Require {
		return errors.New("encryption.allow, default and require must all be true in config.yaml")
	}
	return nil
}

func logoAvatar() *bridgev2.Avatar {
	if len(logoPNG) == 0 {
		return nil
	}
	return &bridgev2.Avatar{
		ID:  "parro-logo-1",
		Get: func(context.Context) ([]byte, error) { return logoPNG, nil },
	}
}

var _ bridgev2.NetworkConnector = (*Connector)(nil)

// UserLoginMetadata is the only place Parro tokens are stored.
type UserLoginMetadata struct {
	Tokens parro.Tokens `json:"tokens"`
}

// Init stores the bridge.
func (c *Connector) Init(bridge *bridgev2.Bridge) { c.Bridge = bridge }

// Start uploads the Parro logo once. Polling itself runs per login.
func (c *Connector) Start(ctx context.Context) error {
	log := zerolog.Ctx(ctx)
	logo, err := os.ReadFile(logoFile)
	if err != nil {
		log.Info().Msg("No Parro logo found, chats get no avatar")
		return nil
	}
	logoPNG = logo
	mxc := id.ContentURIString(c.Bridge.DB.KV.Get(ctx, logoKV))
	if mxc == "" {
		uploaded, _, err := c.Bridge.Bot.UploadMedia(ctx, "", logoPNG, "parro.png", "image/png")
		if err != nil {
			log.Err(err).Msg("Failed to upload Parro logo")
			return nil
		}
		mxc = uploaded
		c.Bridge.DB.KV.Set(ctx, logoKV, string(mxc))
	}
	c.icon = mxc
	// The bridge updates the bot profile from its config right after this, so set the avatar a moment later.
	go func() {
		bg := log.WithContext(c.Bridge.BackgroundCtx)
		time.Sleep(botAvatarLag)
		if err := c.Bridge.Bot.SetAvatarURL(bg, mxc); err != nil {
			log.Err(err).Msg("Failed to set bot avatar")
		}
	}()
	return nil
}

// GetName describes the network.
func (c *Connector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Parro",
		NetworkURL:           "https://www.parro.com",
		NetworkIcon:          c.icon,
		NetworkID:            "parro",
		BeeperBridgeType:     "github.com/aikibobo/parro-beeper-bridge",
		DefaultPort:          29341,
		DefaultCommandPrefix: "!parro",
	}
}

// GetDBMetaTypes registers the login metadata.
func (c *Connector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any { return &UserLoginMetadata{} },
		Message:   func() any { return &MessageMetadata{} },
	}
}

// GetCapabilities reports no general capabilities.
func (c *Connector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

// GetConfig reports that the bridge has no network-specific config.
func (c *Connector) GetConfig() (string, any, configupgrade.Upgrader) {
	return "", nil, configupgrade.NoopUpgrader
}

// GetBridgeInfoVersion is bumped when capabilities change.
func (c *Connector) GetBridgeInfoVersion() (int, int) { return 3, 2 }

// LoadUserLogin attaches a client to a stored login, without network IO.
func (c *Connector) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	api := parro.NewClient(meta.Tokens)
	api.OnTokens = func(tokens parro.Tokens) error {
		meta.Tokens = tokens
		return login.Save(context.Background())
	}
	login.Client = &Client{Main: c, UserLogin: login, API: api, ownIDs: map[string]bool{}, people: map[string]parro.Identity{}, resynced: map[networkid.PortalID]bool{}}
	return nil
}
