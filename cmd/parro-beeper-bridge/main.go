// parro-beeper-bridge shows Parro chats in Beeper, read-only.
package main

import (
	"fmt"
	"os"
	"strings"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/aikibobo/parro-beeper-bridge/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		// Name owns the bridge database. It stays "parro-bridge" from v0.1.0, because mautrix refuses a
		// database owned by another name, which would lock existing installs out of their login and chats.
		Name:        "parro-bridge",
		Description: "A read-only Parro to Beeper bridge",
		URL:         "https://github.com/aikibobo/parro-beeper-bridge",
		Version:     "0.0.0",
		Connector:   &connector.Connector{},
	}
	m.PostInit = func() {
		if err := connector.CheckEncryption(m.Config.Encryption); err != nil {
			fmt.Fprintln(os.Stderr, "parro-beeper-bridge refuses to start:", err)
			os.Exit(1)
		}
	}
	// Release builds set Tag to the git tag; without this the version would read 0.0.0+dev.
	if strings.HasPrefix(Tag, "v") {
		m.Version = strings.TrimPrefix(Tag, "v")
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
