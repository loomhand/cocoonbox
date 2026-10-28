package users

import (
	"context"
	"encoding/json"
)

// ToolList is what the agent of an image serves over MCP: its tools/list tools and its initialize instructions.
type ToolList struct {
	Image        string
	Tools        json.RawMessage
	Instructions string
}

// ToolList returns the tool list of image, else the newest one of family, else the zero ToolList.
func (s *Service) ToolList(ctx context.Context, image, family string) (ToolList, error) {
	return s.store.toolList(ctx, image, family)
}

// PutToolList records l as what the agent of l.Image, an image of family, serves.
func (s *Service) PutToolList(ctx context.Context, family string, l ToolList) error {
	return s.store.putToolList(ctx, family, l)
}
