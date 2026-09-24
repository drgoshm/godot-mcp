BIN := bin/godot-mcp

.PHONY: build test test-godot lint clean

build:
	go build -o $(BIN) ./cmd/godot-mcp

test:
	go test ./...

# Интеграционные тесты с настоящим движком (все пакеты, включая сквозной MCP-тест).
test-godot:
	GODOT_BIN=$${GODOT_BIN:-/Applications/Godot.app/Contents/MacOS/Godot} go test -v ./...

# gofmt -l всегда завершается с кодом 0, поэтому проверяем, что вывод пуст.
lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...

clean:
	rm -rf bin
