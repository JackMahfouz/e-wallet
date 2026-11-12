package domain

import (
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseID checks that value is a UUID and returns it in canonical (lower-case) form. field names
// the value in the error.
func ParseID(field, value string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(value))
	if !uuidPattern.MatchString(id) {
		return "", Invalidf("%s must be a UUID", field)
	}

	return id, nil
}
