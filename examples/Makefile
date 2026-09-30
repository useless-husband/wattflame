CC     ?= clang
CFLAGS ?= -O1 -g -Wall -Wextra
BIN    := bin/cores bin/pipeline bin/validate

all: $(BIN)

.PHONY: all validate clean

bin/%: %.c
	@mkdir -p bin
	$(CC) $(CFLAGS) -o $@ $<

# Check wattflame's attribution against the kernel's own per-phase numbers.
validate: bin/validate
	./validate.sh

clean:
	rm -rf bin
