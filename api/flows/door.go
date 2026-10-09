package flows

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/projecteru2/core/log"
)

// DoorPath is the box door's flows route; the door serves it beside Connect's MCP.
const DoorPath = "/flows"

// Caller maps a box door request's Authorization header to the machine's owner and the machine.
type Caller func(ctx context.Context, header string) (owner, machine string, err error)

// Origins names the box and the image of an owner's machine.
type Origins func(ctx context.Context, owner, machine string) Origin

// Door serves the library to the boxes: each request acts for the owner of the machine whose door key it carries.
type Door struct {
	lib    *Library
	caller Caller
	origin Origins
	mux    *http.ServeMux
}

// NewDoor serves lib on the box door, authenticating each request with caller.
func NewDoor(lib *Library, caller Caller, origin Origins) *Door {
	d := &Door{lib: lib, caller: caller, origin: origin, mux: http.NewServeMux()}
	d.mux.HandleFunc("GET "+DoorPath, d.list)
	d.mux.HandleFunc("GET "+DoorPath+"/{name}", d.get)
	d.mux.HandleFunc("PUT "+DoorPath+"/{name}", d.put)
	return d
}

func (d *Door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, machine, err := d.caller(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	d.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, doorCaller{owner: owner, machine: machine})))
}

func (d *Door) list(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	rev, entries, err := d.lib.List(r.Context(), c.owner)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"rev": rev, "flows": entries})
}

func (d *Door) get(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	e, content, err := d.lib.Get(r.Context(), c.owner, r.PathValue("name"))
	if err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Flow-Rev", strconv.FormatUint(e.Rev, 10))
	w.Header().Set("X-Flow-Sha256", e.SHA256)
	_, _ = w.Write(content)
}

func (d *Door) put(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxContent+1))
	if err != nil {
		fail(w, r, ErrFull)
		return
	}
	e, err := d.lib.Put(r.Context(), c.owner, r.PathValue("name"), content, d.origin(r.Context(), c.owner, c.machine))
	if err != nil {
		fail(w, r, err)
		return
	}
	log.WithFunc("flows.Door.put").Infof(r.Context(), "machine %s of %s saved a flow of %d bytes as rev %d", c.machine, c.owner, e.Size, e.Rev)
	writeJSON(w, e)
}

type callerKey struct{}

type doorCaller struct {
	owner   string
	machine string
}

func callerOf(r *http.Request) doorCaller {
	c, _ := r.Context().Value(callerKey{}).(doorCaller)
	return c
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrFull):
		code = http.StatusRequestEntityTooLarge
	default:
		log.WithFunc("flows.Door").Error(r.Context(), err, "serve a box's flow request")
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
