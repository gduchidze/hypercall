package snapshot

import "time"

func timeAfterLock() <-chan time.Time { return time.After(5 * time.Millisecond) }
