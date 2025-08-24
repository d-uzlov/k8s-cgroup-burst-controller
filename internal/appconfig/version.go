package appconfig

import (
	"fmt"
	"runtime/debug"
)

var externalVersion string

func printVersion() {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		panic("could not read build info")
	}
	buildInfo.Deps = nil

	if externalVersion != "" {
		fmt.Println("version", externalVersion)
	} else {
		fmt.Println("custom version info is missing")
	}

	fmt.Println("golang", buildInfo.GoVersion)
	fmt.Println("package", buildInfo.Path)
	settingsToPrint := map[string]bool{
		"GOARCH":       true,
		"GOOS":         true,
		"CGO_ENABLED":  true,
		"vcs":          true,
		"vcs.revision": true,
		"vcs.time":     true,
		"vcs.modified": true,
	}
	for _, v := range buildInfo.Settings {
		if settingsToPrint[v.Key] {
			fmt.Println(v.Key, v.Value)
		}
	}
}
