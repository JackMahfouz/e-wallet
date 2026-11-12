// Package system is the driven adapter for the things the core must not reach for itself: the
// time and new ids.
package system

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
)

// Clock is the wall clock, in UTC and whole microseconds (what the database stores, so a time that
// is written reads back equal).
type Clock struct{}

var _ application.Clock = Clock{}

// Now implements application.Clock.
func (Clock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// IDs makes random version-4 UUIDs.
type IDs struct{}

var _ application.IDGenerator = IDs{}

// NewID implements application.IDGenerator.
func (IDs) NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err)) // the system is broken; nothing sensible follows
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
