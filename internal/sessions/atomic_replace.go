package sessions

import (
	"os"
	"time"
)

var atomicReplaceRetryDelays = [...]time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
}

func replaceFileAtomic(source, target string) error {
	for attempt := 0; ; attempt++ {
		err := os.Rename(source, target)
		if err == nil {
			return nil
		}
		if attempt >= len(atomicReplaceRetryDelays) || !isRetryableAtomicReplaceError(err) {
			return err
		}
		time.Sleep(atomicReplaceRetryDelays[attempt])
	}
}
