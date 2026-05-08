VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet clean run

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o zapq .

test:
	go test -race -cover ./...

vet:
	go vet ./...

clean:
	rm -f zapq

run: build
	./zapq

.PHONY: docker-build docker-run

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t zapq:$(VERSION) .

docker-run:
	docker run --rm -p 8080:8080 zapq:$(VERSION)
