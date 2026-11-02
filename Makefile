APP := ipo-book
BIN := bin/$(APP)
GO ?= go
MYGO := $(GO) tool mygo
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build app dmg install run test test-live test-ui vet fmt clean release

all: app

# 开发用的命令行二进制，跑源码自带的界面，需要从终端启动。
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

run:
	$(GO) run .

# 桌面应用：macOS 上产出可双击的「打新账本.app」，内含 Intel 与 Apple Silicon 两种架构。
app:
	$(MYGO) build -platform darwin/universal -skip-dmg
	@echo "产物：dist/darwin-universal/打新账本.app（在访达里双击即可打开）"

# 带磁盘映像的 macOS 应用，方便拷给别人安装。
dmg:
	$(MYGO) build -platform darwin/universal

# 装进「应用程序」文件夹，之后从启动台、访达或 Dock 打开，不需要终端。
install: app
	@target="/Applications"; \
	if [ ! -w "$$target" ]; then target="$$HOME/Applications"; mkdir -p "$$target"; fi; \
	rm -rf "$$target/打新账本.app"; \
	cp -R "dist/darwin-universal/打新账本.app" "$$target/"; \
	echo "已安装到 $$target/打新账本.app，双击即可打开"

# 单元测试，不访问网络
test:
	$(GO) test ./...

# 联调测试：访问真实的北交所接口，验证主源与备用源
test-live:
	IPO_BOOK_LIVE=1 $(GO) test ./internal/source -run TestLiveFetch -v

# 界面自截：确认打包进二进制的界面能正常渲染，不依赖屏幕录制权限。
# 产物在 /tmp/ui-check.png，after 参数可指定标签页。
test-ui: build
	$(BIN) -data /tmp/ipo-book-uicheck -capture /tmp/ui-check.png $(if $(VIEW),-view $(VIEW),)
	@ls -lh /tmp/ui-check.png

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin dist

# 全平台发布产物。纯 Go、无 cgo，一台机器即可产出全部。
# macOS 得到签名过的 .app 与 .dmg，Windows 得到可执行文件，Linux 得到可执行文件与桌面入口。
release:
	$(MYGO) build -platform darwin/universal,windows/amd64,windows/arm64,linux/amd64,linux/arm64
	@find dist -maxdepth 2 \( -name "*.app" -o -name "*.exe" -o -name "*.dmg" -o -name "*.tar.gz" -o -name "*.deb" \) -print
