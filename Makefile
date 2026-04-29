PROXY_IMAGE ?= kotakpasir/proxy:dev

.PHONY: proxy-image
proxy-image:
	docker build -f Dockerfile.kpproxy -t $(PROXY_IMAGE) .

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./... -count=1

.PHONY: vet
vet:
	go vet ./...
