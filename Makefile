BINARY := shell-browser

.PHONY: build test vet clean

build:
	go build -o $(BINARY) ./cmd/shell-browser

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
