.PHONY: build clean run fmt vet test check

build:
	go build -o bin/server ./cmd/server

clean:
	rm -f bin/server

run: build
	bin/server

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

check: fmt vet test
