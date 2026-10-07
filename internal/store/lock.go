package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive lock on the file at path, creating it if
// needed, and returns the function that releases it. It waits for a process
// that holds the lock, polling until ctx ends.
//
// This is how processes take turns at migrating. SQLite's own locks protect
// one statement or one transaction, but bringing a schema up to date is
// several transactions (create the version table, then one per migration),
// and two processes interleaving them collide. goose can lock across
// processes, but only on Postgres and MySQL, whose servers offer a lock;
// SQLite has no server, so the lock is a file next to the database.
//
// flock(2) locks are released by the kernel when the process exits, however
// it exits, so a crashed process never leaves the lock held.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: opening lock file: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("store: locking %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("store: waiting for %s: %w", path, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Closing the file releases the lock.
	return func() { f.Close() }, nil
}
