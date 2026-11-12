package system_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/system"
)

func TestClockIsUTCAndWholeMicroseconds(t *testing.T) {
	before := time.Now().Add(-time.Second)
	now := system.Clock{}.Now()

	if now.Location() != time.UTC || now.Nanosecond()%1000 != 0 {
		t.Errorf("now = %s (%d ns): want UTC and whole microseconds", now, now.Nanosecond())
	}
	if now.Before(before) || now.After(time.Now().Add(time.Second)) {
		t.Errorf("now = %s is not the current time", now)
	}
}

func TestIDsAreDistinctVersion4UUIDs(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}

	for i := 0; i < 1000; i++ {
		id := system.IDs{}.NewID()
		if !pattern.MatchString(id) {
			t.Fatalf("%q is not a version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("%q was generated twice", id)
		}
		seen[id] = true
	}
}
