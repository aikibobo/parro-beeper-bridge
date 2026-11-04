// parro-beeper-bridge shows Parro chats in Beeper, read-only.
package main

import (
	"fmt"
	"os"

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
		Name:        "parro-beeper-bridge",
		Description: "A read-only Parro to Beeper bridge",
		URL:         "https://github.com/aikibobo/parro-beeper-bridge",
		Version:     "0.1.0",
		Connector:   &connector.Connector{},
	}
	m.PostInit = func() {
		if err := connector.CheckEncryption(m.Config.Encryption); err != nil {
			fmt.Fprintln(os.Stderr, "parro-beeper-bridge refuses to start:", err)
			os.Exit(1)
		}
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
