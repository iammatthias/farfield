package main

import "github.com/iammatthias/farfield/lib/cid"

// validCID reports whether s is well-formed — only the lowercase base32
// alphabet — so a CID taken from a request path can never be a path-traversal
// payload before it reaches the byte store.
func validCID(s string) bool { return cid.WellFormed(s) }
