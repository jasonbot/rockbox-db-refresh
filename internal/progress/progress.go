// Package progress defines the message types streamed from the build
// pipeline (main) to the interfaces that display it.
package progress

type Found struct{ N int }

type Parse struct {
	Done, Total int
	Path        string
	Reused      bool // -refresh: carried over from the previous database
}

type Skip struct {
	Path string
	Err  error
}

type TagStart struct{ Tag int }
type TagDone struct{ Tag int }

type Shuffle struct {
	N   int
	Err error
}

type Refresh struct {
	Kept, Updated, Added, Removed int
}

type Done struct{ Err error }

type Cancelled struct{}

// Fix/Sync progress messages

type FileStart struct {
	Path       string
	Done, Total int
}

type FileDone struct {
	Path    string
	Skipped bool
	Err     error
}

type ArtworkFetched struct {
	Path string
}

type MetadataNormalized struct {
	Path string
}
