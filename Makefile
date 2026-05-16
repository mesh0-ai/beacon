VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/mesh0-ai/beacon
PLATFORM ?= linux/amd64

.PHONY: build test vet fmt image push clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o beacon .

test:
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

image:
	docker build --platform=$(PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

push: image
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

clean:
	rm -f beacon
