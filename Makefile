BINARY := oh-my-graph
PORT   := 7780

.PHONY: build run clean

build:
	go build -o $(BINARY) ./cmd/oh-my-graph/

run:
	go run ./cmd/oh-my-graph/ --port $(PORT)

clean:
	rm -f $(BINARY)
