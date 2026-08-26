package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"rbdb/internal/progress"
)

// PlainProgressHandler returns a send callback for plain-text (non-TUI) output.
func PlainProgressHandler(label, dir string, doneCh chan error) func(any) {
	return func(m any) {
		switch msg := m.(type) {
		case progress.Found:
			fmt.Printf("Found %d %s files in %s\n", msg.N, label, dir)
		case progress.FileStart:
			if msg.Done%10 == 0 || msg.Done == msg.Total {
				fmt.Printf("\rProcessing %d/%d: %s...", msg.Done, msg.Total, filepath.Base(msg.Path))
			}
		case progress.FileDone:
			if msg.Err != nil {
				vlogf("ERROR %s: %v", msg.Path, msg.Err)
			} else if msg.Skipped {
				vlogf("SKIP %s", msg.Path)
			}
		case progress.ArtworkFetched:
			vlogf("Art fetched: %s", filepath.Base(msg.Path))
		case progress.MetadataNormalized:
			vlogf("Normalized: %s", filepath.Base(msg.Path))
		case progress.Done:
			doneCh <- msg.Err
		}
	}
}

// WaitForDone blocks until a result is received on doneCh and exits on error.
func WaitForDone(doneCh chan error, dir string, label string) {
	if err := <-doneCh; err != nil {
		fmt.Println()
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", label, err)
		os.Exit(1)
	}
	fmt.Printf("\rDone: %s in %s                    \n", label, dir)
}
