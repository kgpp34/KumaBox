package version

// These values are overridden by the Makefile at build time.
var (
	Version   = "0.0.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

func Info() BuildInfo {
	return BuildInfo{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
}
