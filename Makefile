.PHONY: all build test lint run mock clean

BIN      := bin
GO       ?= go
GOFLAGS  := -trimpath
LDFLAGS  := -s -w

all: lint test build

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/llm-gateway ./cmd/llm-gateway
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/mock-upstream ./cmd/mock-upstream

test:
	$(GO) test -race -count=1 ./...

lint:
	$(GO) vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt: files need formatting:"; echo "$$out"; exit 1; fi

run: build
	./$(BIN)/llm-gateway -config config.json

mock: build
	./$(BIN)/mock-upstream -addr :9001 -name primary -fail 0.5 &
	./$(BIN)/mock-upstream -addr :9002 -name fallback

clean:
	rm -rf $(BIN)
