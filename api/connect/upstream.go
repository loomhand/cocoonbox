package connect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	jsonRPCVersion   = "2.0"
	protocolVersion  = "2025-11-25"
	headerSession    = "Mcp-Session-Id"
	headerProtocol   = "MCP-Protocol-Version"
	maxUpstreamBody  = 8 << 20
	maxToolPages     = 20
	maxErrorExcerpt  = 300
	clientName       = "cocoonbox-connect"
	eventStreamMedia = "text/event-stream"
)

var (
	errUnauthorized = errors.New("the MCP server refused the credential")
	errSessionGone  = errors.New("the MCP server ended the session")
)

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitzero"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitzero"`
}

type rpcResponse struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Result jsontext.Value `json:"result"`
	Error  *rpcError      `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

func (e *rpcError) code() int {
	if e == nil {
		return 0
	}
	return e.Code
}

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return strings.TrimSuffix("the MCP server answered HTTP "+strconv.Itoa(e.status)+": "+e.body, ": ")
}

type exchange struct {
	status    int
	reqBytes  int64
	respBytes int64
}

type conn struct {
	mu      sync.Mutex
	session string
	version string
	tools   []tool
	toolsAt time.Time
}

type credential struct {
	header string
	value  string
}

type upstream struct {
	s    *Service
	url  string
	cred credential
	conn *conn
	ids  atomic.Int64
}

// call opens a new session once when the server ended the old one.
func (u *upstream) call(ctx context.Context, method string, params any) (jsontext.Value, exchange, error) {
	session, version, err := u.session(ctx, "")
	if err != nil {
		return nil, exchange{}, err
	}
	result, ex, err := u.post(ctx, session, version, rpcRequest{JSONRPC: jsonRPCVersion, ID: u.ids.Add(1), Method: method, Params: params})
	if !errors.Is(err, errSessionGone) {
		return result, ex, err
	}
	if session, version, err = u.session(ctx, session); err != nil {
		return nil, exchange{}, err
	}
	return u.post(ctx, session, version, rpcRequest{JSONRPC: jsonRPCVersion, ID: u.ids.Add(1), Method: method, Params: params})
}

func (u *upstream) session(ctx context.Context, gone string) (string, string, error) {
	u.conn.mu.Lock()
	defer u.conn.mu.Unlock()
	if u.conn.version != "" && u.conn.session != gone {
		return u.conn.session, u.conn.version, nil
	}
	result, session, _, err := u.send(ctx, "", "", rpcRequest{JSONRPC: jsonRPCVersion, ID: u.ids.Add(1), Method: methodInitialize, Params: initializeParams()})
	if err != nil {
		return "", "", err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &init); err != nil || init.ProtocolVersion == "" {
		return "", "", errors.New("the MCP server sent no protocol version")
	}
	if _, _, _, err := u.send(ctx, session, init.ProtocolVersion, rpcRequest{JSONRPC: jsonRPCVersion, Method: "notifications/initialized"}); err != nil {
		return "", "", err
	}
	u.conn.session, u.conn.version = session, init.ProtocolVersion
	return session, init.ProtocolVersion, nil
}

func (u *upstream) post(ctx context.Context, session, version string, msg rpcRequest) (jsontext.Value, exchange, error) {
	result, _, ex, err := u.send(ctx, session, version, msg)
	if errors.Is(err, errSessionGone) {
		u.conn.mu.Lock()
		if u.conn.session == session {
			u.conn.version = ""
		}
		u.conn.mu.Unlock()
	}
	return result, ex, err
}

func (u *upstream) send(ctx context.Context, session, version string, msg rpcRequest) (result jsontext.Value, newSession string, ex exchange, err error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, "", ex, err
	}
	ex.reqBytes = int64(len(body))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", ex, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, "+eventStreamMedia)
	if u.cred.header != "" {
		req.Header.Set(u.cred.header, u.cred.value)
	}
	if session != "" {
		req.Header.Set(headerSession, session)
	}
	if version != "" {
		req.Header.Set(headerProtocol, version)
	}
	resp, err := u.s.http.Do(req)
	if err != nil {
		return nil, "", ex, err
	}
	defer func() { _ = resp.Body.Close() }()
	ex.status = resp.StatusCode
	counted := &countingReader{r: io.LimitReader(resp.Body, maxUpstreamBody)}
	defer func() { ex.respBytes = counted.n }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, "", ex, errUnauthorized
	case resp.StatusCode == http.StatusNotFound && session != "":
		return nil, "", ex, errSessionGone
	case resp.StatusCode/100 != 2:
		excerpt, _ := io.ReadAll(io.LimitReader(counted, maxErrorExcerpt))
		_, _ = io.Copy(io.Discard, counted)
		return nil, "", ex, &statusError{status: resp.StatusCode, body: strings.Join(strings.Fields(string(excerpt)), " ")}
	case msg.ID == 0:
		_, _ = io.Copy(io.Discard, counted)
		return nil, "", ex, nil
	}
	newSession = resp.Header.Get(headerSession)
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	answer := &rpcResponse{}
	if media == eventStreamMedia {
		answer, err = readEvents(counted, msg.ID)
	} else {
		err = json.UnmarshalRead(counted, answer)
	}
	switch {
	case err != nil:
		return nil, newSession, ex, fmt.Errorf("read the MCP server's answer: %w", err)
	case answer.Error != nil:
		return nil, newSession, ex, answer.Error
	}
	return answer.Result, newSession, ex, nil
}

func (u *upstream) listTools(ctx context.Context) ([]tool, error) {
	var tools []tool
	cursor := ""
	for range maxToolPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, _, err := u.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []jsontext.Value `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, fmt.Errorf("decode tools/list: %w", err)
		}
		for _, raw := range page.Tools {
			if t, err := parseTool(raw); err == nil {
				tools = append(tools, t)
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	return tools, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func readEvents(r io.Reader, id int64) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxUpstreamBody)
	want := strconv.FormatInt(id, 10)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		var msg rpcResponse
		if json.Unmarshal([]byte(data.String()), &msg) == nil && msg.Method == "" && string(msg.ID) == want {
			return &msg, nil
		}
		data.Reset()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the event stream ended without a response")
}

func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      implementation{Name: clientName, Version: "1"},
	}
}
