BINARY := nautilm

.PHONY: build run test test-valkey lint clean docker

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

run: build
	./bin/$(BINARY) -config config.yaml

test:
	go test ./...

lint:
	go vet ./...

docker:
	docker build -t $(BINARY):dev .

clean:
	rm -rf bin

# Integration tests against a real Valkey (quotas and usage ledger).
test-valkey:
	VALKEY_URL=$${VALKEY_URL:-redis://localhost:6379} go test -count=1 -run Valkey ./internal/quota ./internal/usage
