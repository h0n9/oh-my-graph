BINARY  := oh-my-graph
PORT    := 7780
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run clean

build:
	go build -ldflags="-X main.Version=$(VERSION)" -o $(BINARY) ./cmd/oh-my-graph/

run:
	go run ./cmd/oh-my-graph/ --port $(PORT)

clean:
	rm -f $(BINARY)
