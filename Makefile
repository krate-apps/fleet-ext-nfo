.PHONY: build pack
build:
	mkdir -p bin
	go build -trimpath -o bin/nfo-worker ./cmd/nfo-worker

pack:
	bash scripts/pack.sh dist
