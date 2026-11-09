// apply-web-links writes the provisioned dashboards with the public web origin
// substituted into "Open in foreverbull" links. Grafana starts only after this
// exits 0. The image entrypoint uses set -e, so exit 1 keeps Grafana from starting.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/lhjnilsson/foreverbull-grafana-datasource/weblinks"
)

const emptyWebOriginInfo = "FOREVERBULL_WEB_URL is empty, so the dashboards won't link to foreverbull."

const invalidWebOrigin = "FOREVERBULL_WEB_URL isn't a web address foreverbull can link to. Set grafana.webUrl to the foreverbull page's address, such as http://localhost:8080, or leave it empty to hide the links. Grafana didn't start."

func main() {
	src := flagString("src", "/etc/grafana/provisioning/dashboards")
	dst := flagString("dst", "/etc/grafana/provisioning/dashboards")
	os.Exit(run(os.Getenv(weblinks.WebOriginEnv), src, dst, os.Stderr))
}

func flagString(name, fallback string) string {
	for i := 1; i < len(os.Args)-1; i++ {
		if os.Args[i] == "-"+name {
			return os.Args[i+1]
		}
	}
	return fallback
}

// run hides the links when the origin is empty, and refuses to continue when
// it is set but not an http(s) address. A dollar sign is refused because
// Grafana would interpolate it.
func run(origin, src, dst string, out io.Writer) int {
	if strings.TrimSpace(origin) == "" {
		slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})).Info(emptyWebOriginInfo)
		if err := weblinks.Install(src, dst, ""); err != nil {
			fmt.Fprintf(out, "foreverbull web links: %v\n", err)
			return 1
		}
		return 0
	}
	if _, ok := weblinks.BacktestsURL(origin); !ok {
		fmt.Fprintln(out, invalidWebOrigin)
		return 1
	}
	if err := weblinks.Install(src, dst, origin); err != nil {
		fmt.Fprintf(out, "foreverbull web links: %v\n", err)
		return 1
	}
	return 0
}
