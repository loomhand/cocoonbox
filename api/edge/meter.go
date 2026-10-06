package edge

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	methodToolsCall = "tools/call"
	maxMeteredBody  = 64 << 20
)

var (
	meteredTools = []string{"script_run"}

	// lenient matches the agent's parser, so a request the agent runs is never skipped by the meter.
	lenient = json.JoinOptions(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
)

// Meter appends one JSON line per metered box tool call that passes the edge.
type Meter struct {
	mu   sync.Mutex
	file *os.File
}

// OpenMeter opens the meter file at path for appending, creating it owner-only.
func OpenMeter(path string) (*Meter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("open meter %s: %w", path, err)
	}
	return &Meter{file: f}, nil
}

func (m *Meter) record(ctx context.Context, rt route, box string, calls []rpcCall, reply []byte, took time.Duration) {
	replies := repliesByID(reply)
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var lines []byte
	for _, c := range calls {
		r, answered := replies[string(c.ID)]
		row := meterRow{
			TS:         ts,
			UID:        rt.caller,
			Machine:    box,
			Tool:       c.Params.Name,
			Primitives: []string{},
			MS:         took.Milliseconds(),
			OK:         answered && r.ok(),
			BytesOut:   len(r.raw),
			GWUser:     rt.owner,
		}
		line, err := json.Marshal(row)
		if err != nil {
			log.WithFunc("edge.Meter.record").Error(ctx, err, "encode meter row")
			continue
		}
		lines = append(append(lines, line...), '\n')
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.file.Write(lines); err != nil {
		log.WithFunc("edge.Meter.record").Errorf(ctx, err, "append %d meter rows", len(calls))
	}
}

// meterRow is the per-call meter of vendor-cua plus the gw user the box's model calls bill to.
type meterRow struct {
	TS         string   `json:"ts"`
	UID        string   `json:"uid"`
	Machine    string   `json:"machine"`
	Tool       string   `json:"tool"`
	Primitives []string `json:"primitives"`
	MS         int64    `json:"ms"`
	OK         bool     `json:"ok"`
	BytesOut   int      `json:"bytes_out"`
	GWUser     string   `json:"gw_user"`
}

type rpcCall struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
}

type rpcReply struct {
	raw    jsontext.Value
	ID     jsontext.Value `json:"id"`
	Error  jsontext.Value `json:"error"`
	Result *struct {
		IsError bool `json:"isError"`
	} `json:"result"`
}

func (r rpcReply) ok() bool {
	return len(r.Error) == 0 && r.Result != nil && !r.Result.IsError
}

// captureWriter keeps a copy of the response body of a metered exchange.
type captureWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// meteredCalls returns the metered tool calls of a JSON-RPC message or batch.
func meteredCalls(body []byte) []rpcCall {
	var calls []rpcCall
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		_ = json.Unmarshal(trimmed, &calls, lenient)
	} else {
		var c rpcCall
		if json.Unmarshal(trimmed, &c, lenient) == nil {
			calls = []rpcCall{c}
		}
	}
	return slices.DeleteFunc(calls, func(c rpcCall) bool {
		return c.Method != methodToolsCall || !slices.Contains(meteredTools, c.Params.Name)
	})
}

func repliesByID(body []byte) map[string]rpcReply {
	var raws []jsontext.Value
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		_ = json.Unmarshal(trimmed, &raws, lenient)
	} else {
		raws = []jsontext.Value{trimmed}
	}
	replies := make(map[string]rpcReply, len(raws))
	for _, raw := range raws {
		var r rpcReply
		if json.Unmarshal(raw, &r, lenient) == nil {
			r.raw = raw
			replies[string(r.ID)] = r
		}
	}
	return replies
}
