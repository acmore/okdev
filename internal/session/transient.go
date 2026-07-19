package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const transientStreakFileName = "transient-streak.json"

// TransientStreak counts consecutive transient cluster-contact failures for
// a session. A monitoring loop retrying on exit 78 has no way to tell the
// 20th identical failure from the first (#173); the streak is what turns
// "transient, retry" into "sustained outage, stop retrying and escalate".
// Any successful cluster contact clears it.
type TransientStreak struct {
	Count   int       `json:"count"`
	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
}

// Span is how long the streak has been running.
func (s TransientStreak) Span() time.Duration {
	if s.Count == 0 || s.FirstAt.IsZero() {
		return 0
	}
	return s.LastAt.Sub(s.FirstAt)
}

func transientStreakPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, transientStreakFileName), nil
}

// RecordTransientFailure increments the session's streak and returns the
// updated value. Best-effort persistence: the caller still reports the
// underlying failure even if the streak cannot be written.
func RecordTransientFailure(name string, now time.Time) (TransientStreak, error) {
	if strings.TrimSpace(name) == "" {
		return TransientStreak{Count: 1, FirstAt: now, LastAt: now}, nil
	}
	path, err := transientStreakPath(name)
	if err != nil {
		return TransientStreak{Count: 1, FirstAt: now, LastAt: now}, err
	}
	streak := TransientStreak{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &streak)
	}
	if streak.Count == 0 || streak.FirstAt.IsZero() {
		streak = TransientStreak{Count: 0, FirstAt: now}
	}
	streak.Count++
	streak.LastAt = now
	payload, err := json.Marshal(streak)
	if err != nil {
		return streak, fmt.Errorf("marshal transient streak: %w", err)
	}
	if err := atomicWriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return streak, fmt.Errorf("write transient streak: %w", err)
	}
	return streak, nil
}

// ClearTransientStreak forgets the streak (called on any successful cluster
// contact).
func ClearTransientStreak(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := transientStreakPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear transient streak: %w", err)
	}
	return nil
}
