package connect

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/loomhand/cocoonbox/api/mcpwire"
)

const (
	builtinGoogleTasks = "builtin:google-tasks"
	tasksAPI           = "https://tasks.googleapis.com/tasks/v1"
	tasksDone          = "done"

	argTitle      = "title"
	argNotes      = "notes"
	argDue        = "due"
	argStatus     = "status"
	argMaxResults = "maxResults"
	argPageToken  = "pageToken"
	typeString    = "string"
)

var (
	tasksProperties = map[string]tasksProperty{
		"tasklist":      {Type: typeString, Description: "The task list id."},
		"task":          {Type: typeString, Description: "The task id."},
		argTitle:        {Type: typeString, Description: "The title."},
		argNotes:        {Type: typeString, Description: "The task's notes."},
		argDue:          {Type: typeString, Description: "The due date as an RFC 3339 timestamp; Google keeps only the date."},
		argStatus:       {Type: typeString, Enum: []string{"needsAction", "completed"}, Description: "needsAction or completed."},
		"parent":        {Type: typeString, Description: "The parent task id; omit it for a top-level task."},
		"previous":      {Type: typeString, Description: "The sibling task id to place the task after; omit it to place the task first."},
		argMaxResults:   {Type: "integer", Description: "The page size."},
		argPageToken:    {Type: typeString, Description: "The nextPageToken of the previous page."},
		"showCompleted": {Type: "boolean", Description: "Whether completed tasks are listed; the default is true."},
		"showHidden":    {Type: "boolean", Description: "Whether hidden tasks are listed; the default is false."},
		"dueMin":        {Type: typeString, Description: "The lower bound of the due date, as an RFC 3339 timestamp."},
		"dueMax":        {Type: typeString, Description: "The upper bound of the due date, as an RFC 3339 timestamp."},
		"updatedMin":    {Type: typeString, Description: "The lower bound of the last change, as an RFC 3339 timestamp."},
	}

	tasksTools = []tasksTool{
		{
			name: "list_tasklists", description: "Lists your task lists.", readOnly: true,
			method: http.MethodGet, path: "users/@me/lists", query: []string{argMaxResults, argPageToken},
		},
		{
			name: "insert_tasklist", description: "Creates a task list.",
			method: http.MethodPost, path: "users/@me/lists", body: []string{argTitle}, required: []string{argTitle},
		},
		{
			name: "patch_tasklist", description: "Renames a task list.",
			method: http.MethodPatch, path: "users/@me/lists/{tasklist}", body: []string{argTitle}, required: []string{argTitle},
		},
		{
			name: "delete_tasklist", description: "Deletes a task list and its tasks.", destructive: true,
			method: http.MethodDelete, path: "users/@me/lists/{tasklist}",
		},
		{
			name: "list_tasks", description: "Lists the tasks of a task list.", readOnly: true,
			method: http.MethodGet, path: "lists/{tasklist}/tasks",
			query: []string{argMaxResults, argPageToken, "showCompleted", "showHidden", "dueMin", "dueMax", "updatedMin"},
		},
		{
			name: "insert_task", description: "Creates a task in a task list.",
			method: http.MethodPost, path: "lists/{tasklist}/tasks", query: []string{"parent", "previous"},
			body: []string{argTitle, argNotes, argDue, argStatus}, required: []string{argTitle},
		},
		{
			name: "patch_task", description: "Changes the fields you give of a task; status completed marks it done.",
			method: http.MethodPatch, path: "lists/{tasklist}/tasks/{task}", body: []string{argTitle, argNotes, argDue, argStatus},
		},
		{
			name: "delete_task", description: "Deletes a task.", destructive: true,
			method: http.MethodDelete, path: "lists/{tasklist}/tasks/{task}",
		},
	}
)

type tasksProperty struct {
	Type        string   `json:"type"`
	Enum        []string `json:"enum,omitzero"`
	Description string   `json:"description"`
}

// tasksTool is one Google Tasks REST call; path names its arguments in braces.
type tasksTool struct {
	name        string
	description string
	readOnly    bool
	destructive bool
	method      string
	path        string
	query       []string
	body        []string
	required    []string
}

func (t tasksTool) params() []string {
	var params []string
	for seg := range strings.SplitSeq(t.path, "/") {
		if name, ok := strings.CutPrefix(seg, "{"); ok {
			params = append(params, strings.TrimSuffix(name, "}"))
		}
	}
	return params
}

func (t tasksTool) definition() map[string]any {
	properties := map[string]tasksProperty{}
	for _, name := range slices.Concat(t.params(), t.query, t.body) {
		properties[name] = tasksProperties[name]
	}
	return map[string]any{
		"name": t.name, "description": t.description,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": slices.Concat(t.params(), t.required), "additionalProperties": false},
		"annotations": map[string]any{"readOnlyHint": t.readOnly, "destructiveHint": t.destructive},
	}
}

func (t tasksTool) request(ctx context.Context, base string, args map[string]any) (*http.Request, int64, error) {
	segs := strings.Split(t.path, "/")
	for i, seg := range segs {
		name, ok := strings.CutPrefix(seg, "{")
		if !ok {
			continue
		}
		name = strings.TrimSuffix(name, "}")
		v, _ := args[name].(string)
		if v == "" {
			return nil, 0, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: name + " is required"}
		}
		segs[i] = url.PathEscape(v)
	}
	q := url.Values{}
	for _, name := range t.query {
		if v, ok := args[name]; ok {
			q.Set(name, fmt.Sprint(v))
		}
	}
	target := base + "/" + strings.Join(segs, "/")
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var body []byte
	if len(t.body) > 0 {
		fields := map[string]any{}
		for _, name := range t.body {
			if v, ok := args[name]; ok {
				fields[name] = v
			}
		}
		for _, name := range t.required {
			if _, ok := fields[name]; !ok {
				return nil, 0, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: name + " is required"}
			}
		}
		var err error
		if body, err = json.Marshal(fields, json.Deterministic(true)); err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, t.method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, int64(len(body)), nil
}

// googleTasks answers msg as the MCP server of Google Tasks, calling its REST API with authorization.
func (s *Service) googleTasks(ctx context.Context, authorization string, msg rpcRequest) (jsontext.Value, exchange, error) {
	ex := exchange{status: http.StatusOK}
	switch msg.Method {
	case methodInitialize:
		result, err := json.Marshal(struct {
			ProtocolVersion string         `json:"protocolVersion"`
			ServerInfo      implementation `json:"serverInfo"`
		}{mcpwire.Latest, implementation{Name: "google-tasks", Version: "1"}})
		return result, ex, err
	case "tools/list":
		defs := make([]map[string]any, 0, len(tasksTools))
		for _, t := range tasksTools {
			defs = append(defs, t.definition())
		}
		result, err := json.Marshal(struct {
			Tools []map[string]any `json:"tools"`
		}{defs})
		return result, ex, err
	case "tools/call":
		call, _ := msg.Params.(toolCall)
		return s.callGoogleTasks(ctx, authorization, call)
	case "notifications/initialized":
		return nil, ex, nil
	}
	return nil, ex, &mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "unknown method " + msg.Method}
}

func (s *Service) callGoogleTasks(ctx context.Context, authorization string, call toolCall) (jsontext.Value, exchange, error) {
	ex := exchange{status: http.StatusOK}
	i := slices.IndexFunc(tasksTools, func(t tasksTool) bool { return t.name == call.Name })
	if i < 0 {
		return nil, ex, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "unknown tool: " + call.Name}
	}
	args := map[string]any{}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return nil, ex, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "arguments must be a JSON object"}
	}
	req, reqBytes, err := tasksTools[i].request(ctx, s.tasksAPI, args)
	if err != nil {
		return nil, ex, err
	}
	req.Header.Set(headerAuthorization, authorization)
	ex = exchange{reqBytes: reqBytes}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, ex, err
	}
	defer func() { _ = resp.Body.Close() }()
	ex.status = resp.StatusCode
	counted := &countingReader{r: io.LimitReader(resp.Body, maxUpstreamBody)}
	body, err := io.ReadAll(counted)
	ex.respBytes = counted.n
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ex, errUnauthorized
	case resp.StatusCode/100 == 5:
		return nil, ex, &statusError{status: resp.StatusCode, body: shorten(strings.Join(strings.Fields(string(body)), " "), maxErrorExcerpt)}
	case err != nil:
		return nil, ex, fmt.Errorf("read the Google Tasks answer: %w", err)
	}
	text := strings.TrimSpace(string(body))
	result := callResult{Content: []textContent{{Type: "text", Text: cmp.Or(text, tasksDone)}}}
	if resp.StatusCode/100 != 2 {
		result = *errorText(fmt.Sprintf("Google Tasks answered HTTP %d: %s", resp.StatusCode, shorten(strings.Join(strings.Fields(text), " "), maxErrorExcerpt)))
	}
	out, err := json.Marshal(result)
	return out, ex, err
}
