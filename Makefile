VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PRELOAD := internal/preloadlib/libwattflame_preload.dylib
PREFIX  ?= /usr/local

.PHONY: build preload test race lint fuzz bench examples demo install clean

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

fuzz:
	go test -run '^$$' -fuzz FuzzName -fuzztime 30s ./internal/demangle

examples:
	$(MAKE) -C examples

# The numbers quoted in the README: attribution against the kernel's own
# per-phase totals, then the profiler's cost on the two example workloads.
bench: build examples
	examples/validate.sh
	./wattflame record -no-html -o /dev/null -top 0 -- examples/bin/pipeline 3 | grep -E 'Energy|Accounted|Profiler'
	./wattflame record -no-html -o /dev/null -top 0 -- examples/bin/cores 4 | grep -E 'Energy|Accounted|Profiler'

demo: build examples
	./wattflame record -o demo.json --open -- examples/bin/cores 4

install: build
	install -d $(PREFIX)/bin
	install -m 0755 wattflame $(PREFIX)/bin/wattflame

clean:
	rm -rf wattflame dist $(PRELOAD) examples/bin *.dSYM
