//go:build unix

package mcp

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestDecodeEnvelope table-tests every frame shape decodeEnvelope must
// classify or reject: a normal response, an error response, a
// notification, and the malformed shapes acceptance requires be caught —
// invalid JSON, a bare JSON scalar/array, a wrong or missing "jsonrpc"
// field, a frame carrying both a method and a result/error, and a frame
// with an id but no method/result/error at all.
func TestDecodeEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(t *testing.T, env wireEnvelope)
	}{
		{
			name: "response with result",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
			check: func(t *testing.T, env wireEnvelope) {
				if env.isNotification() || env.isPeerRequest() {
					t.Fatalf("classified as notification/peer request: %+v", env)
				}
				if !env.matchID(1) {
					t.Fatalf("matchID(1) = false, want true")
				}
				if env.Error != nil {
					t.Fatalf("Error = %+v, want nil", env.Error)
				}
			},
		},
		{
			name: "response with error object",
			raw:  `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"Method not found"}}`,
			check: func(t *testing.T, env wireEnvelope) {
				if env.Error == nil {
					t.Fatal("Error = nil, want an rpcErrorObject")
				}
				if env.Error.Code != -32601 || env.Error.Message != "Method not found" {
					t.Errorf("Error = %+v, want code -32601", env.Error)
				}
				if !env.matchID(2) {
					t.Fatalf("matchID(2) = false, want true")
				}
			},
		},
		{
			name: "notification has no id",
			raw:  `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`,
			check: func(t *testing.T, env wireEnvelope) {
				if !env.isNotification() {
					t.Fatal("isNotification() = false, want true")
				}
				if env.matchID(0) {
					t.Fatal("matchID(0) = true for a notification, want false")
				}
			},
		},
		{
			name: "id null is treated like no id",
			raw:  `{"jsonrpc":"2.0","id":null,"method":"notifications/message"}`,
			check: func(t *testing.T, env wireEnvelope) {
				if !env.isNotification() {
					t.Fatal("isNotification() = false for id:null, want true")
				}
			},
		},
		{
			name:    "invalid JSON syntax",
			raw:     `{"jsonrpc":"2.0","id":1,`,
			wantErr: true,
		},
		{
			name:    "bare JSON string is not a message",
			raw:     `"hello"`,
			wantErr: true,
		},
		{
			name:    "bare JSON array is not a message",
			raw:     `[1,2,3]`,
			wantErr: true,
		},
		{
			name:    "bare JSON number is not a message",
			raw:     `42`,
			wantErr: true,
		},
		{
			name:    "literal null decodes but fails the version check",
			raw:     `null`,
			wantErr: true,
		},
		{
			name:    "wrong jsonrpc version",
			raw:     `{"jsonrpc":"1.0","id":1,"result":{}}`,
			wantErr: true,
		},
		{
			name:    "missing jsonrpc field",
			raw:     `{"id":1,"result":{}}`,
			wantErr: true,
		},
		{
			name:    "both result and error present",
			raw:     `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`,
			wantErr: true,
		},
		{
			name:    "both method and result present",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","result":{}}`,
			wantErr: true,
		},
		{
			name:    "id present with no method, result, or error",
			raw:     `{"jsonrpc":"2.0","id":1}`,
			wantErr: true,
		},
		{
			name:    "neither id nor method present",
			raw:     `{"jsonrpc":"2.0","foo":"bar"}`,
			wantErr: true,
		},
		{
			name: "peer request (id and method both present) is recognized, not rejected",
			raw:  `{"jsonrpc":"2.0","id":5,"method":"roots/list"}`,
			check: func(t *testing.T, env wireEnvelope) {
				if !env.isPeerRequest() {
					t.Fatal("isPeerRequest() = false, want true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := decodeEnvelope([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeEnvelope(%s) = %+v, want error", tt.raw, env)
				}
				if !errors.Is(err, ErrMalformedFrame) {
					t.Errorf("decodeEnvelope(%s) error = %v, want it to wrap ErrMalformedFrame", tt.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeEnvelope(%s) returned error: %v", tt.raw, err)
			}
			if tt.check != nil {
				tt.check(t, env)
			}
		})
	}
}

// TestMatchID_UnrecognizedShapes proves an id this client would never
// itself send (a string, a float) is reported as not matching rather than
// panicking or matching by accident.
func TestMatchID_UnrecognizedShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{"string id", `{"jsonrpc":"2.0","id":"1","result":{}}`},
		{"float id", `{"jsonrpc":"2.0","id":1.5,"result":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, err := decodeEnvelope([]byte(tt.raw))
			if err != nil {
				t.Fatalf("decodeEnvelope: %v", err)
			}
			if env.matchID(1) {
				t.Errorf("matchID(1) = true for %s, want false", tt.raw)
			}
		})
	}
}

func TestRPCError(t *testing.T) {
	t.Parallel()

	err := &RPCError{Code: -32602, Message: "Invalid params"}
	if !errors.Is(err, ErrServerError) {
		t.Errorf("errors.Is(err, ErrServerError) = false, want true")
	}
	if got := err.Error(); got == "" {
		t.Errorf("Error() is empty")
	}

	withData := &RPCError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`{"field":"projectKey"}`)}
	if got := withData.Error(); got == err.Error() {
		t.Errorf("Error() with Data present should differ from without, both got %q", got)
	}

	// An RPCError must never satisfy errors.Is for a plain transport
	// sentinel: the two failure classes are documented as distinct.
	if errors.Is(err, ErrTransportClosed) {
		t.Error("an *RPCError must not satisfy errors.Is(err, ErrTransportClosed)")
	}
}

func TestNonNilSlice(t *testing.T) {
	t.Parallel()

	if got := nonNilSlice[int](nil); got == nil {
		t.Error("nonNilSlice(nil) = nil, want a non-nil empty slice")
	} else if len(got) != 0 {
		t.Errorf("nonNilSlice(nil) = %v, want empty", got)
	}

	in := []int{1, 2, 3}
	got := nonNilSlice(in)
	if len(got) != 3 || got[0] != 1 {
		t.Errorf("nonNilSlice(%v) = %v, want unchanged", in, got)
	}

	// A non-nil, empty input must round-trip as non-nil, not collapse to
	// nil the way append([]T(nil), src...) would.
	emptyNonNil := []int{}
	if got := nonNilSlice(emptyNonNil); got == nil {
		t.Error("nonNilSlice([]int{}) = nil, want non-nil empty slice preserved")
	}
}
