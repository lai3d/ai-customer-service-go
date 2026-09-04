package rag

// The tokenizer is a Rust static library. Pointing the linker at it from here means
// `go build ./...` works without every caller remembering to set CGO_LDFLAGS.
// `make deps` puts it in third_party/lib.

// #cgo LDFLAGS: -L${SRCDIR}/../../third_party/lib
import "C"
