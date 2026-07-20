# Native LLM Gateway — Makefile
# 用法:
#   make build       编译后端到 bin/gateway
#   make start       后台启动(写 PID 文件)
#   make stop        优雅停止
#   make restart     stop + start
#   make status      进程 + 端口状态
#   make logs        tail -f 日志
#   make test        跑所有单元测试
#   make vet         go vet
#   make clean       清构建产物 + 临时数据
#   make all         build + test + vet
#   make frontend    构建前端生产版本
#   make frontend-dev 启动 Vite dev server(:5180)

# ---- 配置 ----
PROJECT_ROOT := $(shell pwd)
BACKEND_DIR  := $(PROJECT_ROOT)/backend
FRONTEND_DIR := $(PROJECT_ROOT)/frontend
SCRIPTS_DIR  := $(PROJECT_ROOT)/scripts
BIN_DIR      := $(PROJECT_ROOT)/bin
BIN          := $(BIN_DIR)/gateway
CONFIG       ?= $(PROJECT_ROOT)/config.yaml
LOG          ?= /tmp/gateway.log
PIDFILE      ?= /tmp/gateway.pid
DB_PATH      ?= /tmp/gateway-data/gateway.db
PORT         ?= 8080
CTL          := $(SCRIPTS_DIR)/gateway-ctl.sh

# Go 相关
GO           ?= go
GOFLAGS      := -trimpath
LDFLAGS      := -s -w

# 共享环境
export PORT
export LOG
export PIDFILE

# ---- 目标 ----

.PHONY: help build start stop restart status logs test vet clean all frontend frontend-dev

## help: 默认目标,显示用法
help:
	@echo "Native LLM Gateway"
	@echo ""
	@echo "用法:"
	@echo "  make build       编译后端到 bin/gateway"
	@echo "  make start       后台启动(写 PID 到 $(PIDFILE))"
	@echo "  make stop        优雅停止"
	@echo "  make restart     stop + start"
	@echo "  make status      进程 + 端口 + 健康检查"
	@echo "  make logs        tail -f 日志"
	@echo "  make test        跑所有单元测试"
	@echo "  make vet         go vet"
	@echo "  make clean       清构建产物 + 临时 DB"
	@echo "  make all         build + test + vet"
	@echo "  make frontend    构建前端生产版本"
	@echo "  make frontend-dev 启动 Vite dev server(:5180)"
	@echo ""
	@echo "可覆盖变量:"
	@echo "  CONFIG=$(CONFIG)"
	@echo "  PORT=$(PORT)"

## build: 编译后端
build:
	@mkdir -p $(BIN_DIR)
	cd $(BACKEND_DIR) && $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gateway
	@echo "✓ Built: $(BIN)"
	@ls -lh $(BIN) | awk '{print "  Size:", $$5}'

## start: 后台启动 Gateway
start: build
	@if [ -f $(PIDFILE) ] && kill -0 $$(cat $(PIDFILE)) 2>/dev/null; then \
		echo "✗ Gateway 已在运行(PID $$(cat $(PIDFILE)))。先 make stop"; \
		exit 1; \
	fi
	@if ss -tln 2>/dev/null | grep -q ":$(PORT) "; then \
		echo "✗ 端口 $(PORT) 已被占用。先 make stop 或换 PORT"; \
		ss -tlnp 2>/dev/null | grep ":$(PORT)"; \
		exit 1; \
	fi
	@mkdir -p $(dir $(DB_PATH))
	@if [ ! -f $(CONFIG) ]; then \
		echo "✗ 配置文件不存在: $(CONFIG)"; \
		echo "  复制 example: cp config.example.yaml $(CONFIG)"; \
		exit 1; \
	fi
	@echo "启动 Gateway..."
	@nohup $(BIN) --config $(CONFIG) > $(LOG) 2>&1 & disown 2>/dev/null || true
	@# 等最多 5 秒让端口起来(通过 ss 反查真正 PID,比 $! 可靠)
	@STARTED=0; \
	for i in 1 2 3 4 5; do \
		sleep 1; \
		REAL_PID=$$(ss -tlnp 2>/dev/null | grep ":$(PORT) " | grep -oP 'pid=\K[0-9]+' | head -1); \
		if [ -n "$$REAL_PID" ]; then \
			echo $$REAL_PID > $(PIDFILE); \
			echo "✓ Gateway 已启动 (PID $$REAL_PID)"; \
			echo "  日志: $(LOG)"; \
			echo "  端口: $(PORT)"; \
			STARTED=1; \
			break; \
		fi; \
	done; \
	if [ $$STARTED -ne 1 ]; then \
		echo "✗ 启动失败,端口 $(PORT) 未起来,看日志:"; \
		tail -20 $(LOG); \
		rm -f $(PIDFILE); \
		exit 1; \
	fi

## stop: 优雅停止
stop:
	@$(CTL) stop

## restart: stop + start
restart: stop
	@make start

## status: 进程 + 端口 + 健康检查
status:
	@echo "=== 进程 ==="
	@$(CTL) status
	@echo ""
	@echo "=== 端口 :$(PORT) ==="
	@ss -tln 2>/dev/null | grep ":$(PORT)" || echo "  未监听"
	@echo ""
	@echo "=== 健康检查 ==="
	@curl -s -m 2 http://127.0.0.1:$(PORT)/healthz || echo "  ✗ /healthz 失败"

## logs: tail 实时日志
logs:
	@echo "=== $(LOG) ==="
	@tail -F $(LOG)

## test: 跑单元测试
test:
	cd $(BACKEND_DIR) && $(GO) test ./...

## test-verbose: 详细输出
test-verbose:
	cd $(BACKEND_DIR) && $(GO) test -v ./...

## vet: go vet
vet:
	cd $(BACKEND_DIR) && $(GO) vet ./...

## clean: 清构建产物 + 临时数据
clean:
	@echo "清构建产物..."
	@rm -rf $(BIN_DIR)
	@echo "清临时 DB..."
	@rm -f $(DB_PATH) $(DB_PATH)-journal $(DB_PATH)-shm $(DB_PATH)-wal
	@echo "清 PID 文件..."
	@rm -f $(PIDFILE)
	@echo "✓ 清理完成"

## all: build + test + vet
all: build test vet

## frontend: 构建前端生产版本
frontend:
	cd $(FRONTEND_DIR) && npm install --silent && npm run build
	@echo "✓ Frontend 构建完成: $(FRONTEND_DIR)/dist"

## frontend-dev: 启动 Vite dev server(:5180)
frontend-dev:
	cd $(FRONTEND_DIR) && npm run dev -- --host 0.0.0.0 --port 5180
