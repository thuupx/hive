package acp

import (
	"encoding/json"
	"fmt"
)

const jsonrpcVersion = "2.0"

// Standard JSON-RPC error codes used by this adapter.
const (
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// rpcMessage is a JSON-RPC 2.0 message.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// encode marshals a local wire type. Every value passed to it is a type
// defined in this package, so encoding cannot fail.
func encode(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("acp: encode %T: %v", v, err))
	}
	return b
}
