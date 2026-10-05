.PHONY: build clean run fmt fmt-check vet test race check docker

build:
	go build -o bin/server ./cmd/server

clean:
	rm -f bin/server

run: build
	bin/server

fmt:
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt required for:"; echo "$$files"; exit 1; fi

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

check: fmt vet test

docker:
	docker build -t logrift:latest .
