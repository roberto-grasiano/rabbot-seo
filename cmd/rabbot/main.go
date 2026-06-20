// Command rabbot is the entrypoint for the Rabbot-SEO real-time SEO
// monitoring gateway. It constructs the cobra command tree and runs it.
package main

import (
	"fmt"
	"os"

	"github.com/roberto-grasiano/rabbot-seo/internal/cli"
)

// Build metadata, injected at link time via:
//
//	-ldflags "-X main.version=... -X main.commit=... -X main.date=..."
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	bi := cli.BuildInfo{Version: version, Commit: commit, Date: date}
	if err := cli.Execute(bi); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
