// Command promptnet is the single-binary server.
//
//	promptnet serve [-addr :8443] [-db promptnet.db] [-tls-cert c -tls-key k]
//	               [-embed-url URL -embed-model M]   (PROMPTNET_TOKEN, PROMPTNET_EMBED_* env)
//	promptnet put  -uri promptnet://o/r/p -file tmpl.txt [-slot name ...] [-db promptnet.db]
//	promptnet diff -uri promptnet://o/r/p -file edited.txt [-addr localhost:8443]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "promptnet/gen/promptnet/v1"
	"promptnet/internal/pubsub"
	"promptnet/internal/semdiff"
	"promptnet/internal/server"
	"promptnet/internal/store"
	"promptnet/internal/validate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "put":
		put(os.Args[2:])
	case "diff":
		diff(os.Args[2:])
	case "publish":
		publish(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: promptnet serve|put|diff|publish [flags]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8443", "listen address")
	dbPath := fs.String("db", "promptnet.db", "sqlite path")
	cert := fs.String("tls-cert", "", "TLS cert file (optional)")
	key := fs.String("tls-key", "", "TLS key file (optional)")
	cacheTTL := fs.Duration("cache-ttl", 30*time.Second, "L2 server cache TTL; 0 disables")
	embedURL := fs.String("embed-url", os.Getenv("PROMPTNET_EMBED_URL"), "OpenAI-compatible /v1/embeddings URL for semantic diff (default: offline lexical embedder)")
	embedModel := fs.String("embed-model", os.Getenv("PROMPTNET_EMBED_MODEL"), "embedding model name")
	natsAddr := fs.String("nats-addr", "127.0.0.1:4222", "embedded NATS listen address for pub/sub; empty disables it")
	fs.Parse(args)
	token := os.Getenv("PROMPTNET_TOKEN")
	embedKey := os.Getenv("PROMPTNET_EMBED_KEY")

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	emb := buildEmbedder(*embedURL, *embedModel, embedKey)

	var notifier server.Notifier
	if *natsAddr != "" {
		host, port := splitHostPort(*natsAddr)
		bus, err := pubsub.NewEmbedded(host, port)
		if err != nil {
			log.Fatal(err)
		}
		defer bus.Close()
		notifier = bus
	}

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(server.AuthInterceptor(token))}
	if *cert != "" && *key != "" {
		creds, err := credentials.NewServerTLSFromFile(*cert, *key)
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, grpc.Creds(creds))
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterPromptServiceServer(gs, server.NewServer(st, *cacheTTL, emb, notifier))

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("promptnet serving on %s (tls=%v auth=%v cache-ttl=%v embed=%s nats=%s)",
		*addr, *cert != "", token != "", *cacheTTL, embedName(*embedURL, *embedModel), *natsAddr)
	if err := gs.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// splitHostPort parses host:port; an empty host means all interfaces.
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatalf("bad -nats-addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("bad -nats-addr port %q: %v", portStr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return host, port
}

// buildEmbedder picks the operator-configured embedding model. With no URL it
// falls back to the offline lexical embedder so the server runs with zero deps.
func buildEmbedder(url, model, key string) semdiff.Embedder {
	if url != "" {
		return semdiff.HTTPEmbedder{URL: url, Model: model, APIKey: key}
	}
	return semdiff.LexicalEmbedder{}
}

func embedName(url, model string) string {
	if url == "" {
		return "lexical(offline)"
	}
	return model + "@" + url
}

func put(args []string) {
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	uri := fs.String("uri", "", "promptnet:// uri")
	file := fs.String("file", "-", "template file (- for stdin)")
	dbPath := fs.String("db", "promptnet.db", "sqlite path")
	force := fs.Bool("force", false, "store even if the edit is a structural change")
	embedURL := fs.String("embed-url", os.Getenv("PROMPTNET_EMBED_URL"), "embeddings URL for the pre-commit semantic check")
	embedModel := fs.String("embed-model", os.Getenv("PROMPTNET_EMBED_MODEL"), "embedding model name")
	var slots multiFlag
	fs.Var(&slots, "slot", "declared slot name (repeatable)")
	fs.Parse(args)

	template := readTemplate(*file)
	// Commit-time validation: broken prompts never reach the store.
	if err := validate.Prompt(*uri, template, slots); err != nil {
		log.Fatalf("validation failed: %v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// Pre-commit semantic check: if this overwrites an existing prompt, show how
	// far the edit ripples and refuse a structural change unless -force.
	prev, err := st.Get(context.Background(), *uri)
	switch {
	case err == nil && prev.Template != template:
		emb := buildEmbedder(*embedURL, *embedModel, os.Getenv("PROMPTNET_EMBED_KEY"))
		res, derr := semdiff.Analyze(emb, splitLines(prev.Template), splitLines(template))
		if derr != nil {
			log.Fatal(derr)
		}
		fmt.Print(semdiff.Format(res))
		if !*force && hasStructural(res) {
			log.Fatal("structural change detected; re-run with -force to store")
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		log.Fatal(err)
	}

	if err := st.Put(context.Background(), store.Prompt{URI: *uri, Template: template, Slots: slots}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stored %s (%s)\n", *uri, store.Hash(template, slots)[:12])
}

func hasStructural(res []semdiff.Result) bool {
	for _, r := range res {
		if r.Class == "structural" {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// diff asks the server to run the Semantic Propagation Diff between the stored
// prompt at -uri (the original) and the edited template in -file, using the
// embedding model the server was configured with.
func diff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8443", "server address")
	uri := fs.String("uri", "", "stored prompt uri (the original)")
	file := fs.String("file", "-", "edited template file (- for stdin)")
	useTLS := fs.Bool("tls", false, "use TLS")
	caCert := fs.String("ca-cert", "", "CA cert for TLS (optional)")
	fs.Parse(args)
	if *uri == "" {
		log.Fatal("usage: promptnet diff -uri promptnet://... -file edited.txt [-addr localhost:8443]")
	}
	edited := readTemplate(*file)

	conn := dial(*addr, *useTLS, *caCert)
	defer conn.Close()
	resp, err := pb.NewPromptServiceClient(conn).DiffPrompt(authCtx(),
		&pb.DiffPromptRequest{Uri: *uri, NewTemplate: edited})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(semdiff.Format(fromProto(resp)))
}

// publish stores a new prompt version on the server and notifies subscribers.
func publish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8443", "server address")
	uri := fs.String("uri", "", "promptnet:// uri")
	file := fs.String("file", "-", "template file (- for stdin)")
	useTLS := fs.Bool("tls", false, "use TLS")
	caCert := fs.String("ca-cert", "", "CA cert for TLS (optional)")
	var slots multiFlag
	fs.Var(&slots, "slot", "declared slot name (repeatable)")
	fs.Parse(args)
	if *uri == "" {
		log.Fatal("usage: promptnet publish -uri promptnet://... -file tmpl.txt [-slot name ...]")
	}
	template := readTemplate(*file)

	conn := dial(*addr, *useTLS, *caCert)
	defer conn.Close()
	resp, err := pb.NewPromptServiceClient(conn).PublishPrompt(authCtx(),
		&pb.PublishPromptRequest{Uri: *uri, Template: template, Slots: slots})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("published %s (%s) — subscribers notified\n", *uri, resp.GetVersionHash()[:12])
}

// dial opens a gRPC client connection, optionally over TLS.
func dial(addr string, useTLS bool, caCert string) *grpc.ClientConn {
	creds := insecure.NewCredentials()
	if useTLS {
		var err error
		if creds, err = credentials.NewClientTLSFromFile(caCert, ""); err != nil {
			log.Fatal(err)
		}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}
	return conn
}

// authCtx attaches the bearer token from PROMPTNET_TOKEN, if set.
func authCtx() context.Context {
	ctx := context.Background()
	if token := os.Getenv("PROMPTNET_TOKEN"); token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return ctx
}

// fromProto rebuilds semdiff.Results so the shared formatter can render them.
func fromProto(resp *pb.DiffPromptResponse) []semdiff.Result {
	out := make([]semdiff.Result, len(resp.GetChanges()))
	for i, c := range resp.GetChanges() {
		out[i] = semdiff.Result{
			Change:  semdiff.Change{OldStart: int(c.OldStart), OldEnd: int(c.OldEnd), NewStart: int(c.NewStart), NewEnd: int(c.NewEnd)},
			Signal2: c.PointDelta,
			Up:      semdiff.Direction{Curve: fromWindows(c.Up), StoppedAtBoundary: c.UpBoundary},
			Down:    semdiff.Direction{Curve: fromWindows(c.Down), StoppedAtBoundary: c.DownBoundary},
			Class:   c.Classification,
		}
	}
	return out
}

func fromWindows(ws []*pb.Window) []semdiff.Window {
	out := make([]semdiff.Window, len(ws))
	for i, w := range ws {
		out[i] = semdiff.Window{Radius: int(w.Radius), Delta: w.Delta}
	}
	return out
}

func readTemplate(file string) string {
	var b []byte
	var err error
	if file == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		log.Fatal(err)
	}
	return string(b)
}

type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
