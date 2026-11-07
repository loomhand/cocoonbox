package console

import (
	"strconv"
	"testing"
	"time"
)

func TestSignInCountsGoWithTheirWindow(t *testing.T) {
	l := newLimiter(time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := range 3 {
		l.allow("login-begin:10.0.0."+strconv.Itoa(i), 5)
	}
	now = now.Add(time.Minute)
	l.allow("login-begin:10.0.0.9", 5)
	if len(l.failures) != 1 {
		t.Errorf("limiter holds %d keys past their window, want 1", len(l.failures))
	}
}
