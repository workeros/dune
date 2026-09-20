package api

// Raw ACP transports exact bytes. The caller, rather than the host, owns ACP
// request correlation and protocol state across reconnects.
const (
	RawACPMaxMessageBytes    = 1024 * 1024
	RawACPMaxPendingBytes    = 4 * 1024 * 1024
	RawACPMaxPendingMessages = 32
	RawACPStdoutBytes        = 8 * 1024 * 1024
	RawACPStderrBytes        = 1024 * 1024
	RawACPMaxReadBytes       = 256 * 1024
)

// RawACPTake replaces the input owner only if ExpectedEpoch still matches.
// OwnerID is generated and retained by the caller before submitting the take.
// The submission uses the same Runtime namespace as all other mutations.
type RawACPTake struct {
	StreamID      string `json:"stream_id"`
	ExpectedEpoch uint64 `json:"expected_epoch"`
	OwnerID       string `json:"owner_id"`
}

// RawACPWrite carries one complete JSON-RPC line, including its original LF or
// CRLF. SHA256 is the lowercase hex digest of Data. No bytes reach stdin before
// the complete bounded message is admitted. A receipt never proves RPC success.
type RawACPWrite struct {
	StreamID   string `json:"stream_id"`
	InputEpoch uint64 `json:"input_epoch"`
	OwnerID    string `json:"owner_id"`
	Length     int    `json:"length"`
	SHA256     string `json:"sha256"`
	Data       []byte `json:"data"`
}

// RawACPInputReceipt identifies the original admission order. Sequence is zero
// for ownership changes; writes have a strictly increasing nonzero sequence.
type RawACPInputReceipt struct {
	StreamID   string `json:"stream_id"`
	InputEpoch uint64 `json:"input_epoch"`
	Sequence   uint64 `json:"sequence,omitempty"`
	Length     int    `json:"length,omitempty"`
}

type RawACPWindow struct {
	Oldest     uint64 `json:"oldest"`
	Next       uint64 `json:"next"`
	LimitBytes int    `json:"limit_bytes"`
	Closed     bool   `json:"closed"`
}

type RawACPState struct {
	StreamID   string `json:"stream_id"`
	InputEpoch uint64 `json:"input_epoch"`
	// ready, closed, or input_unrecoverable. Reconnect/take never repairs input.
	InputState        string        `json:"input_state"`
	PendingMessages   CapacityUsage `json:"pending_messages"`
	PendingBytes      CapacityUsage `json:"pending_bytes"`
	MessageLimitBytes int           `json:"message_limit_bytes"`
	Stdout            RawACPWindow  `json:"stdout"`
	Stderr            RawACPWindow  `json:"stderr"`
}

// RawACPRead observes one independent byte window. Offsets count original
// bytes, including line endings. A stale offset returns STREAM_GAP together
// with the current window; it never silently resumes at a different position.
type RawACPRead struct {
	StreamID string `json:"stream_id"`
	Channel  string `json:"channel"` // stdout or stderr
	Offset   uint64 `json:"offset"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type RawACPOutput struct {
	StreamID string       `json:"stream_id"`
	Channel  string       `json:"channel"`
	Offset   uint64       `json:"offset"`
	Next     uint64       `json:"next"`
	Window   RawACPWindow `json:"window"`
	Data     []byte       `json:"data"`
}
