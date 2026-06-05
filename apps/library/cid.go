package main

// validCID reports whether s is well-formed — only the lowercase base32
// alphabet — so a CID taken from a request path can never be a path-traversal
// payload before it reaches the byte store.
func validCID(s string) bool {
	if len(s) < 2 || len(s) > 80 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
