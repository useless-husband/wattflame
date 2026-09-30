// Package demangle turns Rust symbol names into readable paths.
//
// C++ and Swift names arrive already demangled from CoreSymbolication. Rust
// names do not: current compilers emit the "v0" scheme (RFC 2603, symbols
// starting with _R), which the system does not know. Older compilers used a
// scheme that looks like C++ with a hash on the end; the system demangles
// that one as C++, and only the hash and a few escapes need cleaning up.
package demangle

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Name returns the readable form of a symbol name, or the name unchanged if it
// is not a Rust symbol or cannot be parsed.
func Name(s string) string {
	if out, ok := rustV0(s); ok {
		return out
	}
	if out, ok := rustLegacy(s); ok {
		return out
	}
	return s
}

// ---------------------------------------------------------------------------
// Legacy scheme, after the C++ demangler has been over it:
// "core::fmt::num::imp::fmt_u64::h0123456789abcdef".

func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var legacyEscapes = strings.NewReplacer(
	"$SP$", "@", "$BP$", "*", "$RF$", "&", "$LT$", "<", "$GT$", ">",
	"$LP$", "(", "$RP$", ")", "$C$", ",",
	"$u20$", " ", "$u22$", "\"", "$u27$", "'", "$u2b$", "+", "$u3b$", ";",
	"$u5b$", "[", "$u5d$", "]", "$u7b$", "{", "$u7d$", "}", "$u7e$", "~",
	"..", "::",
)

func rustLegacy(s string) (string, bool) {
	i := strings.LastIndex(s, "::h")
	if i < 0 || !isHex16(s[i+3:]) {
		return "", false
	}
	path := s[:i]
	if strings.Contains(path, "$") || strings.Contains(path, "..") {
		// A segment that would start with an escape gets an underscore in
		// front to keep it a valid identifier. Drop it again.
		path = strings.ReplaceAll(path, "::_$", "::$")
		if strings.HasPrefix(path, "_$") {
			path = path[1:]
		}
		path = legacyEscapes.Replace(path)
	}
	return path, true
}

// ---------------------------------------------------------------------------
// v0 scheme.

const maxDepth = 200

type errInvalid struct{}

type parser struct {
	sym   string
	pos   int
	out   strings.Builder
	skip  bool // parse without printing
	depth int
	bound uint64 // lifetimes bound so far, for naming them
}

func (p *parser) fail() { panic(errInvalid{}) }

func (p *parser) peek() byte {
	if p.pos >= len(p.sym) {
		return 0
	}
	return p.sym[p.pos]
}

func (p *parser) next() byte {
	if p.pos >= len(p.sym) {
		p.fail()
	}
	c := p.sym[p.pos]
	p.pos++
	return c
}

func (p *parser) eat(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *parser) print(s string) {
	if !p.skip {
		p.out.WriteString(s)
	}
}

// integer62 reads a base-62 number terminated by "_". The encoding is offset
// by one so that a bare "_" means zero.
func (p *parser) integer62() uint64 {
	if p.eat('_') {
		return 0
	}
	var x uint64
	for !p.eat('_') {
		c := p.next()
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'z':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = uint64(c-'A') + 36
		default:
			p.fail()
		}
		if x > (math.MaxUint64-d)/62 {
			p.fail()
		}
		x = x*62 + d
	}
	if x == math.MaxUint64 {
		p.fail()
	}
	return x + 1
}

func (p *parser) optInteger62(tag byte) uint64 {
	if !p.eat(tag) {
		return 0
	}
	x := p.integer62()
	if x == math.MaxUint64 {
		p.fail()
	}
	return x + 1
}

func (p *parser) disambiguator() uint64 { return p.optInteger62('s') }

// ident reads an identifier and returns it decoded.
func (p *parser) ident() string {
	puny := p.eat('u')
	c := p.peek()
	if c < '0' || c > '9' {
		p.fail()
	}
	n := 0
	if c == '0' {
		p.pos++
	} else {
		for {
			c := p.peek()
			if c < '0' || c > '9' {
				break
			}
			p.pos++
			n = n*10 + int(c-'0')
			if n > len(p.sym) {
				p.fail()
			}
		}
	}
	p.eat('_') // separator, present when the name starts with a digit or "_"
	if p.pos+n > len(p.sym) {
		p.fail()
	}
	raw := p.sym[p.pos : p.pos+n]
	p.pos += n
	if !puny {
		return raw
	}
	ascii, code := "", raw
	if i := strings.LastIndexByte(raw, '_'); i >= 0 {
		ascii, code = raw[:i], raw[i+1:]
	}
	if code == "" {
		p.fail()
	}
	out, ok := punycode(ascii, code)
	if !ok {
		return "punycode{" + raw + "}"
	}
	return out
}

// punycode decodes RFC 3492 with the v0 scheme's digit alphabet (a-z, then
// 0-9).
func punycode(ascii, code string) (string, bool) {
	out := []rune(ascii)
	const base, tMin, tMax, skew, damp = 36, 1, 26, 38, 700
	bias, i, n := 72, 0, 0x80
	pos := 0
	for pos < len(code) {
		oldI, w := i, 1
		for k := base; ; k += base {
			if pos >= len(code) {
				return "", false
			}
			c := code[pos]
			pos++
			var d int
			switch {
			case c >= 'a' && c <= 'z':
				d = int(c - 'a')
			case c >= '0' && c <= '9':
				d = int(c-'0') + 26
			default:
				return "", false
			}
			if d > (1<<30-i)/w {
				return "", false
			}
			i += d * w
			t := k - bias
			if t < tMin {
				t = tMin
			} else if t > tMax {
				t = tMax
			}
			if d < t {
				break
			}
			if w > (1<<30)/(base-t) {
				return "", false
			}
			w *= base - t
		}
		// Adapt the bias.
		delta := i - oldI
		if oldI == 0 {
			delta /= damp
		} else {
			delta /= 2
		}
		count := len(out) + 1
		delta += delta / count
		k := 0
		for delta > ((base-tMin)*tMax)/2 {
			delta /= base - tMin
			k += base
		}
		bias = k + (base-tMin+1)*delta/(delta+skew)

		n += i / count
		i %= count
		if n > utf8.MaxRune || !utf8.ValidRune(rune(n)) {
			return "", false
		}
		out = append(out, 0)
		copy(out[i+1:], out[i:])
		out[i] = rune(n)
		i++
	}
	return string(out), true
}

func (p *parser) enter() {
	p.depth++
	if p.depth > maxDepth {
		p.fail()
	}
}

// backref re-parses an earlier part of the symbol in place of the reference.
func (p *parser) backref(f func()) {
	start := p.pos - 1 // position of the "B"
	target := p.integer62()
	if target >= uint64(start) {
		p.fail()
	}
	saved := p.pos
	p.pos = int(target)
	p.enter()
	f()
	p.depth--
	p.pos = saved
}

func (p *parser) skipping(f func()) {
	saved := p.skip
	p.skip = true
	f()
	p.skip = saved
}

func (p *parser) sepList(f func(), sep string) int {
	n := 0
	for !p.eat('E') {
		if n > 0 {
			p.print(sep)
		}
		f()
		n++
	}
	return n
}

func (p *parser) path(inValue bool) {
	p.enter()
	defer func() { p.depth-- }()
	switch tag := p.next(); tag {
	case 'C':
		p.disambiguator()
		p.print(p.ident())
	case 'N':
		ns := p.next()
		if !(ns >= 'a' && ns <= 'z' || ns >= 'A' && ns <= 'Z') {
			p.fail()
		}
		p.path(inValue)
		dis := p.disambiguator()
		name := p.ident()
		if ns >= 'A' && ns <= 'Z' {
			// Namespaces the compiler gives no name of its own to.
			p.print("::{")
			switch ns {
			case 'C':
				p.print("closure")
			case 'S':
				p.print("shim")
			default:
				p.print(string(ns))
			}
			if name != "" {
				p.print(":")
				p.print(name)
			}
			p.print("#")
			p.print(strconv.FormatUint(dis, 10))
			p.print("}")
		} else if name != "" {
			p.print("::")
			p.print(name)
		}
	case 'M', 'X', 'Y':
		if tag != 'Y' {
			// The impl block's own location says nothing a reader needs.
			p.disambiguator()
			p.skipping(func() { p.path(false) })
		}
		p.print("<")
		p.typ()
		if tag != 'M' {
			p.print(" as ")
			p.path(false)
		}
		p.print(">")
	case 'I':
		p.path(inValue)
		if inValue {
			p.print("::")
		}
		p.print("<")
		p.sepList(p.genericArg, ", ")
		p.print(">")
	case 'B':
		p.backref(func() { p.path(inValue) })
	default:
		p.fail()
	}
}

func (p *parser) genericArg() {
	switch {
	case p.eat('L'):
		p.lifetime(p.integer62())
	case p.eat('K'):
		p.constant()
	default:
		p.typ()
	}
}

// lifetime prints a lifetime given its de Bruijn-style index: 0 is erased,
// others count back from the innermost binder.
func (p *parser) lifetime(index uint64) {
	if index == 0 {
		p.print("'_")
		return
	}
	if index > p.bound {
		p.fail()
	}
	depth := p.bound - index
	if depth < 26 {
		p.print("'" + string(rune('a'+depth)))
	} else {
		p.print("'_" + strconv.FormatUint(depth, 10))
	}
}

// binder handles an optional "for<'a, 'b>" prefix around f.
func (p *parser) binder(f func()) {
	n := p.optInteger62('G')
	if n > uint64(len(p.sym)) {
		p.fail()
	}
	if n > 0 {
		p.print("for<")
		for i := uint64(0); i < n; i++ {
			if i > 0 {
				p.print(", ")
			}
			p.bound++
			p.lifetime(1)
		}
		p.print("> ")
	}
	f()
	p.bound -= n
}

var basicTypes = map[byte]string{
	'a': "i8", 'b': "bool", 'c': "char", 'd': "f64", 'e': "str", 'f': "f32",
	'h': "u8", 'i': "isize", 'j': "usize", 'l': "i32", 'm': "u32", 'n': "i128",
	'o': "u128", 's': "i16", 't': "u16", 'u': "()", 'v': "...", 'x': "i64",
	'y': "u64", 'z': "!", 'p': "_",
}

func (p *parser) typ() {
	p.enter()
	defer func() { p.depth-- }()
	tag := p.next()
	if name, ok := basicTypes[tag]; ok {
		p.print(name)
		return
	}
	switch tag {
	case 'R', 'Q':
		p.print("&")
		if p.eat('L') {
			if lt := p.integer62(); lt != 0 {
				p.lifetime(lt)
				p.print(" ")
			}
		}
		if tag == 'Q' {
			p.print("mut ")
		}
		p.typ()
	case 'P':
		p.print("*const ")
		p.typ()
	case 'O':
		p.print("*mut ")
		p.typ()
	case 'A', 'S':
		p.print("[")
		p.typ()
		if tag == 'A' {
			p.print("; ")
			p.constant()
		}
		p.print("]")
	case 'T':
		p.print("(")
		if p.sepList(p.typ, ", ") == 1 {
			p.print(",")
		}
		p.print(")")
	case 'F':
		p.binder(func() {
			unsafe := p.eat('U')
			abi := ""
			if p.eat('K') {
				if p.eat('C') {
					abi = "C"
				} else {
					abi = strings.ReplaceAll(p.ident(), "_", "-")
					if abi == "" {
						p.fail()
					}
				}
			}
			if unsafe {
				p.print("unsafe ")
			}
			if abi != "" {
				p.print(`extern "` + abi + `" `)
			}
			p.print("fn(")
			p.sepList(p.typ, ", ")
			p.print(")")
			if !p.eat('u') {
				p.print(" -> ")
				p.typ()
			}
		})
	case 'D':
		p.print("dyn ")
		p.binder(func() { p.sepList(p.dynTrait, " + ") })
		if !p.eat('L') {
			p.fail()
		}
		if lt := p.integer62(); lt != 0 {
			p.print(" + ")
			p.lifetime(lt)
		}
	case 'B':
		p.backref(p.typ)
	default:
		// Anything else is a named type: a path.
		p.pos--
		p.path(false)
	}
}

func (p *parser) dynTrait() {
	open := p.pathMaybeOpenGenerics()
	for p.eat('p') {
		if !open {
			p.print("<")
			open = true
		} else {
			p.print(", ")
		}
		p.print(p.ident())
		p.print(" = ")
		p.typ()
	}
	if open {
		p.print(">")
	}
}

// pathMaybeOpenGenerics prints a path and, if it ends in generic arguments,
// leaves the "<...>" list open so associated types can be appended to it.
func (p *parser) pathMaybeOpenGenerics() bool {
	if p.eat('B') {
		var open bool
		p.backref(func() { open = p.pathMaybeOpenGenerics() })
		return open
	}
	if p.eat('I') {
		p.path(false)
		p.print("<")
		p.sepList(p.genericArg, ", ")
		return true
	}
	p.path(false)
	return false
}

func (p *parser) hexNibbles() string {
	start := p.pos
	for {
		c := p.next()
		if c == '_' {
			break
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			p.fail()
		}
	}
	return p.sym[start : p.pos-1]
}

func (p *parser) constant() {
	p.enter()
	defer func() { p.depth-- }()
	switch tag := p.next(); tag {
	case 'p':
		p.print("_")
	case 'h', 't', 'm', 'y', 'o', 'j':
		p.constUint()
	case 'a', 's', 'l', 'x', 'n', 'i':
		if p.eat('n') {
			p.print("-")
		}
		p.constUint()
	case 'b':
		switch p.hexNibbles() {
		case "0":
			p.print("false")
		case "1":
			p.print("true")
		default:
			p.fail()
		}
	case 'c':
		hex := p.hexNibbles()
		v, err := strconv.ParseUint(hex, 16, 32)
		if err != nil || len(hex) > 6 || !utf8.ValidRune(rune(v)) {
			p.fail()
		}
		p.print(quoteChar(rune(v)))
	case 'B':
		p.backref(p.constant)
	default:
		// Structured constants (strings, tuples, ADTs) are rare in
		// function names; give up on the whole symbol rather than guess.
		p.fail()
	}
}

func (p *parser) constUint() {
	hex := strings.TrimLeft(p.hexNibbles(), "0")
	switch {
	case hex == "":
		p.print("0")
	case len(hex) <= 16:
		v, _ := strconv.ParseUint(hex, 16, 64)
		p.print(strconv.FormatUint(v, 10))
	default:
		p.print("0x" + hex)
	}
}

func quoteChar(r rune) string {
	switch r {
	case '\'':
		return `'\''`
	case '\\':
		return `'\\'`
	case '\n':
		return `'\n'`
	case '\r':
		return `'\r'`
	case '\t':
		return `'\t'`
	case 0:
		return `'\0'`
	}
	if r < 0x20 || r == 0x7f {
		return `'\u{` + strconv.FormatUint(uint64(r), 16) + `}'`
	}
	return "'" + string(r) + "'"
}

func rustV0(s string) (out string, ok bool) {
	var body string
	switch {
	case strings.HasPrefix(s, "_R"):
		body = s[2:]
	case strings.HasPrefix(s, "__R"):
		body = s[3:]
	default:
		return "", false
	}
	if body == "" || body[0] < 'A' || body[0] > 'Z' {
		// A digit here would be an encoding version this code predates.
		return "", false
	}
	// LLVM appends ".llvm.<hash>" to symbols it has made module-private.
	suffix := ""
	if i := strings.Index(body, ".llvm."); i >= 0 {
		body = body[:i]
	}
	if i := strings.IndexByte(body, '.'); i >= 0 {
		body, suffix = body[:i], body[i:]
	}
	for i := 0; i < len(body); i++ {
		if body[i] >= 0x80 {
			return "", false
		}
	}

	p := &parser{sym: body}
	defer func() {
		if r := recover(); r != nil {
			if _, invalid := r.(errInvalid); !invalid {
				panic(r)
			}
			out, ok = "", false
		}
	}()
	p.path(true)
	if p.pos < len(p.sym) {
		// The crate that instantiated a generic; not part of the name.
		c := p.peek()
		if c < 'A' || c > 'Z' {
			p.fail()
		}
		p.skipping(func() { p.path(false) })
	}
	if p.pos != len(p.sym) {
		p.fail()
	}
	return p.out.String() + suffix, true
}
