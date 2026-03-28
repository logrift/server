.PHONY: build

build: clean
	go build -o bin/server cmd/server/main.go

clean:
	rm bin/server

run: build
	bin/server

