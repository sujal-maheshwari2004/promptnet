package server

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// call runs the interceptor and returns whether the handler ran and the scope
// the interceptor attached (empty if it didn't run or scope was admin/"").
func call(tokens map[string]string, header string) (passed bool, scope string) {
	ctx := context.Background()
	if header != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", header))
	}
	h := func(ctx context.Context, req any) (any, error) {
		passed = true
		scope, _ = ctx.Value(scopeKey{}).(string)
		return nil, nil
	}
	AuthInterceptor(tokens)(ctx, nil, &grpc.UnaryServerInfo{}, h)
	return passed, scope
}

func TestAuthInterceptor(t *testing.T) {
	two := map[string]string{"alice": "", "bob": "acme"}
	if ok, _ := call(two, "Bearer alice"); !ok {
		t.Error("alice's token should authenticate")
	}
	if ok, scope := call(two, "Bearer bob"); !ok || scope != "acme" {
		t.Errorf("bob's token should authenticate scoped to acme, got ok=%v scope=%q", ok, scope)
	}
	if ok, _ := call(two, "Bearer revoked"); ok {
		t.Error("unknown token must be rejected")
	}
	if ok, _ := call(two, ""); ok {
		t.Error("missing token must be rejected")
	}
	if ok, _ := call(map[string]string{}, ""); !ok {
		t.Error("empty token set disables auth")
	}
}

func TestAuthorize(t *testing.T) {
	scoped := context.WithValue(context.Background(), scopeKey{}, "acme")
	if err := authorize(scoped, "promptnet://acme/r/p"); err != nil {
		t.Errorf("acme token on acme prompt should pass: %v", err)
	}
	if err := authorize(scoped, "promptnet://other/r/p"); err == nil {
		t.Error("acme token on other org's prompt must be denied")
	}
	// Empty scope (admin / auth disabled) can touch any org.
	if err := authorize(context.Background(), "promptnet://anything/r/p"); err != nil {
		t.Errorf("admin/unscoped should pass: %v", err)
	}
}
