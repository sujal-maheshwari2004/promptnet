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

// Notifier publishes version-change events. *pubsub.Bus implements it; nil
// disables notifications.
type Notifier interface {
	Publish(uri, versionHash string) error
}

type Server struct {
	pb.UnimplementedPromptServiceServer
	Store    *store.Store
	Cache    Cache            // L2 cache (mem or Redis); nil disables it
	Embedder semdiff.Embedder // configured at startup; used by DiffPrompt
	Notifier Notifier         // Phase 4 pub/sub; nil disables it
}

// NewServer wires a server with an L2 cache (nil to disable), the embedder used
// for semantic diffs, and the pub/sub notifier (nil to disable distribution).
func NewServer(st *store.Store, cache Cache, emb semdiff.Embedder, n Notifier) *Server {
	return &Server{Store: st, Cache: cache, Embedder: emb, Notifier: n}
}

// PublishPrompt validates, stores a new prompt version, invalidates its cache
// entry, and notifies subscribers — the write-through publisher path.
func (s *Server) PublishPrompt(ctx context.Context, req *pb.PublishPromptRequest) (*pb.PublishPromptResponse, error) {
	if err := authorize(ctx, req.GetUri()); err != nil {
		return nil, err
	}
	if err := validate.Prompt(req.GetUri(), req.GetTemplate(), req.GetSlots()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid prompt: %v", err)
	}
	hash := store.Hash(req.GetTemplate(), req.GetSlots())

	// Idempotent: republishing the same content is a no-op — no write, no notify.
	// This lets `promptctl push` publish every prompt and only changed ones fire.
	if prev, err := s.Store.Get(ctx, req.GetUri()); err == nil && prev.VersionHash == hash {
		return &pb.PublishPromptResponse{VersionHash: hash}, nil
	}

	if err := s.Store.Put(ctx, store.Prompt{URI: req.GetUri(), Template: req.GetTemplate(), Slots: req.GetSlots()}); err != nil {
		return nil, status.Errorf(codes.Internal, "store failed: %v", err)
	}
	if s.Cache != nil {
		s.Cache.Invalidate(req.GetUri())
	}
	if s.Notifier != nil {
		// Best-effort: the version is durably stored even if the notify fails;
		// subscribers still converge on the next TTL poll.
		_ = s.Notifier.Publish(req.GetUri(), hash)
	}
	return &pb.PublishPromptResponse{VersionHash: hash}, nil
}

func (s *Server) GetPrompt(ctx context.Context, req *pb.GetPromptRequest) (*pb.GetPromptResponse, error) {
	uri := req.GetUri()
	if err := authorize(ctx, uri); err != nil {
		return nil, err
	}
	if s.Cache != nil {
		if resp, ok := s.Cache.Get(uri); ok {
			return resp, nil
		}
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
	if s.Cache != nil {
		s.Cache.Put(uri, resp)
	}
	return resp, nil
}

// DiffPrompt runs the Semantic Propagation Diff between the stored prompt (the
// original) and the supplied edited template, using the server's embedder.
func (s *Server) DiffPrompt(ctx context.Context, req *pb.DiffPromptRequest) (*pb.DiffPromptResponse, error) {
	if err := authorize(ctx, req.GetUri()); err != nil {
		return nil, err
	}
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

type scopeKey struct{}

// Token is a bearer credential: the org it is scoped to ("" = admin, all orgs)
// and an optional expiry (zero = never expires). Rotation is just overlapping
// tokens — issue the new one, let the old one's Expires lapse, then drop it.
type Token struct {
	Org     string
	Expires time.Time
}

// AuthInterceptor authenticates bearer tokens, rejects expired ones, and
// attaches each token's org scope to the context. An empty token map disables
// auth, leaving every request unscoped (full access).
func AuthInterceptor(tokens map[string]Token) grpc.UnaryServerInterceptor {
	type entry struct {
		want    []byte
		org     string
		expires time.Time
	}
	wants := make([]entry, 0, len(tokens))
	for t, tok := range tokens {
		wants = append(wants, entry{want: []byte("Bearer " + t), org: tok.Org, expires: tok.Expires})
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if len(wants) == 0 {
			return handler(ctx, req)
		}
		var got []byte
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get("authorization"); len(v) > 0 {
				got = []byte(v[0])
			}
		}
		ok, org := false, ""
		var expires time.Time
		for _, w := range wants { // no early break: keep timing independent of which key matches
			if subtle.ConstantTimeCompare(got, w.want) == 1 {
				ok, org, expires = true, w.org, w.expires
			}
		}
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing token")
		}
		if !expires.IsZero() && time.Now().After(expires) {
			return nil, status.Error(codes.Unauthenticated, "token expired")
		}
		return handler(context.WithValue(ctx, scopeKey{}, org), req)
	}
}

// authorize enforces the caller's org scope: a token scoped to org "acme" may
// only touch promptnet://acme/… An empty scope (admin, or auth disabled) passes.
func authorize(ctx context.Context, uri string) error {
	scope, _ := ctx.Value(scopeKey{}).(string)
	if scope == "" || scope == orgOf(uri) {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "token not authorized for org %q", orgOf(uri))
}

// orgOf returns the first path segment of a prompt URI (the owning org).
func orgOf(uri string) string {
	s := strings.TrimPrefix(uri, "promptnet://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}
