.PHONY: gen build test

gen:        ## generate Go + Python stubs from proto (needs `buf`)
	buf generate

build: gen  ## build the single binary
	go build -o promptnet ./cmd/promptnet

test:
	go test ./...
