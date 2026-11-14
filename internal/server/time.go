package server

import "time"

const serverDBTimeLayout = "2006-01-02 15:04:05"

// serverProtocolTime converts a server-local database timestamp into an
// unambiguous instant for the client. The offset identifies the server's
// timezone; the browser can then render it in the client's timezone.
func serverProtocolTime(value string) string {
	if value == "" {
		return ""
	}
	if parsed, err := time.ParseInLocation(serverDBTimeLayout, value, time.Local); err == nil {
		return parsed.Format(time.RFC3339)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.Format(time.RFC3339)
	}
	return value
}

func parseServerTime(value string) (time.Time, error) {
	if parsed, err := time.ParseInLocation(serverDBTimeLayout, value, time.Local); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}
