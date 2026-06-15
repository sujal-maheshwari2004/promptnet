package server

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestAuthInterceptor(t *testing.T) {
	call := func(tokens map[string]bool, header string) bool {
		ctx := context.Background()
		if header != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", header))
		}
		passed := false
		h := func(ctx context.Context, req any) (any, error) { passed = true; return nil, nil }
		AuthInterceptor(tokens)(ctx, nil, &grpc.UnaryServerInfo{}, h)
		return passed
	}

	two := map[string]bool{"alice": true, "bob": true}
	if !call(two, "Bearer alice") {
		t.Error("alice's token should authenticate")
	}
	if !call(two, "Bearer bob") {
		t.Error("bob's token should authenticate (independent keys)")
	}
	if call(two, "Bearer revoked") {
		t.Error("unknown token must be rejected")
	}
	if call(two, "") {
		t.Error("missing token must be rejected")
	}
	if !call(map[string]bool{}, "") {
		t.Error("empty token set disables auth")
	}
}
