GO      ?= go
VERSION ?= 0.4.0
REGISTRY ?= ghcr.io/moods445
IMAGE   ?= $(REGISTRY)/lan-sentinel:$(VERSION)
AGENT_IMAGE ?= $(REGISTRY)/lan-sentinel-agent:$(VERSION)
UNRAID  ?= unraid
UDIR    ?= /mnt/user/appdata/lan-sentinel
LDFLAGS := -s -w -X main.version=$(VERSION) -X github.com/home-aio/lan-sentinel/internal/agent.agentVersion=$(VERSION)

.PHONY: help build server agent cross test vet pull up down logs ps demo-db deploy agent-install clean

help:
	@echo "make build         构建 server + agent（静态、去符号）"
	@echo "make test          运行单元测试"
	@echo "make pull          从 GHCR 拉取镜像 $(IMAGE)"
	@echo "make up            本机 docker compose 起 server:8110（镜像来自 GHCR，不本地构建）"
	@echo "make demo-db       起一个临时 PostgreSQL 16（端口 55432）用于本地联调"
	@echo "make deploy        把 compose 与配置推送到 Unraid，在远端拉 GHCR 镜像并启动"
	@echo "make agent-install 在本机安装 agent（sudo）"
	@echo "make cross         交叉编译 Agent（linux/arm64、darwin、windows）"

build: server agent

server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel ./cmd/sentinel

agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent ./cmd/sentinel-agent

cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-linux-amd64 ./cmd/sentinel
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-linux-arm64 ./cmd/sentinel
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-amd64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-arm64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-armv7 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=386 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-linux-386 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-darwin-amd64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-darwin-arm64 ./cmd/sentinel-agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/sentinel-agent-windows-amd64.exe ./cmd/sentinel-agent

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

pull:
	docker pull $(IMAGE)
	docker pull $(AGENT_IMAGE)

up:
	@test -f .env || cp .env.example .env
	docker compose up -d

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

deploy:
	ssh $(UNRAID) 'mkdir -p $(UDIR)/config $(UDIR)/data $(UDIR)/agent/data && chmod 777 $(UDIR)/agent/data'
	scp docker-compose.yml .env $(UNRAID):$(UDIR)/
	scp -r config/* $(UNRAID):$(UDIR)/config/
	ssh $(UNRAID) 'cd $(UDIR) && SENTINEL_IMAGE=$(IMAGE) SENTINEL_AGENT_IMAGE=$(AGENT_IMAGE) \
		docker compose pull -q && docker compose up -d --force-recreate'
	@echo "面板: http://$(UNRAID):8110"

agent-install: agent
	sudo ./agent/install.sh

clean:
	rm -rf bin
