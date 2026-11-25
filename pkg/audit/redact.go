package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// sanitizer is implemented by every gosdk message that has (or contains) a
// field marked (nebius.sensitive); the generated method masks those fields.
type sanitizer interface{ Sanitize() }

// Redact returns a copy of m with every sensitive field masked. It never
// modifies m. Unlike a bare Sanitize call it also descends into
// google.protobuf.Any values (Operation.request, Operation.resource), and it
// drops an Any whose type is unknown, since it cannot be inspected.
func Redact(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	c := proto.Clone(m)
	redact(c.ProtoReflect())
	return c
}

func redact(m protoreflect.Message) {
	if !m.IsValid() {
		return
	}
	if a, ok := m.Interface().(*anypb.Any); ok {
		redactAny(a)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					redact(mv.Message())
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				l := v.List()
				for i := range l.Len() {
					redact(l.Get(i).Message())
				}
			}
		case fd.Message() != nil:
			redact(v.Message())
		}
		return true
	})
	if s, ok := m.Interface().(sanitizer); ok {
		s.Sanitize()
	}
}

// redactAny redacts the message packed in a in place.
func redactAny(a *anypb.Any) {
	inner, err := a.UnmarshalNew()
	if err != nil {
		a.Value = nil // unknown or corrupt payload: drop it rather than risk a leak
		return
	}
	redact(inner.ProtoReflect())
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(inner)
	if err != nil {
		a.Value = nil
		return
	}
	a.Value = data
}

// ToStruct renders a (redacted) message as a JSON-shaped Struct for
// request.parameters and response.payload. It returns nil when m cannot be
// rendered.
func ToStruct(m proto.Message) *structpb.Struct {
	if m == nil {
		return nil
	}
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(data, s); err != nil {
		return nil // not a JSON object (well-known scalar wrappers)
	}
	return s
}

// MaskToken returns the form of a bearer token that may appear in an event.
// A JWT loses its signature, as in Nebius. An opaque token (the mocker's dev
// tokens) is replaced by a short prefix and a hash, so the event identifies
// the credential without revealing it.
func MaskToken(token string) string {
	if token == "" {
		return ""
	}
	if parts := strings.Split(token, "."); len(parts) == 3 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "." + parts[1]
	}
	sum := sha256.Sum256([]byte(token))
	prefix := token[:min(len(token)/3, 6)]
	return prefix + "***" + hex.EncodeToString(sum[:])[:12]
}
