// Package canonical defines the normalized request/response types used
// across ingress, routing, execution and the ledger.
package canonical

// Part is a single content unit. v0 supports text; image/tool payloads are
// carried opaquely so filters can detect vision/tools without full parsing.
type Part struct {
	Text     string
	ImageURL bool
	ToolCall bool
}

// Message is one chat message.
type Message struct {
	Role    string
	Content []Part
	RawLen  int // raw character length, used for token estimation
}

// Request is the normalized chat request the router and executor operate on.
type Request struct {
	ID          string
	Tenant      string
	KeyID       string
	Requested   string // "auto", "auto:cheap", "policy:x", "mock/fast", ...
	Messages    []Message
	Tools       bool
	Vision      bool
	JSONMode    bool
	Stream      bool
	MaxOutput   int
	InTokensEst int
	Task        string
	TaskConf    float64
	Metadata    map[string]string
	Raw         []byte // original body for zero-copy passthrough
}

// Usage mirrors OpenAI usage.
type Usage struct {
	In       int
	Out      int
	CachedIn int
}

// Chunk is one streamed delta.
type Chunk struct {
	Delta        string
	FinishReason string
	Usage        *Usage
}
