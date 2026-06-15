.PHONY: gen build test

gen:        ## generate Go + Python stubs from proto (needs `buf`)
	buf generate

build: gen  ## build both binaries (server + authoring CLI)
	go build -o promptnet ./cmd/promptnet
	go build -o promptctl ./cmd/promptctl

test:
	go test ./...
