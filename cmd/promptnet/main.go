// Command promptnet is the single-binary Phase 1 server.
//
//	promptnet serve [-addr :8443] [-db promptnet.db] [-tls-cert c -tls-key k]   (PROMPTNET_TOKEN for auth)
//	promptnet put -uri promptnet://o/r/p -file tmpl.txt [-slot name ...] [-db promptnet.db]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "promptnet/gen/promptnet/v1"
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: promptnet serve|put [flags]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8443", "listen address")
	dbPath := fs.String("db", "promptnet.db", "sqlite path")
	cert := fs.String("tls-cert", "", "TLS cert file (optional)")
	key := fs.String("tls-key", "", "TLS key file (optional)")
	fs.Parse(args)
	token := os.Getenv("PROMPTNET_TOKEN")

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(server.AuthInterceptor(token))}
	if *cert != "" && *key != "" {
		creds, err := credentials.NewServerTLSFromFile(*cert, *key)
		if err != nil {
			log.Fatal(err)
		}
		opts = append(opts, grpc.Creds(creds))
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterPromptServiceServer(gs, &server.Server{Store: st})

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("promptnet serving on %s (tls=%v auth=%v)", *addr, *cert != "", token != "")
	if err := gs.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

func put(args []string) {
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	uri := fs.String("uri", "", "promptnet:// uri")
	file := fs.String("file", "-", "template file (- for stdin)")
	dbPath := fs.String("db", "promptnet.db", "sqlite path")
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
	if err := st.Put(context.Background(), store.Prompt{URI: *uri, Template: template, Slots: slots}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stored %s (%s)\n", *uri, store.Hash(template, slots)[:12])
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
