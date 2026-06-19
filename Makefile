BINARY      := oh-my-graph
INSTALL_DIR := /usr/local/bin
PORT        := 7780
PLIST_LABEL := com.h0n9.oh-my-graph
PLIST_PATH  := $(HOME)/Library/LaunchAgents/$(PLIST_LABEL).plist
LOG_PATH    := $(HOME)/Library/Logs/oh-my-graph.log

.PHONY: build install uninstall start stop restart status logs clean

build:
	go build -o $(BINARY) ./cmd/oh-my-graph/

install: build
	@echo "→ Installing binary to $(INSTALL_DIR)/$(BINARY)"
	install -m 755 $(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "→ Writing plist to $(PLIST_PATH)"
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0">' \
		'<dict>' \
		'	<key>Label</key>' \
		'	<string>$(PLIST_LABEL)</string>' \
		'	<key>ProgramArguments</key>' \
		'	<array>' \
		'		<string>$(INSTALL_DIR)/$(BINARY)</string>' \
		'		<string>--port</string>' \
		'		<string>$(PORT)</string>' \
		'	</array>' \
		'	<key>RunAtLoad</key>' \
		'	<true/>' \
		'	<key>KeepAlive</key>' \
		'	<true/>' \
		'	<key>StandardOutPath</key>' \
		'	<string>$(LOG_PATH)</string>' \
		'	<key>StandardErrorPath</key>' \
		'	<string>$(LOG_PATH)</string>' \
		'</dict>' \
		'</plist>' > $(PLIST_PATH)
	@if launchctl list $(PLIST_LABEL) >/dev/null 2>&1; then \
		echo "→ Service already registered — restarting with new binary"; \
		launchctl kickstart -k gui/$$(id -u)/$(PLIST_LABEL); \
	else \
		echo "→ Registering service with launchd"; \
		launchctl bootstrap gui/$$(id -u) $(PLIST_PATH); \
	fi
	@echo "✓ oh-my-graph running on port $(PORT)"

uninstall:
	@if launchctl list $(PLIST_LABEL) >/dev/null 2>&1; then \
		echo "→ Stopping and unregistering service"; \
		launchctl bootout gui/$$(id -u)/$(PLIST_LABEL) 2>/dev/null || true; \
	fi
	@rm -f $(PLIST_PATH) $(INSTALL_DIR)/$(BINARY)
	@echo "✓ oh-my-graph uninstalled"

start:
	launchctl kickstart gui/$$(id -u)/$(PLIST_LABEL)

stop:
	launchctl kill SIGTERM gui/$$(id -u)/$(PLIST_LABEL)

restart:
	launchctl kickstart -k gui/$$(id -u)/$(PLIST_LABEL)

status:
	@launchctl list $(PLIST_LABEL) 2>/dev/null || echo "Service not registered"

logs:
	tail -f $(LOG_PATH)

clean:
	rm -f $(BINARY)
