package streams

import "time"

// timeNowUnixNano isolates the stdlib call so the file above stays free
// of the time import and is easy to read against the other-language
// equivalents.
func timeNowUnixNano() int64 { return time.Now().UnixNano() }
