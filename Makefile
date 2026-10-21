BINARY := pedagio

.PHONY: build run test lint clean docker

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
