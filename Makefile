.PHONY: all engine wad build server-dev purge-all-games worker web test

WAD_DEB := http://deb.debian.org/debian/pool/non-free/d/doom-wad-shareware/doom-wad-shareware_1.9.fixed-5_all.deb
WAD_MD5 := f0cefca49926d00903cf57551d901abe

all: wad engine build

# DOOM itself, compiled to a WASI reactor module. The same file runs in the
# worker (under wazero) and in the browser.
#
# A game only replays on the exact engine that recorded it (the engine's hash
# is its version), so the build must be reproducible: -buildvcs=false keeps
# git metadata out, otherwise every commit would be a "new engine" and a
# Docker build (with no .git) would differ from a local one.
engine: build/doom.wasm
build/doom.wasm: $(shell find engine third_party/gore -name '*.go' -not -path '*/example/*')
	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -buildvcs=false -o $@ ./engine

# The shareware IWAD, from Debian's doom-wad-shareware package.
wad: wad/doom1.wad
wad/doom1.wad:
	mkdir -p wad build/deb
	curl -fsSL -o build/deb/wad.deb $(WAD_DEB)
	cd build/deb && ar x wad.deb && tar xf data.tar.xz ./usr/share/games/doom/doom1.wad
	mv build/deb/usr/share/games/doom/doom1.wad $@
	rm -rf build/deb
	@test "$$(md5 -q $@ 2>/dev/null || md5sum $@ | cut -d' ' -f1)" = "$(WAD_MD5)" || (echo "unexpected doom1.wad checksum"; rm -f $@; exit 1)

build: engine
	go build -o build/ ./cmd/...

# A local Temporal server, with games persisted in data/temporal.db.
server-dev:
	mkdir -p data
	temporal server start-dev --db-filename data/temporal.db --log-level warn

# purge-all-games terminates every running DoomGame workflow.
#
# Why this exists: a game only replays on the exact engine that recorded it.
# The engine (doom.wasm plus the WAD) is part of the workflow's deterministic
# logic, so when it changes (e.g. bumping the third_party/gore submodule),
# a new worker can't replay the history of a game started by the old one.
# It hits a non-determinism error and the game gets stuck. Terminating every
# game before deploying a new engine avoids that.
#
# This is a poor way to upgrade. Players lose their games, and there is a
# race: a game started after the purge but before the old worker stops
# still breaks. The proper fix is Worker Versioning. Workflows get pinned
# to the Worker Deployment Version that started them, so old games finish
# on old workers while new games start on new ones, with no termination:
#
# https://docs.temporal.io/production-deployment/worker-deployments/worker-versioning
purge-all-games:
	temporal workflow terminate \
		--query "WorkflowType = 'DoomGame' AND ExecutionStatus = 'Running'" \
		--reason "purge-all-games: upgrading the game engine" \
		$(if $(TEMPORAL_TLS_CERT),--tls-cert-path "$(TEMPORAL_TLS_CERT)") \
		$(if $(TEMPORAL_TLS_KEY),--tls-key-path "$(TEMPORAL_TLS_KEY)")

worker: build
	./build/worker

web: build
	./build/server

test: wad engine
	go test ./...
	# gore allows one engine per process, so each test gets its own.
	cd third_party/gore && for t in TestStep TestSaveEveryTic TestStepSound TestStepNet; do DOOM_WAD=$(CURDIR)/wad/doom1.wad go test -count 1 -run "^$$t$$" . || exit 1; done
	cd third_party/gore && rm -rf .default.cfg .extra.cfg .savegame
