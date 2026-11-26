GO      ?= go
VERSION ?= 0.2.0
IMAGE   ?= lan-sentinel:$(VERSION)
AGENT_IMAGE ?= lan-sentinel-agent:$(VERSION)
UNRAID  ?= unraid
UDIR    ?= /mnt/user/appdata/lan-sentinel
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build server agent cross test vet image up down logs ps demo-db deploy agent-install clean

help:
	@echo "make build         构建 server + agent（静态、去符号）"
	@echo "make test          运行单元测试"
	@echo "make image         构建 docker 镜像 $(IMAGE)"
	@echo "make up            本机 docker compose 起 server:8110"
	@echo "make demo-db       起一个临时 PostgreSQL 16（端口 55432）用于本地联调"
	@echo "make deploy        构建并推送到 Unraid（$(UNRAID):$(UDIR)）"
	@echo "make agent-install 在本机安装 agent（sudo）"
	@echo "make cross         交叉编译 Agent（linux/arm64、darwin、windows）"

build: server agent

server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel ./cmd/sentinel

agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent ./cmd/sentinel-agent

cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-arm64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-armv7 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=386 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-386 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-darwin-amd64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-darwin-arm64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-windows-amd64.exe ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-linux-arm64 ./cmd/sentinel

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

image: server
	docker build -t $(IMAGE) .

agent-image: agent
	docker build -f Dockerfile.agent -t $(AGENT_IMAGE) .

up:
	@test -f .env || cp .env.example .env
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f --tail=100

ps:
	docker compose ps

demo-db:
	docker rm -f sentinel-pg 2>/dev/null || true
	docker run -d --rm --name sentinel-pg \
		-e POSTGRES_USER=sentinel_app -e POSTGRES_PASSWORD=sentinel -e POSTGRES_DB=sentinel \
		-p 55432:5432 postgres:16-alpine

deploy: image agent-image
	docker save $(IMAGE) $(AGENT_IMAGE) | gzip -1 | ssh $(UNRAID) 'gunzip | docker load'
	ssh $(UNRAID) 'mkdir -p $(UDIR)/config $(UDIR)/data'
	scp docker-compose.yml .env $(UNRAID):$(UDIR)/
	scp -r config/* $(UNRAID):$(UDIR)/config/
	ssh $(UNRAID) 'cd $(UDIR) && docker compose up -d --force-recreate'
	@echo "面板: http://$(UNRAID):8110"

agent-install: agent
	sudo ./agent/install.sh

clean:
	rm -rf bin
