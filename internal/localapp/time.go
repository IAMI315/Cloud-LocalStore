package localapp

import "time"

const localDBTimeLayout = "2006-01-02 15:04:05"

// localProtocolTime marks a client-local database timestamp with the
// client's offset before it reaches the browser.
func localProtocolTime(value string) string {
	if value == "" {
		return ""
	}
	if parsed, err := time.ParseInLocation(localDBTimeLayout, value, time.Local); err == nil {
		return parsed.Format(time.RFC3339)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.Format(time.RFC3339)
	}
	return value
}
