# farfield — build, test, and run the fleet locally.
#
#   make build     compile every app into ./bin
#   make test      go test every module in the workspace
#   make dev       rebuild + (re)start the whole fleet on localhost (password: demo)
#   make dev-stop  stop the local fleet
#   make e2e       run the content editor end-to-end suite (needs `make dev` running)
#
# Theme and editor assets are embedded at build time — after editing
# lib/theme/*, a binary must be rebuilt to serve the change. `make dev`
# handles that; it exists so nobody debugs a stale binary again.

APPS := apex backup bard blobs bookmarks content daily dead-presidents feed keys library pulse qr scrap sideload

.PHONY: build test vet dev dev-stop e2e

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
