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
	Path        string
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

type ArtworkSearch struct {
	Path string
}

type SkippedHasArt struct {
	Path string
}

type MetadataNormalized struct {
	Path string
}

type Banner struct {
	Lines []string
}

// StockDB reports the outcome of a sync-stock-db run. N is the number of
// tracks written to the stock database.
type StockDB struct {
	N       int
	Albums  int
	Classic bool
	Written bool // true if the iTunesDB file was written (false for -dry-run)
	Err     error
}
