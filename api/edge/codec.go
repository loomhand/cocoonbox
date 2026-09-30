package edge

import (
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
)

// Codec passes proxied frames through as raw bytes and delegates every other message to the proto codec.
type Codec struct {
	proto encoding.CodecV2
}

// NewCodec wraps the registered proto codec.
func NewCodec() Codec {
	return Codec{proto: encoding.GetCodecV2("proto")}
}

func (c Codec) Marshal(v any) (mem.BufferSlice, error) {
	if f, ok := v.(*frame); ok {
		return mem.BufferSlice{mem.SliceBuffer(f.payload)}, nil
	}
	return c.proto.Marshal(v)
}

func (c Codec) Unmarshal(data mem.BufferSlice, v any) error {
	if f, ok := v.(*frame); ok {
		f.payload = data.Materialize()
		return nil
	}
	return c.proto.Unmarshal(data, v)
}

// Name keeps the proto content-subtype so both peers see a normal gRPC call.
func (Codec) Name() string { return "proto" }

type frame struct {
	payload []byte
}
