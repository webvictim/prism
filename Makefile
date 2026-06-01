.PHONY: all prism prism-windows-amd64 prism-linux-amd64 prism-linux-arm64 install uninstall clean

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

all: prism

prism:
	go build -ldflags '$(LDFLAGS)' -o bin/prism ./cmd/prism

prism-windows-amd64:
	GOOS=windows GOARCH=amd64 \
		go build -ldflags '$(LDFLAGS)' -o bin/prism-windows-amd64.exe ./cmd/prism

prism-linux-amd64:
	GOOS=linux GOARCH=amd64 \
		go build -ldflags '$(LDFLAGS)' -o bin/prism-linux-amd64 ./cmd/prism

prism-linux-arm64:
	GOOS=linux GOARCH=arm64 \
		go build -ldflags '$(LDFLAGS)' -o bin/prism-linux-arm64 ./cmd/prism

install: prism
	install -d $(BINDIR)
	install -m 0755 bin/prism $(BINDIR)/prism
	@echo "installed $(BINDIR)/prism"

uninstall:
	rm -f $(BINDIR)/prism
	@echo "removed $(BINDIR)/prism"

clean:
	rm -rf bin/
