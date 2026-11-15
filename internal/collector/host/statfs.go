package host

// StatfsData captures essential filesystem space metrics from statfs.
type StatfsData struct {
	Blocks uint64
	Bfree  uint64
	Bavail uint64
	Bsize  uint64
}

// statfsFunc allows mocking filesystem statistics in tests.
var statfsFunc = platformStatfs
