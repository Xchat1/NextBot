.PHONY: build test web mac-app

build: web
	go build -trimpath -o bin/nextbot ./cmd/nextbot

web:
	npm --prefix web ci
	npm --prefix web run build

test:
	go test -race ./...

mac-app: web
	mkdir -p bin/NextBot.app/Contents/MacOS bin/NextBot.app/Contents/Resources bin/swift-cache
	go build -trimpath -o bin/NextBot.app/Contents/Resources/nextbot ./cmd/nextbot
	xcrun swiftc -O -parse-as-library -module-cache-path bin/swift-cache -target "$$(uname -m)-apple-macosx13.0" macos/NextBot.swift -o bin/NextBot.app/Contents/MacOS/NextBot
	cp macos/Info.plist bin/NextBot.app/Contents/Info.plist
	codesign --force --sign - bin/NextBot.app/Contents/Resources/nextbot
	codesign --force --sign - bin/NextBot.app
