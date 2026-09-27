package bridge

// These values are stamped by the build/release helper using Go linker flags.
// A plain `go build` deliberately makes no release or source identity claim.
var (
	Version     = "dev"
	BuildCommit = "unknown"
	BuildDate   = "unknown"
)
