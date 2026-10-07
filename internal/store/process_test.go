package store_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fleveque/quantic-agent/internal/store"
)

// The test binary doubles as the second process: run with this variable set,
// TestHelperOpen opens the database it names and exits. The race this guards
// against is between processes (the daemon and a command run by hand, say),
// so the test uses real processes, without building another binary.
const helperEnv = "STORE_TEST_OPEN"

func TestHelperOpen(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		t.Skip("only runs as a child process of TestProcessesOpeningANewDatabase")
	}
	s, err := store.Open(context.Background(), path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s.Close()
}

// Several processes opening the same new database at the same moment: each
// must find the schema created exactly once, by whichever got there first.
func TestProcessesOpeningANewDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("starts 32 processes")
	}
	for round := range 8 {
		path := filepath.Join(t.TempDir(), "agent.db")
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOpen$")
				cmd.Env = append(os.Environ(), helperEnv+"="+path)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("round %d: %v: %s", round, err, out)
				}
			})
		}
		wg.Wait()
	}
}
