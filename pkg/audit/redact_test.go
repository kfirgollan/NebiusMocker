package audit

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
)

const secretValue = "s3cr3t"

func createSecretRequest() *mysteryboxpb.CreateSecretRequest {
	return &mysteryboxpb.CreateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{Name: "my-secret", ParentId: "project-x"},
		Spec: &mysteryboxpb.SecretSpec{
			SecretVersion: &mysteryboxpb.SecretVersionSpec{
				Payload: []*mysteryboxpb.Payload{{
					Key:     "password",
					Payload: &mysteryboxpb.Payload_StringValue{StringValue: secretValue},
				}},
			},
		},
	}
}

func jsonOf(t *testing.T, m proto.Message) string {
	t.Helper()
	b, err := protojson.Marshal(m)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return string(b)
}

func TestRedactSecretPayload(t *testing.T) {
	t.Parallel()
	orig := createSecretRequest()
	before := proto.Clone(orig)

	red := Redact(orig)

	if got := jsonOf(t, red); strings.Contains(got, secretValue) {
		t.Errorf("redacted copy still contains the secret: %s", got)
	}
	if got := jsonOf(t, red); !strings.Contains(got, "my-secret") {
		t.Errorf("redaction removed non-sensitive fields: %s", got)
	}
	if diff := cmp.Diff(before, orig, protocmp.Transform()); diff != "" {
		t.Errorf("original was modified (-before +after):\n%s", diff)
	}
	if !strings.Contains(jsonOf(t, orig), secretValue) {
		t.Error("original lost its secret")
	}
}

func TestRedactOperationAny(t *testing.T) {
	t.Parallel()
	packed, err := anypb.New(createSecretRequest())
	if err != nil {
		t.Fatal(err)
	}
	op := &commonpb.Operation{Id: "op-1", Request: packed}

	red := Redact(op)

	if got := jsonOf(t, red); strings.Contains(got, secretValue) {
		t.Errorf("redacted operation still contains the secret: %s", got)
	}
	// The packed request is still decodable and keeps its non-secret parts.
	inner, err := red.(*commonpb.Operation).GetRequest().UnmarshalNew()
	if err != nil {
		t.Fatalf("redacted Any no longer decodes: %v", err)
	}
	if !strings.Contains(jsonOf(t, inner), "my-secret") {
		t.Errorf("redacted Any lost the secret's name: %s", jsonOf(t, inner))
	}
	if !strings.Contains(jsonOf(t, op), secretValue) {
		t.Error("original operation lost its secret")
	}
}

func TestRedactAccessKeySecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  proto.Message
	}{
		{
			name: "get secret response",
			msg:  &iamv2pb.GetAccessKeySecretResponse{AwsAccessKeyId: "AKIA1", Secret: secretValue},
		},
		{
			name: "list response",
			msg: &iamv2pb.ListAccessKeysResponse{Items: []*iamv2pb.AccessKey{
				{Metadata: &commonpb.ResourceMetadata{Id: "accesskey-1"}, Status: &iamv2pb.AccessKeyStatus{Secret: secretValue}},
				{Metadata: &commonpb.ResourceMetadata{Id: "accesskey-2"}, Status: &iamv2pb.AccessKeyStatus{Secret: secretValue}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(jsonOf(t, tt.msg), secretValue) {
				t.Fatal("test message does not carry the secret")
			}
			if got := jsonOf(t, Redact(tt.msg)); strings.Contains(got, secretValue) {
				t.Errorf("secret survived redaction: %s", got)
			}
			if !strings.Contains(jsonOf(t, tt.msg), secretValue) {
				t.Error("original was modified")
			}
		})
	}
}

func TestRedactUnknownAnyDropsValue(t *testing.T) {
	t.Parallel()
	a := &anypb.Any{TypeUrl: "type.googleapis.com/nobody.knows.This", Value: []byte(secretValue)}
	red, ok := Redact(&commonpb.Operation{Id: "op-1", Request: a}).(*commonpb.Operation)
	if !ok {
		t.Fatal("Redact changed the message type")
	}
	if got := red.GetRequest().GetValue(); len(got) != 0 {
		t.Errorf("unknown Any value = %q, want dropped", got)
	}
	if string(a.GetValue()) != secretValue {
		t.Error("original Any was modified")
	}
}

func TestRedactBareUnknownAny(t *testing.T) {
	t.Parallel()
	a := &anypb.Any{TypeUrl: "type.googleapis.com/nobody.knows.This", Value: []byte(secretValue)}
	red, ok := Redact(a).(*anypb.Any)
	if !ok {
		t.Fatal("Redact changed the message type")
	}
	if len(red.GetValue()) != 0 {
		t.Errorf("value = %q, want dropped", red.GetValue())
	}
	if red.GetTypeUrl() != a.GetTypeUrl() {
		t.Errorf("type_url = %q, want %q", red.GetTypeUrl(), a.GetTypeUrl())
	}
}

func TestRedactNil(t *testing.T) {
	t.Parallel()
	if got := Redact(nil); got != nil {
		t.Errorf("Redact(nil) = %v, want nil", got)
	}
}

func TestToStruct(t *testing.T) {
	t.Parallel()
	got := ToStruct(&commonpb.ResourceMetadata{Id: "group-1", ParentId: "project-x", Name: "g"})
	want, err := structpb.NewStruct(map[string]any{"id": "group-1", "parent_id": "project-x", "name": "g"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("ToStruct mismatch (-want +got):\n%s", diff)
	}
	if ToStruct(nil) != nil {
		t.Error("ToStruct(nil) != nil")
	}
}

func TestMaskToken(t *testing.T) {
	t.Parallel()
	t.Run("jwt drops signature", func(t *testing.T) {
		t.Parallel()
		got := MaskToken("aGVhZGVy.cGF5bG9hZA.c2lnbmF0dXJl")
		if got != "aGVhZGVy.cGF5bG9hZA" {
			t.Errorf("MaskToken(jwt) = %q, want header.payload", got)
		}
	})
	t.Run("mocker token drops its mac", func(t *testing.T) {
		t.Parallel()
		if got := MaskToken("nmock.v1.eyJzdWIiOiJ4In0.bWFjbWFj"); got != "nmock.v1.eyJzdWIiOiJ4In0" {
			t.Errorf("MaskToken(nmock) = %q, want nmock.v1.<claims>", got)
		}
	})
	t.Run("mocker token without mac is hidden", func(t *testing.T) {
		t.Parallel()
		if got := MaskToken("nmock.v1.eyJzdWIiOiJ4In0"); strings.Contains(got, "eyJzdWIiOiJ4In0") {
			t.Errorf("MaskToken(nmock, no mac) = %q reveals the token", got)
		}
	})
	t.Run("clipped", func(t *testing.T) {
		t.Parallel()
		got := MaskToken("h." + strings.Repeat("p", 5000) + ".sig")
		if len(got) != maxMaskedToken {
			t.Errorf("len(MaskToken(long jwt)) = %d, want %d", len(got), maxMaskedToken)
		}
	})
	t.Run("opaque is hidden and stable", func(t *testing.T) {
		t.Parallel()
		const tok = "nmock-dev-admin"
		got := MaskToken(tok)
		if got == tok || strings.Contains(got, tok) {
			t.Errorf("MaskToken(%q) = %q reveals the token", tok, got)
		}
		if got != MaskToken(tok) {
			t.Error("MaskToken is not deterministic")
		}
		if other := MaskToken("nmock-dev-viewer"); other == got {
			t.Errorf("different tokens mask identically: %q", got)
		}
	})
	t.Run("short opaque", func(t *testing.T) {
		t.Parallel()
		if got := MaskToken("ab"); got == "ab" || !strings.Contains(got, "***") {
			t.Errorf("MaskToken(short) = %q, want masked form", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if got := MaskToken(""); got != "" {
			t.Errorf("MaskToken(\"\") = %q, want empty", got)
		}
	})
}
