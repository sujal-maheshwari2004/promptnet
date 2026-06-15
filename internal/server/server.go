package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "promptnet/gen/promptnet/v1"
	"promptnet/internal/semdiff"
	"promptnet/internal/store"
	"promptnet/internal/validate"
)

type Server struct {
	pb.UnimplementedPromptServiceServer
	Store    *store.Store
	Cache    *ttlCache         // L2 cache; nil disables it
	Embedder semdiff.Embedder  // configured at startup; used by DiffPrompt
}

// NewServer wires a server with an L2 cache of the given TTL (non-positive
// disables it) and the embedder used for semantic diffs.
func NewServer(st *store.Store, ttl time.Duration, emb semdiff.Embedder) *Server {
	s := &Server{Store: st, Embedder: emb}
	if ttl > 0 {
		s.Cache = newTTLCache(ttl)
	}
	return s
}

func (s *Server) GetPrompt(ctx context.Context, req *pb.GetPromptRequest) (*pb.GetPromptResponse, error) {
	uri := req.GetUri()
	if resp, ok := s.Cache.get(uri); ok {
		return resp, nil
	}
	p, err := s.Store.Get(ctx, uri)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "prompt %q not found", uri)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lookup failed: %v", err)
	}
	// Serve-time validation: never hand a malformed prompt to a production agent,
	// and never cache one (validation before caching).
	if err := validate.Prompt(p.URI, p.Template, p.Slots); err != nil {
		return nil, status.Errorf(codes.DataLoss, "stored prompt invalid: %v", err)
	}
	resp := &pb.GetPromptResponse{
		Uri:         p.URI,
		Template:    p.Template,
		Slots:       p.Slots,
		VersionHash: p.VersionHash,
	}
	s.Cache.put(uri, resp)
	return resp, nil
}

// DiffPrompt runs the Semantic Propagation Diff between the stored prompt (the
// original) and the supplied edited template, using the server's embedder.
func (s *Server) DiffPrompt(ctx context.Context, req *pb.DiffPromptRequest) (*pb.DiffPromptResponse, error) {
	p, err := s.Store.Get(ctx, req.GetUri())
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "prompt %q not found", req.GetUri())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lookup failed: %v", err)
	}
	results, err := semdiff.Analyze(s.Embedder, splitLines(p.Template), splitLines(req.GetNewTemplate()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "diff failed: %v", err)
	}
	resp := &pb.DiffPromptResponse{Changes: make([]*pb.Change, len(results))}
	for i, r := range results {
		resp.Changes[i] = &pb.Change{
			OldStart:       int32(r.Change.OldStart),
			OldEnd:         int32(r.Change.OldEnd),
			NewStart:       int32(r.Change.NewStart),
			NewEnd:         int32(r.Change.NewEnd),
			Kind:           semdiff.Kind(r.Change),
			PointDelta:     r.Signal2,
			Up:             toWindows(r.Up.Curve),
			Down:           toWindows(r.Down.Curve),
			UpBoundary:     r.Up.StoppedAtBoundary,
			DownBoundary:   r.Down.StoppedAtBoundary,
			Classification: r.Class,
		}
	}
	return resp, nil
}

func toWindows(ws []semdiff.Window) []*pb.Window {
	out := make([]*pb.Window, len(ws))
	for i, w := range ws {
		out[i] = &pb.Window{Radius: int32(w.Radius), Delta: w.Delta}
	}
	return out
}

func splitLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// AuthInterceptor enforces a static bearer token. Empty token disables auth.
// ponytail: single static token; swap for per-org API keys when multi-tenant.
func AuthInterceptor(token string) grpc.UnaryServerInterceptor {
	want := []byte("Bearer " + token)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if token == "" {
			return handler(ctx, req)
		}
		var got string
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get("authorization"); len(v) > 0 {
				got = v[0]
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing token")
		}
		return handler(ctx, req)
	}
}
