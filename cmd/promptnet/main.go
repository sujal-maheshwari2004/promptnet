// Command promptnet is the single-binary server.
//
//	promptnet serve [-addr :8443] [-db promptnet.db] [-tls-cert c -tls-key k]
//	               [-embed-url URL -embed-model M]   (PROMPTNET_TOKEN, PROMPTNET_EMBED_* env)
//	promptnet put  -uri promptnet://o/r/p -file tmpl.txt [-slot name ...] [-db promptnet.db]
//	promptnet diff -uri promptnet://o/r/p -file edited.txt [-addr localhost:8443]
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
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
	case "watch":
		watch(os.Args[2:])
	case "backup":
		backup(os.Args[2:])
	case "restore":
		restore(os.Args[2:])
	case "migrate":
		migrateCmd(os.Args[2:])
	case "gen-token":
		genToken()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: promptnet serve|put|diff|publish|watch|backup|restore|migrate|gen-token [flags]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8443", "listen address")
	dbPath := fs.String("db", "promptnet.db", "sqlite file path, or a postgres:// DSN for enterprise")
	cert := fs.String("tls-cert", "", "TLS cert file (optional)")
	key := fs.String("tls-key", "", "TLS key file (optional)")
	clientCA := fs.String("client-ca", "", "CA cert to verify client certs; enables mTLS (requires -tls-cert/-tls-key)")
	cacheTTL := fs.Duration("cache-ttl", 30*time.Second, "L2 server cache TTL; 0 disables")
	redisURL := fs.String("redis-url", os.Getenv("PROMPTNET_REDIS_URL"), "redis:// URL for a shared L2 cache (default: in-process cache)")
	embedURL := fs.String("embed-url", os.Getenv("PROMPTNET_EMBED_URL"), "OpenAI-compatible /v1/embeddings URL for semantic diff (default: offline lexical embedder)")
	embedModel := fs.String("embed-model", os.Getenv("PROMPTNET_EMBED_MODEL"), "embedding model name")
	natsAddr := fs.String("nats-addr", "127.0.0.1:4222", "embedded NATS listen address for pub/sub; empty disables it")
	tokensFile := fs.String("tokens-file", "", "file of `token [org]` lines (# comments ok); org scopes the token, blank = admin")
	metricsAddr := fs.String("metrics-addr", ":2112", "Prometheus /metrics listen address; empty disables it")
	rateLimit := fs.Float64("rate-limit", 0, "per-org request/sec limit; 0 disables")
	rateBurst := fs.Int("rate-burst", 0, "per-org burst size; 0 = equal to -rate-limit")
	fs.Parse(args)
	tokens := loadTokens(*tokensFile)
	embedKey := os.Getenv("PROMPTNET_EMBED_KEY")

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	emb := buildEmbedder(*embedURL, *embedModel, embedKey)
	cache := buildCache(*redisURL, *cacheTTL)

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

	if *metricsAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", server.MetricsHandler())
			log.Printf("metrics on %s/metrics", *metricsAddr)
			if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
				log.Printf("metrics server stopped: %v", err)
			}
		}()
	}

	audit := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	opts := []grpc.ServerOption{grpc.ChainUnaryInterceptor(
		server.MetricsInterceptor,                       // outermost: observes every outcome
		server.AuthInterceptor(tokens),                  // sets org scope
		server.AuditInterceptor(audit),                  // logs with scope, sees rate-limit rejections
		server.RateLimitInterceptor(*rateLimit, *rateBurst),
	)}
	if *cert != "" && *key != "" {
		creds, err := serverCreds(*cert, *key, *clientCA)
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, grpc.Creds(creds))
	} else if *clientCA != "" {
		log.Fatal("-client-ca requires -tls-cert and -tls-key")
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterPromptServiceServer(gs, server.NewServer(st, cache, emb, notifier))

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("promptnet serving on %s (tls=%v mtls=%v auth=%d-token cache=%s embed=%s nats=%s metrics=%s ratelimit=%g/s)",
		*addr, *cert != "", *clientCA != "", len(tokens), cacheName(*redisURL, *cacheTTL), embedName(*embedURL, *embedModel), *natsAddr, *metricsAddr, *rateLimit)
	if err := gs.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// serverCreds builds the server's TLS credentials. With clientCA set it
// requires and verifies client certs (mutual TLS).
func serverCreds(certFile, keyFile, clientCA string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if clientCA != "" {
		pool, err := certPool(clientCA)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(cfg), nil
}

// certPool loads a PEM file into a fresh cert pool.
func certPool(pemFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(pemFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", pemFile)
	}
	return pool, nil
}

// buildCache picks the L2 cache: Redis when a URL is given, else the in-process
// cache (or none when ttl<=0).
func buildCache(redisURL string, ttl time.Duration) server.Cache {
	if redisURL != "" {
		c, err := server.NewRedisCache(redisURL, ttl)
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
		return c
	}
	return server.NewMemCache(ttl)
}

func cacheName(redisURL string, ttl time.Duration) string {
	switch {
	case redisURL != "":
		return "redis"
	case ttl > 0:
		return "mem"
	default:
		return "off"
	}
}

// loadTokens reads bearer tokens, their org scope, and an optional expiry.
// PROMPTNET_TOKEN is an admin token (all orgs, never expires). The file has
// `token [org] [expiry]` lines — org scopes the token to promptnet://org/…
// (blank = admin); expiry is a date (2006-01-02) or RFC3339 timestamp, after
// which the token is rejected. Blank lines and # comments are ignored. An empty
// result = auth disabled. To rotate: add the new token, give the old one a
// near-future expiry, drop it once it lapses.
func loadTokens(file string) map[string]server.Token {
	tokens := map[string]server.Token{}
	if t := os.Getenv("PROMPTNET_TOKEN"); t != "" {
		tokens[t] = server.Token{} // admin, never expires
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			log.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			tok := server.Token{}
			if len(fields) > 1 {
				tok.Org = fields[1]
			}
			if len(fields) > 2 {
				tok.Expires = parseExpiry(fields[2])
			}
			tokens[fields[0]] = tok
		}
	}
	return tokens
}

// parseExpiry accepts a bare date (2006-01-02) or an RFC3339 timestamp.
func parseExpiry(s string) time.Time {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		log.Fatalf("bad token expiry %q: want 2006-01-02 or RFC3339", s)
	}
	return t
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
	dbPath := fs.String("db", "promptnet.db", "sqlite file path, or a postgres:// DSN for enterprise")
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
	clientCert := fs.String("cert", "", "client cert for mTLS (optional)")
	clientKey := fs.String("key", "", "client key for mTLS (optional)")
	fs.Parse(args)
	if *uri == "" {
		log.Fatal("usage: promptnet diff -uri promptnet://... -file edited.txt [-addr localhost:8443]")
	}
	edited := readTemplate(*file)

	conn := dial(*addr, *useTLS, *caCert, *clientCert, *clientKey)
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
	clientCert := fs.String("cert", "", "client cert for mTLS (optional)")
	clientKey := fs.String("key", "", "client key for mTLS (optional)")
	var slots multiFlag
	fs.Var(&slots, "slot", "declared slot name (repeatable)")
	fs.Parse(args)
	if *uri == "" {
		log.Fatal("usage: promptnet publish -uri promptnet://... -file tmpl.txt [-slot name ...]")
	}
	template := readTemplate(*file)

	conn := dial(*addr, *useTLS, *caCert, *clientCert, *clientKey)
	defer conn.Close()
	resp, err := pb.NewPromptServiceClient(conn).PublishPrompt(authCtx(),
		&pb.PublishPromptRequest{Uri: *uri, Template: template, Slots: slots})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("published %s (%s) — subscribers notified\n", *uri, resp.GetVersionHash()[:12])
}

// watch subscribes to a prompt's version-change events — the push side an agent
// uses to learn the prompt it runs was updated. It prints each change; with -exec
// it also runs a command (POSIX sh -c) with the new hash in $PROMPTNET_VERSION,
// so it works as a sidecar that reloads or redeploys on prompt updates.
func watch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	uri := fs.String("uri", "", "prompt uri to watch")
	natsURL := fs.String("nats-url", "nats://127.0.0.1:4222", "NATS url the server exposes")
	hook := fs.String("exec", "", "command to run on each change (via OS shell); new hash in PROMPTNET_VERSION env")
	fs.Parse(args)
	if *uri == "" {
		log.Fatal("usage: promptnet watch -uri promptnet://... [-nats-url nats://host:4222] [-exec CMD]")
	}
	sub, err := pubsub.Connect(*natsURL)
	if err != nil {
		log.Fatal(err)
	}
	defer sub.Close()
	if _, err := sub.Subscribe(*uri, func(hash string) {
		log.Printf("%s updated -> %s", *uri, hash)
		if *hook != "" {
			runHook(*hook, *uri, hash)
		}
	}); err != nil {
		log.Fatal(err)
	}
	log.Printf("watching %s on %s", *uri, *natsURL)
	select {} // block forever; Ctrl-C to stop
}

// runHook runs the -exec command with the changed prompt's uri/hash in the env,
// through the platform's shell (sh on unix, cmd on Windows).
func runHook(cmd, uri, hash string) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Env = append(os.Environ(), "PROMPTNET_URI="+uri, "PROMPTNET_VERSION="+hash)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		log.Printf("exec hook failed: %v", err)
	}
}

// backup writes every stored prompt to -out as JSON lines (one prompt per
// line) — a portable snapshot that restores into either backend.
func backup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dbPath := fs.String("db", "promptnet.db", "sqlite file path, or a postgres:// DSN")
	out := fs.String("out", "-", "output file (- for stdout)")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	prompts, err := st.Dump(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	for _, p := range prompts {
		if err := enc.Encode(p); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Fprintf(os.Stderr, "backed up %d prompts\n", len(prompts))
}

// restore reads JSON-lines prompts from -in and upserts each into the store.
// Put is an idempotent upsert, so restoring is safe to repeat.
func restore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dbPath := fs.String("db", "promptnet.db", "sqlite file path, or a postgres:// DSN")
	in := fs.String("in", "-", "input file (- for stdin)")
	fs.Parse(args)

	r := io.Reader(os.Stdin)
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		r = f
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	dec := json.NewDecoder(r)
	n := 0
	for {
		var p store.Prompt
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		if err := st.Put(context.Background(), p); err != nil {
			log.Fatal(err)
		}
		n++
	}
	fmt.Printf("restored %d prompts\n", n)
}

// migrateCmd applies pending schema migrations (Open runs them) and reports the
// resulting schema version.
func migrateCmd(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dbPath := fs.String("db", "promptnet.db", "sqlite file path, or a postgres:// DSN")
	fs.Parse(args)
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	fmt.Printf("schema up to date (version %d)\n", st.SchemaVersion())
}

// genToken prints a fresh random bearer token for the tokens file.
func genToken() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	fmt.Println(hex.EncodeToString(b))
}

// dial opens a gRPC client connection, optionally over TLS. With clientCert/Key
// set it presents a client certificate for mutual TLS.
func dial(addr string, useTLS bool, caCert, clientCert, clientKey string) *grpc.ClientConn {
	creds := insecure.NewCredentials()
	if useTLS {
		cfg := &tls.Config{}
		if caCert != "" {
			pool, err := certPool(caCert)
			if err != nil {
				log.Fatal(err)
			}
			cfg.RootCAs = pool
		}
		if clientCert != "" {
			c, err := tls.LoadX509KeyPair(clientCert, clientKey)
			if err != nil {
				log.Fatal(err)
			}
			cfg.Certificates = []tls.Certificate{c}
		}
		creds = credentials.NewTLS(cfg)
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
