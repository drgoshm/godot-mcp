BIN := bin/godot-mcp

.PHONY: build test test-godot lint clean

build:
	go build -o $(BIN) ./cmd/godot-mcp

test:
	go test ./...

# Интеграционные тесты с настоящим движком.
test-godot:
	GODOT_BIN=$${GODOT_BIN:-/Applications/Godot.app/Contents/MacOS/Godot} go test -v ./internal/godot/

lint:
	gofmt -l . && go vet ./...

clean:
	rm -rf bin
