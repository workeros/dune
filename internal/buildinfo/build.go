// Package buildinfo reports identity embedded in the executing program.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
)

// Version can be set at build time using -ldflags -X. Git revision and modified
// state come from Go's VCS build information, never from the current checkout.
var Version = "dev"

func Current() api.BuildInfo {
	info := api.BuildInfo{Version: safe(Version), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if embedded, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range embedded.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.Revision = safe(setting.Value)
			case "vcs.modified":
				info.Modified = setting.Value == "true"
			}
		}
	}
	return info
}

func safe(value string) string {
	if len(value) > 128 || strings.ContainsFunc(value, unicode.IsControl) {
		return "invalid"
	}
	return value
}
