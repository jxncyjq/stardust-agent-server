package version

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

func Info() BuildInfo {
	return BuildInfo{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
}
