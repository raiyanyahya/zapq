VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test bench vet fmt lint fuzz vulncheck cover clean run

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o zapq .

test:
	go test -race -cover ./...

bench:
	go test -bench=. -benchmem -run '^$$' .

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	go run honnef.co/go/tools/cmd/staticcheck@2024.1.1 ./...

fuzz:
	go test -run '^$$' -fuzz '^FuzzParseSize$$' -fuzztime=15s .
	go test -run '^$$' -fuzz '^FuzzSnapshotLoad$$' -fuzztime=15s .
	go test -run '^$$' -fuzz '^FuzzFrames$$' -fuzztime=15s ./internal/frames

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

cover:
	go test -race -coverprofile=coverage.out .
	go tool cover -html=coverage.out -o coverage.html

clean:
	rm -f zapq coverage.out coverage.html

run: build
	./zapq

.PHONY: docker-build docker-run
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t zapq:$(VERSION) .

docker-run:
	docker run --rm -p 8080:8080 zapq:$(VERSION)
