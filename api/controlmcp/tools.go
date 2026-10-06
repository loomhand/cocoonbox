package controlmcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	stateAbsent = "absent"
	boxProperty = `{"type":"string","description":"The box's name or id; without it, your first box."}`

	errUpgradeUnconfirmed toolError = "Upgrading restarts running programs; files, logins and agent history are kept. Call again with confirm: true."
	errUpgradeCurrent     toolError = "Already on the latest desktop."
	errDeleteUnconfirmed  toolError = "Deleting removes the box and stops its running programs; its last saved state stays in storage. Call again with confirm: true."
	errDeleteUnnamed      toolError = "Name the box to delete in box."
	errUnknownKind        toolError = "kind must be general or script."
)

var (
	// kindFamilies maps a box_create kind to the image family the api resolves; general keeps the default image.
	kindFamilies = map[string]string{"general": "", "script": "cbplan"}

	toolSpecs = []toolSpec{
		{
			Name:         "box_list",
			Description:  "List your cocoonbox cloud desktops with their names, states and images.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"boxes":{"type":"array","items":{"type":"object","properties":{"box_id":{"type":"string"},"name":{"type":"string"},"state":{"type":"string"},"image":{"type":"string"},"last_saved_at_ms":{"type":"integer"}},"required":["box_id","name","state","image","last_saved_at_ms"]}}},"required":["boxes"]}`),
		},
		{
			Name:         "box_status",
			Description:  "Show the state of your cocoonbox cloud desktop: running or hibernated, its image, whether an upgrade is available, and when your files were last saved.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"name":{"type":"string"},"state":{"type":"string"},"image":{"type":"string"},"latest_image":{"type":"string"},"upgrade_available":{"type":"boolean"},"last_saved_at_ms":{"type":"integer"}},"required":["box_id","name","state","image","latest_image","upgrade_available","last_saved_at_ms"]}`),
		},
		{
			Name:         "box_ensure",
			Description:  "Start or wake your cocoonbox cloud desktop and wait until it is running. Waking takes a few seconds; creating a new box takes longer.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `,"wait":{"type":"boolean","default":true}},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"state":{"type":"string"},"operation_id":{"type":"string"}},"required":["box_id","state","operation_id"]}`),
		},
		{
			Name:         "box_create",
			Description:  "Create another cocoonbox cloud desktop with this name and wait until it is running. Your other boxes stay as they are; only a limited number may run at once. kind general (the default) is the standard desktop; kind script also has the Script runtime, so that box adds the script_run tool.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"kind":{"type":"string","enum":["general","script"],"default":"general"},"wait":{"type":"boolean","default":true}},"required":["name"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"name":{"type":"string"},"state":{"type":"string"},"operation_id":{"type":"string"}},"required":["box_id","name","state","operation_id"]}`),
		},
		{
			Name:         "box_hibernate",
			Description:  "Hibernate your cocoonbox cloud desktop. Running programs and memory are kept and come back on the next wake.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"state":{"type":"string"},"operation_id":{"type":"string"}},"required":["box_id","state","operation_id"]}`),
		},
		{
			Name:         "box_save",
			Description:  "Save your files, logins and agent history from the box to storage now. The browser in the box closes during the save.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"last_saved_at_ms":{"type":"integer"},"operation_id":{"type":"string"},"bytes":{"type":"integer"}},"required":["box_id","last_saved_at_ms","operation_id","bytes"]}`),
		},
		{
			Name:         "box_upgrade",
			Description:  "Move your box to the latest desktop image. Running programs restart; files, logins and agent history are kept. Requires confirm: true.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `,"confirm":{"type":"boolean"}},"required":["confirm"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"state":{"type":"string"},"image":{"type":"string"},"operation_id":{"type":"string"}},"required":["box_id","state","image","operation_id"]}`),
		},
		{
			Name:         "box_delete",
			Description:  "Delete one of your cocoonbox cloud desktops. A running box is saved first; its saved state stays in storage. Requires the box and confirm: true.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"box":` + boxProperty + `,"confirm":{"type":"boolean"}},"required":["box","confirm"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"box_id":{"type":"string"},"operation_id":{"type":"string"}},"required":["box_id","operation_id"]}`),
		},
	}
)

type (
	progressFunc func(message string)
	toolFunc     func() (any, error)
)

// toolError is a refusal the model reads as an isError result.
type toolError string

func (e toolError) Error() string { return string(e) }

type toolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type toolResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type statusResult struct {
	BoxID            string `json:"box_id"`
	Name             string `json:"name"`
	State            string `json:"state"`
	Image            string `json:"image"`
	LatestImage      string `json:"latest_image"`
	UpgradeAvailable bool   `json:"upgrade_available"`
	LastSavedAtMs    int64  `json:"last_saved_at_ms"`
}

type boxArgs struct {
	Box string `json:"box"`
}

type confirmArgs struct {
	Box     string `json:"box"`
	Confirm bool   `json:"confirm"`
}

type listResult struct {
	Boxes []listEntry `json:"boxes"`
}

type listEntry struct {
	BoxID         string `json:"box_id"`
	Name          string `json:"name"`
	State         string `json:"state"`
	Image         string `json:"image"`
	LastSavedAtMs int64  `json:"last_saved_at_ms"`
}

type createResult struct {
	BoxID       string `json:"box_id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	OperationID string `json:"operation_id"`
}

type deleteResult struct {
	BoxID       string `json:"box_id"`
	OperationID string `json:"operation_id"`
}

type operationResult struct {
	BoxID       string `json:"box_id"`
	State       string `json:"state"`
	OperationID string `json:"operation_id"`
}

type saveResult struct {
	BoxID         string `json:"box_id"`
	LastSavedAtMs int64  `json:"last_saved_at_ms"`
	OperationID   string `json:"operation_id"`
	Bytes         uint64 `json:"bytes"`
}

type upgradeResult struct {
	BoxID       string `json:"box_id"`
	State       string `json:"state"`
	Image       string `json:"image"`
	OperationID string `json:"operation_id"`
}

// call runs one tool as the user in ctx; progress receives each event of the operation it waits on.
func (s *Server) call(ctx context.Context, call *callParams, progress progressFunc) (*toolResult, *rpcError) {
	switch call.Name {
	case "box_list":
		return withArguments(call.Arguments, &struct{}{}, func() (any, error) { return s.list(ctx) })
	case "box_status":
		var args boxArgs
		return withArguments(call.Arguments, &args, func() (any, error) { return s.status(ctx, args.Box) })
	case "box_ensure":
		var args struct {
			Box  string `json:"box"`
			Wait *bool  `json:"wait"`
		}
		return withArguments(call.Arguments, &args, func() (any, error) {
			return s.ensure(ctx, args.Box, args.Wait == nil || *args.Wait, progress)
		})
	case "box_create":
		var args struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			Wait *bool  `json:"wait"`
		}
		return withArguments(call.Arguments, &args, func() (any, error) {
			family, ok := kindFamilies[cmp.Or(args.Kind, "general")]
			if !ok {
				return nil, errUnknownKind
			}
			return s.create(ctx, args.Name, family, args.Wait == nil || *args.Wait, progress)
		})
	case "box_hibernate":
		var args boxArgs
		return withArguments(call.Arguments, &args, func() (any, error) { return s.hibernate(ctx, args.Box, progress) })
	case "box_save":
		var args boxArgs
		return withArguments(call.Arguments, &args, func() (any, error) { return s.save(ctx, args.Box, progress) })
	case "box_upgrade":
		var args confirmArgs
		return withArguments(call.Arguments, &args, func() (any, error) {
			if !args.Confirm {
				return nil, errUpgradeUnconfirmed
			}
			return s.upgrade(ctx, args.Box, progress)
		})
	case "box_delete":
		var args confirmArgs
		return withArguments(call.Arguments, &args, func() (any, error) {
			if !args.Confirm {
				return nil, errDeleteUnconfirmed
			}
			return s.delete(ctx, args.Box, progress)
		})
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + call.Name}
	}
}

func (s *Server) list(ctx context.Context) (*listResult, error) {
	resp, err := s.boxes.List(ctx, &controlv1.ListBoxesRequest{})
	if err != nil {
		return nil, err
	}
	out := &listResult{Boxes: []listEntry{}}
	for _, b := range resp.GetBoxes() {
		out.Boxes = append(out.Boxes, listEntry{BoxID: b.GetId(), Name: b.GetName(), State: stateName(b.GetState()), Image: b.GetImage(), LastSavedAtMs: b.GetLastSavedAtMs()})
	}
	return out, nil
}

func (s *Server) status(ctx context.Context, ref string) (*statusResult, error) {
	box, err := s.box(ctx, ref)
	if err != nil || box == nil {
		return &statusResult{State: stateAbsent}, err
	}
	return &statusResult{
		BoxID:            box.GetId(),
		Name:             box.GetName(),
		State:            stateName(box.GetState()),
		Image:            box.GetImage(),
		LatestImage:      box.GetLatestImage(),
		UpgradeAvailable: box.GetImage() != "" && box.GetLatestImage() != "" && box.GetImage() != box.GetLatestImage(),
		LastSavedAtMs:    box.GetLastSavedAtMs(),
	}, nil
}

func (s *Server) ensure(ctx context.Context, ref string, wait bool, progress progressFunc) (*operationResult, error) {
	req := &controlv1.EnsureRequest{}
	if ref != "" {
		box, err := s.box(ctx, ref)
		if err != nil {
			return nil, err
		}
		req.BoxId = box.GetId()
	}
	resp, err := s.boxes.Ensure(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &operationResult{BoxID: resp.GetBox().GetId(), State: stateName(resp.GetBox().GetState()), OperationID: resp.GetOperation().GetId()}
	if res.OperationID == "" || !wait {
		return res, nil
	}
	if _, err = s.finish(ctx, "Ensure", res.OperationID, progress); err != nil {
		return nil, err
	}
	box, err := s.box(ctx, res.BoxID)
	res.State = stateName(box.GetState())
	return res, err
}

func (s *Server) create(ctx context.Context, name, family string, wait bool, progress progressFunc) (*createResult, error) {
	resp, err := s.boxes.Create(ctx, &controlv1.CreateBoxRequest{Name: name, Image: family})
	if err != nil {
		return nil, err
	}
	res := &createResult{BoxID: resp.GetBox().GetId(), Name: resp.GetBox().GetName(), State: stateName(resp.GetBox().GetState()), OperationID: resp.GetOperation().GetId()}
	if res.OperationID == "" || !wait {
		return res, nil
	}
	if _, err = s.finish(ctx, "Create", res.OperationID, progress); err != nil {
		return nil, err
	}
	box, err := s.box(ctx, res.BoxID)
	res.State = stateName(box.GetState())
	return res, err
}

func (s *Server) hibernate(ctx context.Context, ref string, progress progressFunc) (*operationResult, error) {
	box, err := s.runningBox(ctx, ref)
	if err != nil {
		return nil, err
	}
	op, err := s.boxes.Hibernate(ctx, &controlv1.BoxRef{BoxId: box.GetId()})
	if err != nil {
		return nil, err
	}
	if _, err = s.finish(ctx, "Hibernate", op.GetId(), progress); err != nil {
		return nil, err
	}
	box, err = s.box(ctx, op.GetBoxId())
	return &operationResult{BoxID: op.GetBoxId(), State: stateName(box.GetState()), OperationID: op.GetId()}, err
}

func (s *Server) save(ctx context.Context, ref string, progress progressFunc) (*saveResult, error) {
	box, err := s.runningBox(ctx, ref)
	if err != nil {
		return nil, err
	}
	op, err := s.boxes.SaveState(ctx, &controlv1.BoxRef{BoxId: box.GetId()})
	if err != nil {
		return nil, err
	}
	w, err := s.finish(ctx, "Save", op.GetId(), progress)
	if err != nil {
		return nil, err
	}
	box, err = s.box(ctx, op.GetBoxId())
	return &saveResult{BoxID: op.GetBoxId(), LastSavedAtMs: box.GetLastSavedAtMs(), OperationID: op.GetId(), Bytes: w.savedBytes}, err
}

func (s *Server) upgrade(ctx context.Context, ref string, progress progressFunc) (*upgradeResult, error) {
	box, err := s.runningBox(ctx, ref)
	if err != nil {
		return nil, err
	}
	if box.GetImage() == box.GetLatestImage() {
		return nil, errUpgradeCurrent
	}
	op, err := s.boxes.Rebase(ctx, &controlv1.RebaseRequest{BoxId: box.GetId()})
	if err != nil {
		return nil, err
	}
	if _, err = s.finish(ctx, "Upgrade", op.GetId(), progress); err != nil {
		return nil, err
	}
	box, err = s.box(ctx, op.GetBoxId())
	return &upgradeResult{BoxID: op.GetBoxId(), State: stateName(box.GetState()), Image: box.GetImage(), OperationID: op.GetId()}, err
}

func (s *Server) delete(ctx context.Context, ref string, progress progressFunc) (*deleteResult, error) {
	if ref == "" {
		return nil, errDeleteUnnamed
	}
	box, err := s.box(ctx, ref)
	if err != nil {
		return nil, err
	}
	op, err := s.boxes.Delete(ctx, &controlv1.BoxRef{BoxId: box.GetId()})
	if err != nil {
		return nil, err
	}
	if _, err = s.finish(ctx, "Delete", op.GetId(), progress); err != nil {
		return nil, err
	}
	return &deleteResult{BoxID: op.GetBoxId(), OperationID: op.GetId()}, nil
}

// finish follows operation id to its end; a failed operation becomes "<verb> failed: <detail>".
func (s *Server) finish(ctx context.Context, verb, id string, progress progressFunc) (*watchStream, error) {
	w := &watchStream{ctx: ctx, progress: progress}
	if err := s.boxes.WatchOperation(&controlv1.WatchOperationRequest{OperationId: id}, w); err != nil {
		return nil, err
	}
	if w.last.GetPhase() == controlv1.Phase_PHASE_FAILED {
		return nil, toolError(verb + " failed: " + w.last.GetDetail())
	}
	return w, nil
}

func (s *Server) runningBox(ctx context.Context, ref string) (*controlv1.Box, error) {
	box, err := s.box(ctx, ref)
	switch {
	case err != nil:
		return nil, err
	case box == nil:
		return nil, toolError("The box is " + stateAbsent + ".")
	case box.GetState() != controlv1.BoxState_BOX_STATE_RUNNING:
		return nil, toolError("The box is " + stateName(box.GetState()) + ".")
	}
	return box, nil
}

// box returns the caller's box named or numbered ref, or the first one when ref is empty, nil when the caller has none.
func (s *Server) box(ctx context.Context, ref string) (*controlv1.Box, error) {
	resp, err := s.boxes.List(ctx, &controlv1.ListBoxesRequest{})
	if err != nil {
		return nil, err
	}
	list := resp.GetBoxes()
	if ref == "" {
		if len(list) == 0 {
			return nil, nil
		}
		return list[0], nil
	}
	for _, b := range list {
		if b.GetId() == ref || b.GetName() == ref {
			return b, nil
		}
	}
	return nil, toolError(fmt.Sprintf("You have no box %q; box_list lists your boxes.", ref))
}

// watchStream receives WatchOperation events in-process as the caller in ctx.
type watchStream struct {
	grpc.ServerStream
	ctx      context.Context
	progress progressFunc

	last       *controlv1.OperationEvent
	savedBytes uint64
}

func (w *watchStream) Context() context.Context { return w.ctx }

func (w *watchStream) Send(ev *controlv1.OperationEvent) error {
	w.last = ev
	message := strings.ToLower(strings.TrimPrefix(ev.GetPhase().String(), "PHASE_")) + ": " + ev.GetDetail()
	if ev.GetTotalBytes() != 0 {
		message += fmt.Sprintf(" (%d/%d bytes)", ev.GetDoneBytes(), ev.GetTotalBytes())
		if ev.GetPhase() == controlv1.Phase_PHASE_SAVING {
			w.savedBytes = ev.GetTotalBytes()
		}
	}
	w.progress(message)
	return nil
}

// withArguments decodes raw into dst, refusing names outside the input schema, and runs fn.
func withArguments(raw json.RawMessage, dst any, fn toolFunc) (*toolResult, *rpcError) {
	if len(raw) != 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid arguments: " + err.Error()}
		}
	}
	out, err := fn()
	var refusal toolError
	switch {
	case errors.As(err, &refusal):
		return errorText(refusal.Error()), nil
	case internal(err):
		return nil, &rpcError{Code: codeInternal, Message: status.Convert(err).Message()}
	case err != nil:
		return errorText(status.Convert(err).Message()), nil
	}
	data, err := marshal(out)
	if err != nil {
		return nil, &rpcError{Code: codeInternal, Message: "encode result"}
	}
	return &toolResult{Content: []textContent{{Type: "text", Text: string(bytes.TrimSpace(data))}}, StructuredContent: out}, nil
}

func errorText(text string) *toolResult {
	return &toolResult{Content: []textContent{{Type: "text", Text: text}}, IsError: true}
}

// internal reports a Boxes failure the model cannot act on.
func internal(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Internal, codes.Unknown, codes.DataLoss:
		return true
	default:
		return false
	}
}

func stateName(state controlv1.BoxState) string {
	if state == controlv1.BoxState_BOX_STATE_UNSPECIFIED {
		return stateAbsent
	}
	return strings.ToLower(strings.TrimPrefix(state.String(), "BOX_STATE_"))
}
