ARCH ?= $(shell go env GOARCH)
IMAGE ?= sandbox:dev
CLUSTER ?= sandbox-bakeoff

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux-$(ARCH)/sandbox ./cmd/sandbox
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux-$(ARCH)/sandbox-snap ./cmd/sandbox-snap

image: build
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE) .

kind-load: image
	kind load docker-image $(IMAGE) --name $(CLUSTER)

secrets:
	deploy/kind/secrets.sh

deploy: kind-load secrets
	kubectl apply -f deploy/kind/postgres.yaml -f deploy/kind/server.yaml
	kubectl -n sandbox-system rollout restart deploy/sandbox
	kubectl -n sandbox-system rollout status deploy/sandbox --timeout=180s

test:
	go vet ./... && go test ./...

.PHONY: build image kind-load secrets deploy test
