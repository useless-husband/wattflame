package demangle

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// testdata/rust_v0.tsv holds symbols taken from real Rust binaries (the
// standard library and a few crates, built with rustc 1.98) plus hand-written
// edge cases, each with the output of the reference implementation, the
// rustc-demangle crate, in its alternate ("{:#}") form.
func TestRustV0MatchesReference(t *testing.T) {
	f, err := os.Open("testdata/rust_v0.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<16)
	n := 0
	for sc.Scan() {
		sym, want, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			t.Fatalf("malformed line %q", sc.Text())
		}
		n++
		if got := Name(sym); got != want {
			t.Errorf("\n sym  %s\n got  %s\n want %s", sym, got, want)
		}
		// Mach-O adds an underscore; both spellings occur in practice.
		if got := Name("_" + sym); got != want {
			t.Errorf("with a leading underscore:\n sym  _%s\n got  %s\n want %s", sym, got, want)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 500 {
		t.Errorf("only %d reference symbols", n)
	}
}

func TestRustV0Rejects(t *testing.T) {
	// Each of these is rejected by the reference implementation too. A name
	// that cannot be parsed must come back unchanged, never half-printed.
	for _, sym := range []string{
		"_R",
		"_RNvC1a",         // identifier runs past the end
		"_RNvB_1a",        // a back reference to itself
		"_RNvC1ab",        // trailing garbage
		"_R1NvC1a1b",      // an encoding version from the future
		"_RNvC1a1b1",      // length without bytes
		"_RNvCs_1a1b\xff", // not ASCII
		"_RNvNvMCs4fqI2P2rA04_13const_genericINtB0_3FooKpE3foo3FOO", // back reference into the middle of a number
		"_RINvNtC3std3mem8align_ofFKC_dE",                           // function type without a return
		"_Rust_begin_unwind",
		"_RNvC0",
	} {
		if got := Name(sym); got != sym {
			t.Errorf("Name(%q) = %q, want it unchanged", sym, got)
		}
	}
	deep := "_R" + strings.Repeat("Nv", 5000) + "C1a" + strings.Repeat("1b", 5000)
	if got := Name(deep); got != deep {
		t.Errorf("deeply nested path was not rejected (%d bytes out)", len(got))
	}
}

func TestRustLegacy(t *testing.T) {
	tests := map[string]string{
		// As they come out of the C++ demangler.
		"core::fmt::num::imp::fmt_u64::h0123456789abcdef":                                            "core::fmt::num::imp::fmt_u64",
		"_$LT$alloc..vec..Vec$LT$T$GT$$u20$as$u20$core..ops..Drop$GT$::drop::hfedcba9876543210":      "<alloc::vec::Vec<T> as core::ops::Drop>::drop",
		"core::ptr::drop_in_place$LT$std..io..Error$GT$::h1111111111111111":                          "core::ptr::drop_in_place<std::io::Error>",
		"std::rt::lang_start::_$u7b$$u7b$closure$u7d$$u7d$::h2222222222222222":                       "std::rt::lang_start::{{closure}}",
		"_$LT$$RF$T$u20$as$u20$core..fmt..Debug$GT$::fmt::h3333333333333333":                         "<&T as core::fmt::Debug>::fmt",
		"_$LT$$LP$A$C$B$RP$$u20$as$u20$core..cmp..PartialEq$GT$::eq::h4444444444444444":              "<(A,B) as core::cmp::PartialEq>::eq",
		"hashbrown::raw::RawTable$LT$T$C$A$GT$::reserve_rehash::h5555555555555555":                   "hashbrown::raw::RawTable<T,A>::reserve_rehash",
		"_$LT$$u5b$T$u5d$$u20$as$u20$core..slice..cmp..SlicePartialEq$GT$::equal::h6666666666666666": "<[T] as core::slice::cmp::SlicePartialEq>::equal",
	}
	for in, want := range tests {
		if got := Name(in); got != want {
			t.Errorf("Name(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestNameLeavesOtherSymbolsAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"main",
		"-[NSObject init]",
		"std::__1::vector<int>::push_back(int const&)",
		"Simulation.step()",
		"runtime.mallocgc",
		"foo::h123",              // hash too short
		"foo::hGGGGGGGGGGGGGGGG", // not hex
		"foo::h0123456789ABCDEF", // rustc writes lower case
		"h0123456789abcdef",      // nothing but a hash-like word
		"DYLD-STUB$$log1p",
		"_platform_memmove",
		"Rfoo",
	} {
		if got := Name(s); got != s {
			t.Errorf("Name(%q) = %q", s, got)
		}
	}
}

func TestPunycode(t *testing.T) {
	tests := []struct{ ascii, code, want string }{
		{"", "9ca", "é"},
		{"__", "7hkackfecea1cbdathfdh9hlq6y", "საჭმელად_გემრიელი_სადილი"},
		{"bcher", "kva", "bücher"},
	}
	for _, tt := range tests {
		got, ok := punycode(tt.ascii, tt.code)
		if !ok || got != tt.want {
			t.Errorf("punycode(%q, %q) = %q, %v; want %q", tt.ascii, tt.code, got, ok, tt.want)
		}
	}
	for _, bad := range []string{"!", "a-", "A", "9", "99999999999999999999"} {
		if got, ok := punycode("", bad); ok {
			t.Errorf("punycode(%q) = %q, want failure", bad, got)
		}
	}
}

// Symbol names come from whatever binary is being profiled. Nothing in one may
// make the demangler crash or hang.
func FuzzName(f *testing.F) {
	for _, seed := range []string{
		"_RNvC6_123foo3bar",
		"_RNCINkXs25_NgCsbmNqQUJIY6D_4core5sliceINyB9_4IterhENuNgNoBb_4iter8iterator8Iterator9rpositionNCNgNpB9_6memchr7memrchrs_0E0Bb_",
		"_RINbNbCskIICzLVDPPb_5alloc5alloc8box_freeDINbNiB4_5boxed5FnBoxuEp6OutputuEL_ECs1iopQbuBiw2_3std",
		"_RINvNtC3std3mem8align_ofFG_RL0_hEuE",
		"_RMCs4fqI2P2rA04_13const_genericINtB0_4CharKc2202_E",
		"_RNqCs4fqI2P2rA04_11utf8_identsu30____7hkackfecea1cbdathfdh9hlq6y",
		"core::fmt::num::imp::fmt_u64::h0123456789abcdef",
		"_RNvB_1a",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Name(s)
		if len(out) > 1<<20 {
			t.Errorf("%d bytes of output from %d bytes of input", len(out), len(s))
		}
	})
}
