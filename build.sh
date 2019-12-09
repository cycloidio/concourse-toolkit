#!/bin/bash

export PATH=$PATH:$GOPATH/bin

if [ -z "$GOPATH" ]; then
  echo "Ensure GOPATH is set"
  exit 1
fi

# Cleanup
rm -rf $GOPATH/bin

# Static build :
#CGO_ENABLED=0 GOOS=linux go build -o bin/concourse-toolkit main.go

# build
GOFLAGS=-mod=vendor go build -o bin/concourse-toolkit main.go
