ARCH ?= $(shell go env GOARCH)
IMAGE ?= srvm:dev
CLUSTER ?= srvm-bakeoff

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux-$(ARCH)/srvm ./cmd/srvm
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux-$(ARCH)/srvm-snap ./cmd/srvm-snap

image: build
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE) .

kind-load: image
	kind load docker-image $(IMAGE) --name $(CLUSTER)

secrets:
	deploy/kind/secrets.sh

deploy: kind-load secrets
	kubectl apply -f deploy/kind/postgres.yaml -f deploy/kind/srvm.yaml
	kubectl -n srvm-system rollout restart deploy/srvm
	kubectl -n srvm-system rollout status deploy/srvm --timeout=180s

test:
	go vet ./... && go test ./...

.PHONY: build image kind-load secrets deploy test
