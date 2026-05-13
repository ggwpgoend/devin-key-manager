.PHONY: build build-windows run test vet fmt lint clean

BIN := devinmgr
DIST := dist
LDFLAGS := -s -w -X github.com/ggwpgoend/devin-key-manager/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/devinmgr

build-windows:
	mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN).exe ./cmd/devinmgr

run: build
	./$(BIN)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

lint: vet
	@which golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipping"

clean:
	rm -rf $(BIN) $(BIN).exe $(DIST) devinmgr.db devinmgr.db-* .master_key artifacts
