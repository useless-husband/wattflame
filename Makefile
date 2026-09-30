VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PRELOAD := internal/preloadlib/libwattflame_preload.dylib
PREFIX  ?= /usr/local

.PHONY: build preload test race lint examples demo install clean

build: preload
	go build -trimpath -ldflags "$(LDFLAGS)" -o wattflame ./cmd/wattflame

# The library injected into launched programs. It is embedded into the Go
# binary, so it has to exist before `go build`.
preload: $(PRELOAD)

$(PRELOAD): preload/preload.c
	clang -O2 -Wall -Wextra -arch arm64 -arch arm64e -arch x86_64 -mmacosx-version-min=13.0 -dynamiclib -o $@ $<

test: preload
	go test ./...

race: preload
	go test -race -count=1 ./...

lint: preload
	gofmt -l . | (! grep .) || (echo "gofmt needed on the files above" && exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

examples:
	$(MAKE) -C examples

demo: build examples
	./wattflame record -o demo.json --open -- examples/bin/cores 4

install: build
	install -d $(PREFIX)/bin
	install -m 0755 wattflame $(PREFIX)/bin/wattflame

clean:
	rm -rf wattflame dist $(PRELOAD) examples/bin *.dSYM
