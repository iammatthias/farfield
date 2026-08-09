# farfield — build, test, and run the fleet locally.
#
#   make build     compile every app into ./bin
#   make test      go test every module in the workspace
#   make race      the same tests under the race detector (CI runs this too)
#   make dev       rebuild + (re)start the whole fleet on localhost (password: demo)
#   make dev-stop  stop the local fleet
#   make e2e       run the content editor end-to-end suite (needs `make dev` running)
#
# Theme and editor assets are embedded at build time — after editing
# lib/theme/*, a binary must be rebuilt to serve the change. `make dev`
# handles that; it exists so nobody debugs a stale binary again.

APPS := apex backup bard blobs bookmarks content daily dead-presidents feed keys library pulse qr scrap sideload

.PHONY: build test race vet dev dev-stop e2e

build:
	@mkdir -p bin
	@for app in $(APPS); do \
		echo "build $$app"; \
		(cd apps/$$app && go build -o ../../bin/$$app .) || exit 1; \
	done

test:
	@for d in apps/* lib/*; do \
		[ -f $$d/go.mod ] || continue; \
		(cd $$d && go test ./...) || exit 1; \
	done

# Concurrency bugs in a test's own scaffolding (a stub server tallying hits
# from several handler goroutines) surface only when the runtime happens to
# catch the bad interleaving, which reads as a flaky CI failure. The detector
# makes them deterministic, and the whole suite runs under it in about a
# minute — cheap enough that CI runs it on every push.
race:
	@for d in apps/* lib/*; do \
		[ -f $$d/go.mod ] || continue; \
		(cd $$d && go test -race ./...) || exit 1; \
	done

vet:
	@for d in apps/* lib/*; do \
		[ -f $$d/go.mod ] || continue; \
		(cd $$d && go vet ./...) || exit 1; \
	done

dev: build
	@scripts/devfleet.sh restart

dev-stop:
	@scripts/devfleet.sh stop

e2e:
	@node e2e/content-editor.mjs
