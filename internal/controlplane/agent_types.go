package controlplane

import "time"

// AgentRequest is one registered cooperative action. Authority comes from the
// authenticated host assignment, never from request-selected model or identity.
type AgentRequest struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Step          int64  `json:"step"`
	Position      int32  `json:"position"`
	Name          string `json:"name,omitempty"`
	Payload       []byte `json:"payload"`
	PayloadDigest string `json:"payload_digest"`
	SessionHash   string `json:"session_hash"`
}
type AgentResult struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	Payload       []byte `json:"payload,omitempty"`
	EventSequence int64  `json:"event_sequence"`
	ErrorCode     string `json:"error_code,omitempty"`
	Replayed      bool   `json:"replayed"`
}
type AgentBarrier struct {
	Step            int64  `json:"step"`
	EventSequence   int64  `json:"event_sequence"`
	StateDigest     string `json:"state_digest"`
	SessionHash     string `json:"session_hash"`
	ProtocolVersion string `json:"protocol_version"`
}
type AgentPoll struct {
	SessionAck string
	Request    *AgentRequest
	Barrier    *AgentBarrier
}
type AgentDelivery struct {
	SessionHash   string
	RequestHash   string
	SessionAck    string
	Result        *AgentResult
	SealedBarrier *AgentBarrier
}
type AgentCall struct {
	RegisteredSequence int64        `json:"registered_sequence"`
	Request            AgentRequest `json:"request"`
	Result             AgentResult  `json:"result"`
	ExecutionID        string       `json:"execution_id"`
	Epoch              int64        `json:"epoch"`
	ModelDigest        string       `json:"model_digest"`
	RouteDigest        string       `json:"route_digest"`
	AdapterVersion     string       `json:"adapter_version"`
	Mutating           bool         `json:"mutating"`
	CreatedAt          time.Time    `json:"created_at"`
}
type AgentStep struct {
	BranchID      string `json:"branch_id"`
	Step          int64  `json:"step"`
	StateDigest   string `json:"state_digest"`
	EventSequence int64  `json:"event_sequence"`
	SessionHash   string `json:"session_hash"`
	Complete      bool   `json:"complete"`
}
type ForkResult struct {
	Operation Resource   `json:"operation"`
	Children  []Resource `json:"children"`
}
type Divergence struct {
	Comparator  string `json:"comparator"`
	State       string `json:"state"`
	Step        int64  `json:"step,omitempty"`
	Position    int32  `json:"position,omitempty"`
	LeftDigest  string `json:"left_digest,omitempty"`
	RightDigest string `json:"right_digest,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
