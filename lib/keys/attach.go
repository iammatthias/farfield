package keys

import (
	"log/slog"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// Attach wires the shared admin key store into an app's auth gates when the
// KEYS_DB_PATH env var is set; when it is not, the app keeps its env-key-only
// behavior. An open failure logs and degrades the same way — admin keys are
// an additive layer, never an outage. The returned cleanup func is for defer
// and is a no-op when the store is disabled:
//
//	defer keys.Attach(s.auth, "feed")()
func Attach(a *web.Auth, app string) func() {
	path := store.Env("KEYS_DB_PATH", "")
	if path == "" {
		return func() {}
	}
	s, err := Open(path)
	if err != nil {
		slog.Warn("admin-issued keys disabled: could not open key store",
			"path", path, "err", err)
		return func() {}
	}
	a.Keys, a.App = s, app
	return func() { _ = s.Close() }
}
