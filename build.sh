#!/bin/bash

BINARY_NAME="suws"

#for linux
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o ${BINARY_NAME}-linux-amd64 -v
GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o ${BINARY_NAME}-linux-arm64 -v

#for windows
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o ${BINARY_NAME}-windows-amd64.exe -v

#for mac
GOOS=darwin GOARCH=amd64 go build -ldflags "-s -w" -o ${BINARY_NAME}-darwin-amd64 -v
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w" -o ${BINARY_NAME}-darwin-arm64 -v
