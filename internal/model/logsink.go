package model

// LogSink defines an interface for receiving ingested log entries.
type LogSink interface {
	PushLog(entry LogEntry) bool
}

// LogChanSink adapts a chan<- LogEntry channel into a LogSink.
type LogChanSink chan<- LogEntry

// PushLog pushes a log entry without blocking; returns false if channel is full.
func (s LogChanSink) PushLog(entry LogEntry) bool {
	select {
	case s <- entry:
		return true
	default:
		return false
	}
}
